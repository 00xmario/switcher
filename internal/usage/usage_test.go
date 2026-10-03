package usage

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const codexHeader = `{"type":"session_meta","timestamp":"2026-10-01T10:00:00Z","payload":{"id":"fixture-session"}}` + "\n" +
	`{"type":"turn_context","timestamp":"2026-10-01T10:00:00Z","payload":{"model":"fixture-model"}}` + "\n"

var usageDay = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func codexEvent(second, input, cumulative int) string {
	return fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-10-01T10:00:%02dZ","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"output_tokens":1},"total_token_usage":{"input_tokens":%d,"output_tokens":%d}}}}`+"\n", second, input, cumulative, second)
}

func writeUsageFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep the mtime prefilter independent of the machine's calendar.
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func usageSummary(src Source, cache *ScanCache) *Summary {
	return SummaryFor([]Source{src}, cache, RateTable{"fixture-model": {Input: 1, Output: 1}}, usageDay, usageDay, time.UTC)
}

func TestAppendPreservesHistoricalUsage(t *testing.T) {
	for _, provider := range []string{ProviderCodex, ProviderClaude, ProviderGrok} {
		t.Run(provider, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.jsonl")
			cache := LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))
			src := Source{Provider: provider, Dir: dir}
			var first, second string
			switch provider {
			case ProviderCodex:
				first = codexHeader + codexEvent(2, 10, 10)
				second = codexEvent(3, 20, 30)
			case ProviderClaude:
				first = `{"type":"assistant","timestamp":"2026-10-01T10:00:02Z","sessionId":"fixture","message":{"id":"first","model":"fixture-model","usage":{"input_tokens":10,"output_tokens":1}}}` + "\n"
				second = strings.ReplaceAll(strings.ReplaceAll(first, "first", "second"), `:10,`, `:20,`)
			case ProviderGrok:
				first = `{"timestamp":1790848802000,"params":{"sessionId":"fixture","update":{"sessionUpdate":"turn_completed","prompt_id":"first","usage":{"inputTokens":10,"outputTokens":1}}}}` + "\n"
				second = strings.ReplaceAll(strings.ReplaceAll(first, "first", "second"), `:10,`, `:20,`)
			}
			writeUsageFile(t, path, first)
			if got := usageSummary(src, cache); got.TotalTokens != 11 {
				t.Fatalf("initial tokens=%d, want 11", got.TotalTokens)
			}
			writeUsageFile(t, path, first+second)
			for i := 0; i < 2; i++ {
				if got := usageSummary(src, cache); got.TotalTokens != 32 || got.Records != 2 {
					t.Errorf("scan %d: tokens=%d records=%d, want 32 and 2", i, got.TotalTokens, got.Records)
				}
			}
		})
	}
}

func TestCodexStateSurvivesCacheReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	src := Source{Provider: ProviderCodex, Dir: dir}
	first := codexHeader + codexEvent(2, 10, 10)
	writeUsageFile(t, path, first)
	usageSummary(src, LoadScanCache(cachePath))
	writeUsageFile(t, path, first+codexEvent(3, 20, 30))
	got := usageSummary(src, LoadScanCache(cachePath))
	if got.TotalTokens != 32 || got.Records != 2 || got.Sessions != 1 {
		t.Fatalf("after restart: tokens=%d records=%d sessions=%d, want 32, 2, 1", got.TotalTokens, got.Records, got.Sessions)
	}
}

func TestOldCacheVersionIsRebuilt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	cachePath := filepath.Join(t.TempDir(), "cache.json")
	src := Source{Provider: ProviderCodex, Dir: dir}
	writeUsageFile(t, path, codexHeader+codexEvent(2, 10, 10))
	cache := LoadScanCache(cachePath)
	usageSummary(src, cache)
	cache.Version = 1
	entry := cache.Files[path]
	entry.Records = nil // Version 1 may already have lost accepted usage.
	cache.Files[path] = entry
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := usageSummary(src, LoadScanCache(cachePath)); got.TotalTokens != 11 {
		t.Errorf("rebuilt tokens=%d, want 11", got.TotalTokens)
	}
}

func TestSameSizeRewriteRestartsScan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	cache := LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))
	src := Source{Provider: ProviderCodex, Dir: dir}
	first := codexHeader + codexEvent(2, 10, 10)
	last := codexEvent(3, 20, 30)
	writeUsageFile(t, path, first+last)
	usageSummary(src, cache)
	// Change an earlier line, preserving the file size and resume guard.
	writeUsageFile(t, path, strings.ReplaceAll(first, `"input_tokens":10`, `"input_tokens":11`)+last)
	if got := usageSummary(src, cache); got.TotalTokens != 33 {
		t.Errorf("rewritten tokens=%d, want 33", got.TotalTokens)
	}
}

func TestConcurrentWindowsDoNotShareMutableRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	src := Source{Provider: ProviderCodex, Dir: dir}
	first := codexHeader
	for i := 2; i <= 10; i++ {
		first += codexEvent(i, i, i*10)
	}
	for attempt := 0; attempt < 30; attempt++ {
		cache := LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))
		writeUsageFile(t, path, first)
		usageSummary(src, cache)
		writeUsageFile(t, path, first+codexEvent(11, 11, 121))
		start := make(chan struct{})
		results := make(chan int64, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- usageSummary(src, cache).Records
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for count := range results {
			if count != 10 {
				t.Fatalf("concurrent records=%d, want 10", count)
			}
		}
	}
}

func TestUnterminatedTailIsStableAndResumable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	cache := LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))
	src := Source{Provider: ProviderCodex, Dir: dir}
	body := codexHeader + strings.TrimSuffix(codexEvent(2, 10, 10), "\n")
	writeUsageFile(t, path, body)
	for i := 0; i < 2; i++ {
		if got := usageSummary(src, cache); got.TotalTokens != 11 {
			t.Errorf("unterminated scan %d: tokens=%d, want 11", i, got.TotalTokens)
		}
	}
	writeUsageFile(t, path, body+"\n"+codexEvent(3, 20, 30))
	if got := usageSummary(src, cache); got.TotalTokens != 32 || got.Records != 2 {
		t.Errorf("completed tail: tokens=%d records=%d, want 32 and 2", got.TotalTokens, got.Records)
	}
}

func TestCodexDeduplicatesCumulativeIdentityNotEqualDeltas(t *testing.T) {
	first := codexEvent(2, 10, 10)
	for _, tc := range []struct {
		name   string
		second string
		want   int64
	}{
		{"equal deltas", codexEvent(3, 10, 20), 2},
		{"repeated cumulative usage", strings.ReplaceAll(first, "10:00:02", "10:00:03"), 1},
		{"exact duplicate", first, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), codexHeader+first+tc.second)
			if got := usageSummary(Source{Provider: ProviderCodex, Dir: dir}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))); got.Records != tc.want {
				t.Errorf("records=%d, want %d", got.Records, tc.want)
			}
		})
	}
}

func TestLegacyCodexUsesEventTimeWhenCumulativeUsageIsAbsent(t *testing.T) {
	first := strings.Replace(codexEvent(2, 10, 10), `,"total_token_usage":{"input_tokens":10,"output_tokens":2}`, "", 1)
	second := strings.ReplaceAll(first, "10:00:02", "10:00:03")
	dir := t.TempDir()
	writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), codexHeader+first+first+second)
	if got := usageSummary(Source{Provider: ProviderCodex, Dir: dir}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))); got.Records != 2 {
		t.Errorf("legacy records=%d, want 2", got.Records)
	}
}

func TestNullForkMetadataDoesNotSuppressUsage(t *testing.T) {
	for _, metadata := range []string{`"forked_from_id":null`, `"forked_from_id":""`, `"source":{"subagent":{"thread_spawn":{"parent_thread_id":null}}}`} {
		t.Run(metadata, func(t *testing.T) {
			dir := t.TempDir()
			header := strings.Replace(codexHeader, `"id":"fixture-session"`, `"id":"fixture-session",`+metadata, 1)
			event := strings.ReplaceAll(codexEvent(2, 10, 10), "10:00:02Z", "10:00:00.500Z")
			writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), header+event)
			if got := usageSummary(Source{Provider: ProviderCodex, Dir: dir}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))); got.TotalTokens != 11 {
				t.Errorf("tokens=%d, want 11", got.TotalTokens)
			}
		})
	}
}

func TestRealForkMetadataStillSuppressesCopiedUsage(t *testing.T) {
	dir := t.TempDir()
	header := strings.Replace(codexHeader, `"id":"fixture-session"`, `"id":"fixture-session","forked_from_id":"fixture-parent"`, 1)
	copy := strings.ReplaceAll(codexEvent(2, 10, 10), "10:00:02Z", "10:00:00.500Z")
	writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), header+copy+codexEvent(3, 20, 30))
	if got := usageSummary(Source{Provider: ProviderCodex, Dir: dir}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json"))); got.TotalTokens != 21 || got.Records != 1 {
		t.Errorf("fork tokens=%d records=%d, want 21 and 1", got.TotalTokens, got.Records)
	}
}

func TestGrokRemainingCostIsAllocatedOnlyToUntickedModels(t *testing.T) {
	dir := t.TempDir()
	line := `{"timestamp":1790848800000,"params":{"sessionId":"fixture","update":{"sessionUpdate":"turn_completed","prompt_id":"fixture","usage":{"costUsdTicks":40000000000,"modelUsage":{"priced":{"inputTokens":100,"costUsdTicks":10000000000},"small":{"inputTokens":25},"large":{"inputTokens":75}}}}}}` + "\n"
	writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), line)
	got := usageSummary(Source{Provider: ProviderGrok, Dir: dir}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json")))
	if math.Abs(got.TotalCost-4) > 1e-9 {
		t.Errorf("total cost=%v, want 4", got.TotalCost)
	}
	want := map[string]float64{"priced": 1, "small": 0.75, "large": 2.25}
	for _, m := range got.Models {
		if math.Abs(m.Cost-want[m.Model]) > 1e-9 {
			t.Errorf("%s cost=%v, want %v", m.Model, m.Cost, want[m.Model])
		}
	}
}

type ratesTransport func(*http.Request) (*http.Response, error)

func (f ratesTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureService(t *testing.T, client *http.Client) *Service {
	t.Helper()
	dir := t.TempDir()
	projects := filepath.Join(dir, "claude", "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Dir(projects))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "absent-codex"))
	t.Setenv("GROK_HOME", filepath.Join(dir, "absent-grok"))
	line := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"fixture","message":{"id":"fixture","model":"fixture-model","usage":{"input_tokens":1}}}`+"\n", time.Now().UTC().Format(time.RFC3339Nano))
	writeUsageFile(t, filepath.Join(projects, "fixture.jsonl"), line)
	return NewService(filepath.Join(dir, "data"), client)
}

func TestServiceRefreshesExpiredRates(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: ratesTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"fixture-model":{"input_cost_per_token":2,"output_cost_per_token":0}}`))}, nil
	})}
	s := fixtureService(t, client)
	data, err := json.Marshal(ratesSnapshot{FetchedAtMs: time.Now().Add(-25 * time.Hour).UnixMilli(), Document: RateTable{"fixture-model": {Input: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "model-rates.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// An initialized daemon retains the old table in memory too.
	s.rates = RateTable{"fixture-model": {Input: 1}}
	s.ratesOK = true
	if got := s.Scan(1); calls != 1 || got.TotalCost != 2 {
		t.Errorf("expired rates: fetches=%d cost=%v, want 1 and 2", calls, got.TotalCost)
	}
}

func TestServiceHonorsDiskRateTTLAndKeepsStaleRatesOnFailure(t *testing.T) {
	calls := 0
	fail := false
	client := &http.Client{Transport: ratesTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if fail {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("fixture unavailable"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"fixture-model":{"input_cost_per_token":2,"output_cost_per_token":0}}`))}, nil
	})}
	s := fixtureService(t, client)
	base := time.Now().UTC().Truncate(time.Millisecond)
	now := base
	s.now = func() time.Time { return now }
	data, err := json.Marshal(ratesSnapshot{FetchedAtMs: base.Add(-23 * time.Hour).UnixMilli(), Document: RateTable{"fixture-model": {Input: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "model-rates.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Scan(7); calls != 0 || got.TotalCost != 1 {
		t.Fatalf("fresh disk rates: fetches=%d cost=%v, want 0 and 1", calls, got.TotalCost)
	}
	now = base.Add(59 * time.Minute)
	if got := s.Scan(7); calls != 0 || got.TotalCost != 1 {
		t.Fatalf("unexpired memory rates: fetches=%d cost=%v, want 0 and 1", calls, got.TotalCost)
	}
	now = base.Add(time.Hour)
	if got := s.Scan(7); calls != 1 || got.TotalCost != 2 {
		t.Fatalf("expired disk rates: fetches=%d cost=%v, want 1 and 2", calls, got.TotalCost)
	}
	now = now.Add(24 * time.Hour)
	fail = true
	if got := s.Scan(7); calls != 2 || got.TotalCost != 2 {
		t.Errorf("failed refresh: fetches=%d cost=%v, want 2 and stale cost 2", calls, got.TotalCost)
	}
	fail = false
	if got := s.Scan(7); calls != 3 || got.TotalCost != 2 {
		t.Errorf("refresh retry: fetches=%d cost=%v, want 3 and 2", calls, got.TotalCost)
	}
}

func TestLocalDayBoundaries(t *testing.T) {
	dir := t.TempDir()
	var lines string
	for i, ts := range []string{"2026-09-30T21:59:59Z", "2026-09-30T22:00:00Z", "2026-10-01T21:59:59Z", "2026-10-01T22:00:00Z"} {
		lines += fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":"fixture","message":{"id":%q,"model":"fixture-model","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", ts, fmt.Sprint(i))
	}
	writeUsageFile(t, filepath.Join(dir, "fixture.jsonl"), lines)
	loc := time.FixedZone("UTC+2", 2*3600)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
	got := SummaryFor([]Source{{Provider: ProviderClaude, Dir: dir}}, LoadScanCache(filepath.Join(t.TempDir(), "cache.json")), RateTable{"fixture-model": {Input: 1, Output: 1}}, day, day, loc)
	if got.Records != 2 || got.TotalTokens != 4 || got.TotalCost != 4 {
		t.Errorf("boundary records=%d tokens=%d cost=%v, want 2, 4, 4", got.Records, got.TotalTokens, got.TotalCost)
	}
}
