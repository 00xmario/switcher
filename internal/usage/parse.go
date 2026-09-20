package usage

import (
	"encoding/json"
	"strings"
	"time"
)

// mightCarryUsage is a cheap substring gate so most lines never reach
// json.Unmarshal.
func mightCarryUsage(provider string, line []byte) bool {
	switch provider {
	case ProviderClaude:
		return strings.Contains(string(line), `"usage"`)
	case ProviderGrok:
		return strings.Contains(string(line), `"turn_completed"`)
	default:
		return strings.Contains(string(line), `"token_count"`) ||
			strings.Contains(string(line), `"turn_context"`) ||
			strings.Contains(string(line), `"session_meta"`)
	}
}

// int64From keeps finite positive values, truncating fractions; everything
// else becomes 0 (matches T3 Code's int() helper).
func int64From(v float64) int64 {
	if v > 0 && v == v && v < 1e15 { // NaN check plus sane bound
		return int64(v)
	}
	return 0
}

// codexScanState carries the stateful bits of a Codex rollout across the
// lines of one file (and across resumes): current model and session, the
// last accepted token-usage signature, and fork-copy suppression.
type codexScanState struct {
	model           string
	sessionID       string
	sawSessionMeta  bool
	lastSig         string
	suppressingFork bool
	forkAnchorMs    int64
}

const forkCopyMaxGapMs = 1000

// parseCodexLine consumes one JSONL line from ~/.codex/sessions/**/*.jsonl.
// Records are deltas (last_token_usage), not totals; duplicate consecutive
// events are skipped; forked rollouts repeat ancestor events within a
// second, which the suppression window drops.
func parseCodexLine(line []byte, st *codexScanState) *Record {
	var root struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &root); err != nil {
		return nil
	}
	tsMs := parseISOMs(root.Timestamp)
	if tsMs < 0 {
		return nil
	}
	switch root.Type {
	case "session_meta":
		if st.sawSessionMeta {
			return nil
		}
		var meta struct {
			ID        string          `json:"id"`
			SessionID string          `json:"session_id"`
			Forked    json.RawMessage `json:"forked_from_id"`
			Source    json.RawMessage `json:"source"`
		}
		// payload.source can be a string ("vscode") or an object; a strict
		// struct would fail the whole decode and lose the session, so the
		// flexible fields are inspected as raw JSON.
		if json.Unmarshal(root.Payload, &meta) != nil {
			return nil
		}
		st.sawSessionMeta = true
		st.sessionID = firstString(meta.ID, meta.SessionID)
		forkCopy := false
		if v, ok := jsonString(meta.Forked); ok {
			_ = v
			forkCopy = true
		}
		if len(meta.Source) > 0 && meta.Source[0] == '{' {
			var src struct {
				Subagent struct {
					ThreadSpawn struct {
						ParentThreadID json.RawMessage `json:"parent_thread_id"`
					} `json:"thread_spawn"`
				} `json:"subagent"`
			}
			if json.Unmarshal(meta.Source, &src) == nil {
				if _, ok := jsonString(src.Subagent.ThreadSpawn.ParentThreadID); ok {
					forkCopy = true
				}
			}
		}
		if forkCopy {
			st.suppressingFork = true
			st.forkAnchorMs = tsMs
		}
		return nil
	case "turn_context":
		var ctx struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(root.Payload, &ctx) == nil && ctx.Model != "" {
			st.model = ctx.Model
		}
		return nil
	case "event_msg":
		var evt struct {
			Type string `json:"type"`
			Info *struct {
				LastTokenUsage *struct {
					Input      float64 `json:"input_tokens"`
					Cached     float64 `json:"cached_input_tokens"`
					CacheWrite float64 `json:"cache_write_input_tokens"`
					Output     float64 `json:"output_tokens"`
					Reasoning  float64 `json:"reasoning_output_tokens"`
				} `json:"last_token_usage"`
			} `json:"info"`
		}
		if json.Unmarshal(root.Payload, &evt) != nil || evt.Type != "token_count" || evt.Info == nil || evt.Info.LastTokenUsage == nil {
			return nil
		}
		u := evt.Info.LastTokenUsage
		if st.model == "" || st.sessionID == "" {
			return nil
		}
		sig := string(mustJSON(u))
		if sig == st.lastSig {
			return nil
		}
		if st.suppressingFork {
			if tsMs-st.forkAnchorMs < forkCopyMaxGapMs {
				st.forkAnchorMs = tsMs
				return nil
			}
			st.suppressingFork = false
		}
		st.lastSig = sig
		input := int64From(u.Input)
		cached := int64From(u.Cached)
		creation := int64From(u.CacheWrite)
		output := int64From(u.Output)
		rec := Record{
			Provider:  ProviderCodex,
			Model:     st.model,
			SessionID: st.sessionID,
			Timestamp: time.UnixMilli(tsMs).UTC(),
			Totals: Totals{
				UncachedInput: maxInt64(0, input-cached-creation),
				CachedInput:   cached,
				CacheCreation: creation,
				Output:        output,
				Reasoning:     minInt64(output, int64From(u.Reasoning)),
			},
		}
		if rec.Totals.Processed() == 0 {
			return nil
		}
		return &rec
	}
	return nil
}

// parseClaudeLine consumes one line of ~/.claude/projects/**/*.jsonl. Every
// assistant content block repeats the same usage object for its message, so
// dedupe on message.id plus requestId is mandatory.
func parseClaudeLine(line []byte) *Record {
	var root struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		SessionID string `json:"sessionId"`
		RequestID any    `json:"requestId"`
		CostUSD   any    `json:"costUSD"`
		Message   *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input         float64 `json:"input_tokens"`
				CacheRead     float64 `json:"cache_read_input_tokens"`
				CacheCreation float64 `json:"cache_creation_input_tokens"`
				Output        float64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &root); err != nil || root.Type != "assistant" ||
		root.Message == nil || root.Message.Usage == nil || root.Message.Model == "" {
		return nil
	}
	tsMs := parseISOMs(root.Timestamp)
	if tsMs < 0 {
		return nil
	}
	u := root.Message.Usage
	rec := Record{
		Provider:  ProviderClaude,
		Model:     root.Message.Model,
		SessionID: root.SessionID,
		Timestamp: time.UnixMilli(tsMs).UTC(),
		Totals: Totals{
			UncachedInput: int64From(u.Input),
			CachedInput:   int64From(u.CacheRead),
			CacheCreation: int64From(u.CacheCreation),
			Output:        int64From(u.Output),
		},
	}
	if cost, ok := root.CostUSD.(float64); ok && cost == cost {
		c := cost
		rec.Reported = &c
	}
	if id, ok := root.RequestID.(string); ok && id != "" {
		rec.DedupeKey = root.Message.ID + ":" + id
	} else if root.Message.ID != "" {
		rec.DedupeKey = root.Message.ID + ":"
	} else {
		rec.DedupeKey = ""
	}
	return &rec
}

// grokCostTicksPerDollar converts Grok's integer cost ticks to USD.
const grokCostTicksPerDollar = 10_000_000_000

// parseGrokLine consumes one line of ~/.grok/sessions/**/updates.jsonl and
// emits one record per model referenced in the turn (usage is per model).
func parseGrokLine(line []byte) []*Record {
	var root struct {
		Timestamp float64 `json:"timestamp"`
		Params    *struct {
			SessionID string `json:"sessionId"`
			Update    *struct {
				SessionUpdate string `json:"sessionUpdate"`
				PromptID      string `json:"prompt_id"`
				Usage         *struct {
					Input       float64 `json:"inputTokens"`
					Output      float64 `json:"outputTokens"`
					CachedRead  float64 `json:"cachedReadTokens"`
					CacheCreate float64 `json:"cacheCreationTokens"`
					Reasoning   float64 `json:"reasoningTokens"`
					CostTicks   float64 `json:"costUsdTicks"`
					ModelUsage  map[string]struct {
						Input       float64 `json:"inputTokens"`
						Output      float64 `json:"outputTokens"`
						CachedRead  float64 `json:"cachedReadTokens"`
						CacheCreate float64 `json:"cacheCreationTokens"`
						Reasoning   float64 `json:"reasoningTokens"`
						CostTicks   float64 `json:"costUsdTicks"`
					} `json:"modelUsage"`
				} `json:"usage"`
			} `json:"update"`
			Meta *struct {
				AgentTimestampMs float64 `json:"agentTimestampMs"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &root); err != nil || root.Params == nil ||
		root.Params.Update == nil || root.Params.Update.SessionUpdate != "turn_completed" ||
		root.Params.Update.Usage == nil {
		return nil
	}
	u := root.Params.Update.Usage
	tsMs := int64From(root.Params.Meta.AgentTimestampMs)
	if tsMs <= 0 {
		tsMs = int64(root.Timestamp) * 1000
		if tsMs <= 1_000_000_000_000 { // seconds, not ms
			tsMs *= 1000
		}
	}
	sessionID := root.Params.SessionID

	// Totals helper for a modelUsage entry.
	mk := func(model string, in, cached, create, out, reason, ticks float64, dedupe string) *Record {
		cachedN := int64From(cached)
		createN := int64From(create)
		rec := &Record{
			Provider:  ProviderGrok,
			Model:     model,
			SessionID: root.Params.SessionID,
			Timestamp: time.UnixMilli(tsMs).UTC(),
			Totals: Totals{
				UncachedInput: maxInt64(0, int64From(in)-cachedN-createN),
				CachedInput:   cachedN,
				CacheCreation: createN,
				Output:        int64From(out),
				Reasoning:     minInt64(int64From(out), int64From(reason)),
			},
		}
		if ticks != 0 {
			c := ticks / grokCostTicksPerDollar
			rec.Reported = &c
		}
		rec.DedupeKey = dedupe
		return rec
	}

	var out []*Record
	if len(u.ModelUsage) > 0 {
		// Cost pro-rated among models without their own tick count, by
		// token share.
		var tickedCost, untickedTokens float64
		for _, m := range u.ModelUsage {
			tickedCost += m.CostTicks
			untickedTokens += m.Input + m.Output
		}
		remaining := maxFloat64(0, u.CostTicks-tickedCost)
		for model, m := range u.ModelUsage {
			dedupe := ""
			if root.Params.Update.PromptID != "" && sessionID != "" {
				dedupe = sessionID + ":" + root.Params.Update.PromptID + ":" + model
			}
			ticks := m.CostTicks
			if ticks == 0 && untickedTokens > 0 && remaining > 0 {
				ticks = remaining * ((m.Input + m.Output) / untickedTokens)
			}
			if r := mk(model, m.Input, m.CachedRead, m.CacheCreate, m.Output, m.Reasoning, ticks, dedupe); r.Totals.Processed() > 0 {
				out = append(out, r)
			}
		}
		return out
	}
	dedupe := ""
	if root.Params.Update.PromptID != "" {
		dedupe = sessionID + ":" + root.Params.Update.PromptID + ":grok"
	}
	if r := mk("grok", u.Input, u.CachedRead, u.CacheCreate, u.Output, u.Reasoning, u.CostTicks, dedupe); r != nil && r.Totals.Processed() > 0 {
		return []*Record{r}
	}
	return nil
}

// jsonString reports whether raw is a JSON string and returns its value.
func jsonString(raw []byte) (string, bool) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}

func parseISOMs(s string) int64 {
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s)); err == nil {
		return t.UnixMilli()
	}
	// Tolerate missing fractional seconds and other RFC3339 variants.
	for _, layout := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UnixMilli()
		}
	}
	return -1
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func firstString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// mustJSON marshals v with nil on failure (only called on decodable values).
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
