package usage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RatesURL is LiteLLM's public per-model price table (USD per token).
const RatesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

const ratesTTL = 24 * time.Hour

// Rate is USD per token for one model.
type Rate struct {
	Input         float64 `json:"input"`
	CacheRead     float64 `json:"cache_read"`
	CacheCreation float64 `json:"cache_creation"`
	Output        float64 `json:"output"`
}

// RateTable maps lowercase model names to rates.
type RateTable map[string]Rate

// unpriceable names have no meaningful per-token price (internal markers or
// bare aliases that would price the wrong model).
var unpriceable = map[string]bool{
	"<synthetic>": true, "synthetic": true, "opus": true, "sonnet": true, "haiku": true, "fable": true,
}

type ratesSnapshot struct {
	FetchedAtMs int64     `json:"fetched_at_ms"`
	Document    RateTable `json:"document"`
}

// LoadRates serves the table from disk when fresh, fetching LiteLLM
// otherwise; on fetch failure the stale table still answers. The second
// return value is "fresh", "cached", or "unavailable".
func LoadRates(path string, client *http.Client) (RateTable, string) {
	var snap ratesSnapshot
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &snap) == nil &&
		snap.Document != nil && time.Since(time.UnixMilli(snap.FetchedAtMs)) < ratesTTL {
		return snap.Document, "cached"
	}
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, RatesURL, nil)
	if err != nil {
		return snap.Document, "unavailable"
	}
	resp, err := client.Do(req)
	if err != nil {
		return snap.Document, "unavailable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return snap.Document, "unavailable"
	}
	var raw map[string]rawRate
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&raw); err != nil {
		return snap.Document, "unavailable"
	}
	table := buildTable(raw)
	if len(table) == 0 {
		return snap.Document, "unavailable"
	}
	out := ratesSnapshot{FetchedAtMs: time.Now().UnixMilli(), Document: table}
	if data, err := json.Marshal(out); err == nil {
		_ = os.WriteFile(path, data, 0o644)
	}
	return table, "fresh"
}

type rawRate struct {
	InputCost       any `json:"input_cost_per_token"`
	OutputCost      any `json:"output_cost_per_token"`
	CacheReadCost   any `json:"cache_read_input_token_cost"`
	CacheCreateCost any `json:"cache_creation_input_token_cost"`
}

// buildTable normalises keys (lowercase), defaults cache rates to the input
// rate, ignores tiered/variant entries, and adds a bare-name alias for
// vendor-prefixed keys when every qualified entry with that bare name
// agrees on rates.
func buildTable(raw map[string]rawRate) RateTable {
	table := make(RateTable, len(raw))
	for name, r := range raw {
		in, okIn := number(r.InputCost)
		out, okOut := number(r.OutputCost)
		if !okIn || !okOut {
			continue
		}
		read := in
		if v, ok := number(r.CacheReadCost); ok {
			read = v
		}
		create := in
		if v, ok := number(r.CacheCreateCost); ok {
			create = v
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" || strings.ContainsAny(key, "[") || strings.Contains(key, "above_") {
			continue
		}
		table[key] = Rate{Input: in, CacheRead: read, CacheCreation: create, Output: out}
	}
	// Bare-name aliasing: vendor/model -> model when unambiguous.
	type candidate struct {
		name string
		rate Rate
	}
	var candidates []candidate
	for key, rate := range table {
		if i := strings.LastIndex(key, "/"); i >= 0 {
			bare := key[i+1:]
			if _, exists := table[bare]; !exists {
				candidates = append(candidates, candidate{bare, rate})
			}
		}
	}
	agree := map[string][]Rate{}
	for _, c := range candidates {
		agree[c.name] = append(agree[c.name], c.rate)
	}
	for bare, rates := range agree {
		if unpriceable[bare] || len(rates) == 0 {
			continue
		}
		first := rates[0]
		same := true
		for _, r := range rates[1:] {
			if r != first {
				same = false
				break
			}
		}
		if same {
			table[bare] = first
		}
	}
	return table
}

func number(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

// LookupRate finds the rate for a model: exact lowercase key only, with
// "[1m]" style suffixes stripped and unpriceable bare names rejected.
func LookupRate(table RateTable, model string) (Rate, bool) {
	key := model
	if i := strings.Index(key, "["); i >= 0 {
		key = key[:i]
	}
	key = strings.ToLower(strings.TrimSpace(key))
	bare := key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		bare = key[i+1:]
	}
	if bare == "" || unpriceable[bare] {
		return Rate{}, false
	}
	r, ok := table[key]
	return r, ok
}

// Price computes the USD cost of one record: provider-reported cost wins,
// then the rate table, else the record is unpriced.
type CostSource string

const (
	CostModelPriced      CostSource = "model_priced"
	CostProviderReported CostSource = "provider_reported"
	CostUnpriced         CostSource = "unpriced"
)

func Price(table RateTable, r Record) (usd float64, source CostSource) {
	if r.Reported != nil {
		return *r.Reported, CostProviderReported
	}
	rate, ok := LookupRate(table, r.Model)
	if !ok {
		return 0, CostUnpriced
	}
	t := r.Totals
	cost := float64(t.UncachedInput)*rate.Input +
		float64(t.CachedInput)*rate.CacheRead +
		float64(t.CacheCreation)*rate.CacheCreation +
		float64(t.Output)*rate.Output
	return cost, CostModelPriced
}

// CacheSavings is what the cached input saved versus full input price.
func CacheSavings(table RateTable, r Record) float64 {
	rate, ok := LookupRate(table, r.Model)
	if !ok {
		return 0
	}
	return float64(r.Totals.CachedInput) * (rate.Input - rate.CacheRead)
}
