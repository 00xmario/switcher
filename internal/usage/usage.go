// Package usage reads the provider CLIs' own session logs (Codex, Claude,
// Grok) from disk and aggregates token and cost usage, the way ccusage and
// T3 Code do: no provider API is involved, the logs are the source of truth.
package usage

import "time"

// Totals is a set of token counts. Input counts are "inclusive": the CLI
// reports input_tokens including cached portions, so uncached input is
// derived as input minus cached minus cache-creation.
type Totals struct {
	UncachedInput int64 `json:"uncached_input_tokens"`
	CachedInput   int64 `json:"cached_input_tokens"`
	CacheCreation int64 `json:"cache_creation_tokens"`
	Output        int64 `json:"output_tokens"`
	Reasoning     int64 `json:"reasoning_tokens"`
}

// Processed is the canonical "processed tokens" total: reasoning is a subset
// of output, so it is not added again.
func (t Totals) Processed() int64 {
	return t.UncachedInput + t.CachedInput + t.CacheCreation + t.Output
}

// Record is one deduplicated usage event from a session log.
type Record struct {
	Provider  string // "codex", "claude", "grok"
	Model     string
	SessionID string
	Timestamp time.Time
	Totals    Totals
	Reported  *float64 // provider-reported cost in USD (Grok ticks, Claude costUSD), nil when absent
	DedupeKey string   // empty means the record cannot duplicate across files
	SourceDir string   // provider home directory the record came from (dedupe scope)
}

// Provider identifies which CLI produced a record.
const (
	ProviderCodex  = "codex"
	ProviderClaude = "claude"
	ProviderGrok   = "grok"
)
