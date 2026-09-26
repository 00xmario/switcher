package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/provider/codex"
	"switcher/internal/store"
)

type probeTransport func(*http.Request) (*http.Response, error)

func (f probeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func probeFixture(t *testing.T) *Manager {
	t.Helper()
	st := store.New(t.TempDir())
	for _, id := range []string{"codex-one", "codex-two"} {
		if err := st.Save(store.Account{ID: id, Provider: "codex", Email: id + "@example.test",
			Token: store.Token{AccessToken: id + "-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(st, map[string]provider.Provider{"codex": codex.New()}, []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("codex-one"); err != nil {
		t.Fatal(err)
	}
	return m
}

func probeResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const completedProbe = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func TestCodexProbeUsesOnePinnedRequestAndNeverSwitches(t *testing.T) {
	m := probeFixture(t)
	var calls atomic.Int32
	m.probeClient = &http.Client{Transport: probeTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.GetBody != nil {
			t.Error("probe request is replayable")
		}
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || r.Method != http.MethodPost ||
			r.Header.Get("Authorization") != "Bearer codex-one-token" || r.Header.Get("originator") != "codex_cli_rs" {
			t.Error("probe did not use the Codex provider's route/auth")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"store":false`) || !strings.Contains(string(body), `"stream":true`) ||
			!strings.Contains(string(body), `"model":"gpt-5.5"`) {
			t.Errorf("probe request shape changed: %s", body)
		}
		return probeResponse(http.StatusOK, completedProbe), nil
	})}
	result := m.ProbeCodex(context.Background())
	if result.Outcome != "success" || result.Fingerprint == "" || result.AccountID != "codex-one" || calls.Load() != 1 || m.ActiveID("codex") != "codex-one" {
		t.Fatalf("probe was not a single pinned request: %+v, calls=%d", result, calls.Load())
	}
	before := result.Fingerprint
	if err := m.Activate("codex-two"); err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("codex-one"); err != nil {
		t.Fatal(err)
	}
	if after, _ := m.ProbeFingerprint(); after == before {
		t.Fatal("A→B→A selection kept old route proof current")
	}
	other := probeFixture(t)
	if after, _ := other.ProbeFingerprint(); after == before {
		t.Fatal("new process reused old proof fingerprint")
	}
}

func TestCodexProbeFailuresDoNotRetryParkOrSwitch(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body, outcome string
	}{
		{"unauthorized", 401, "{}", "upstream_unauthorized"},
		{"rate limit", 429, "{}", "quota_or_rate_limit"},
		{"bad model", 400, "{}", "model_or_request_rejected"},
		{"redirect", 302, "", "upstream_error"},
		{"incomplete SSE", 200, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", "incomplete_response"},
		{"failed SSE", 200, "event: response.failed\ndata: {}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", "incomplete_response"},
		{"truncated", 200, strings.TrimSpace(completedProbe), "incomplete_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := probeFixture(t)
			var calls atomic.Int32
			m.probeClient = &http.Client{Transport: probeTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return probeResponse(tc.status, tc.body), nil
			})}
			got := m.ProbeCodex(context.Background())
			if got.Outcome != tc.outcome || got.Fingerprint != "" || calls.Load() != 1 || m.ActiveID("codex") != "codex-one" {
				t.Fatalf("%s: result=%+v calls=%d", tc.name, got, calls.Load())
			}
			if _, parked := m.Exhausted("codex-one"); parked {
				t.Fatal("probe parked an account")
			}
		})
	}
}

func TestCodexProbeOverlapAndSelectionChange(t *testing.T) {
	m := probeFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	m.probeClient = &http.Client{Transport: probeTransport(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return probeResponse(200, completedProbe), nil
	})}
	done := make(chan ProbeCodexResult, 1)
	go func() { done <- m.ProbeCodex(context.Background()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	if got := m.ProbeCodex(context.Background()).Outcome; got != "busy" {
		t.Fatalf("overlapping probe outcome=%s", got)
	}
	if err := m.Activate("codex-two"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case got := <-done:
		if got.Outcome != "selection_changed" || got.Fingerprint != "" {
			t.Fatalf("selection race was certified: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("probe failed to finish")
	}
}

func TestCodexProbeClientBypassesEnvironmentProxy(t *testing.T) {
	client := directProbeClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || !transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || client.CheckRedirect == nil {
		t.Fatal("probe client may use an environment proxy, redirects, or retry a reused connection")
	}
	if err := client.CheckRedirect(nil, nil); err == nil {
		t.Fatal("probe followed a redirect")
	}
}

func TestCodexProbeLockWaitRespectsDeadline(t *testing.T) {
	m := probeFixture(t)
	lock := m.refreshLock("codex-one")
	lock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	done := make(chan ProbeCodexResult, 1)
	go func() { done <- m.ProbeCodex(ctx) }()
	select {
	case result := <-done:
		lock.Unlock()
		if result.Outcome != "timeout_or_cancelled" {
			t.Fatalf("deadline lock wait result: %+v", result)
		}
	case <-time.After(100 * time.Millisecond):
		lock.Unlock()
		<-done
		t.Fatal("probe blocked on account lock after deadline")
	}
}

func TestCodexProbeCancellationAndCredentialRevision(t *testing.T) {
	m := probeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.probeClient = &http.Client{Transport: probeTransport(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	if got := m.ProbeCodex(ctx).Outcome; got != "timeout_or_cancelled" {
		t.Fatalf("cancelled probe: %s", got)
	}
	before, _ := m.ProbeFingerprint()
	a, err := m.store.Get("codex-one")
	if err != nil {
		t.Fatal(err)
	}
	a.Token.AccessToken = "new-token"
	if err := m.ReplaceAccount(a); err != nil {
		t.Fatal(err)
	}
	if after, _ := m.ProbeFingerprint(); after == before {
		t.Fatal("credential replacement kept old result current")
	}
	if err := m.DeleteAccount("codex-one"); err != nil {
		t.Fatal(err)
	}
	if after, id := m.ProbeFingerprint(); after != "" || id != "" {
		t.Fatal("deleted active account still has a fingerprint")
	}
}

type probeRefreshProvider struct {
	provider.Provider
	refreshes atomic.Int32
}

func (*probeRefreshProvider) ID() string                   { return "codex" }
func (*probeRefreshProvider) IsExpired(store.Account) bool { return true }
func (p *probeRefreshProvider) Refresh(_ context.Context, a *store.Account) error {
	p.refreshes.Add(1)
	a.Token.AccessToken, a.Token.ExpiresAt = "refreshed-token", time.Now().Add(time.Hour).Unix()
	return nil
}
func (*probeRefreshProvider) UpstreamURL(string) string {
	return "https://chatgpt.com/backend-api/codex/responses"
}
func (*probeRefreshProvider) ApplyAuth(r *http.Request, a store.Account) error {
	r.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	return nil
}

func TestCodexProbeRefreshesOnceThenMakesOneInference(t *testing.T) {
	m := probeFixture(t)
	p := &probeRefreshProvider{}
	m.providers["codex"] = p
	a, _ := m.store.Get("codex-one")
	a.Token.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if err := m.store.Save(a); err != nil {
		t.Fatal(err)
	}
	var inferences atomic.Int32
	m.probeClient = &http.Client{Transport: probeTransport(func(r *http.Request) (*http.Response, error) {
		inferences.Add(1)
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Error("stale token used after refresh")
		}
		return probeResponse(200, completedProbe), nil
	})}
	result := m.ProbeCodex(context.Background())
	if result.Outcome != "success" || p.refreshes.Load() != 1 || inferences.Load() != 1 {
		t.Fatalf("refresh/inference count: result=%+v refresh=%d inference=%d", result, p.refreshes.Load(), inferences.Load())
	}
}

func TestCodexProbeTransportErrorDoesNotExposeCredentials(t *testing.T) {
	m := probeFixture(t)
	m.probeClient = &http.Client{Transport: probeTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private-token-in-transport")
	})}
	result := m.ProbeCodex(context.Background())
	if result.Outcome != "transport_error" || strings.Contains(result.Outcome, "private-token") {
		t.Fatalf("transport details escaped: %+v", result)
	}
}
