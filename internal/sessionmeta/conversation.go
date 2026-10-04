package sessionmeta

import (
	"context"
	"encoding/json"
	"strings"
)

// Resolve maps request session UUIDs to the Desktop conversation UUID whose
// native metadata currently names them as its cliSessionId. Unknown or
// ambiguous sessions are absent from the result.
func (i *Index) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	_, conversations, err := i.lookup(ctx, ids, true)
	return conversations, err
}

// membership collects cliSessionId -> Desktop conversation claims.
type membership struct {
	aliases map[string]map[string]bool
}

func newMembership() *membership {
	return &membership{aliases: make(map[string]map[string]bool)}
}

func desktopID(id string) string {
	if !strings.HasPrefix(id, "local_") {
		return ""
	}
	return uuid(strings.TrimPrefix(id, "local_"))
}

func (m *membership) add(data []byte, alias string) {
	var record struct {
		SessionID string `json:"sessionId"`
	}
	if alias == "" || json.Unmarshal(data, &record) != nil {
		return
	}
	if stable := desktopID(record.SessionID); stable != "" {
		if m.aliases[alias] == nil {
			m.aliases[alias] = make(map[string]bool)
		}
		m.aliases[alias][stable] = true
	}
}

func (m *membership) resolve() map[string]string {
	out := make(map[string]string)
	for alias, groups := range m.aliases {
		if len(groups) != 1 {
			continue
		}
		for id := range groups {
			out[alias] = id
		}
	}
	return out
}
