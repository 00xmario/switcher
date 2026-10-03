package sessionmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Resolve implements a trusted native conversation resolver without importing
// the relay. Values are canonical Desktop UUIDs; admission scopes belong to the
// caller. Titles, models, CLI indexes and parent identifiers are never links.
// Missing or ambiguous metadata is absence. Cancellation returns an error and
// no membership. Both Resolve and Lookup share one bounded, serialized cache.
func (i *Index) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	_, conversations, err := i.lookup(ctx, ids, true, nil)
	return conversations, err
}

// ResolveHistorical accepts only prior verified alias-to-native-UUID mappings
// supplied by the core, never browser/body fields or inferred title matches.
// Admission scopes and proof persistence belong to that caller. Current native
// claims take precedence. An absent alias may retain its prior association only
// while a complete current scan verifies the immutable native UUID's group.
// This does not compare historical creation/cwd fingerprints or publish history
// into Lookup/Resolve's current metadata. Known current mappings can be cached
// for three seconds; a previously unseen unmapped alias forces one scan per TTL.
// Negative attempts are bounded to 4096 UUIDs; further misses wait for expiry. An
// opaque/unreadable metadata record disables historical fallback because alias
// absence cannot be verified, while current validated claims remain usable.
func (i *Index) ResolveHistorical(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
	_, conversations, err := i.lookup(ctx, ids, true, known)
	return conversations, err
}

func canonicalKnown(ctx context.Context, known map[string]string) (map[string]string, map[string]bool) {
	if len(known) == 0 {
		return nil, nil
	}
	prior, disputed := make(map[string]string), make(map[string]bool)
	for original, value := range known {
		if ctx.Err() != nil {
			return nil, nil
		}
		alias, group := uuid(original), uuid(value)
		if alias == "" {
			continue
		}
		if group == "" || prior[alias] != "" && prior[alias] != group {
			disputed[alias] = true
		}
		prior[alias] = group
	}
	return prior, disputed
}

type conversationGroup struct {
	created int64
	cwd     string
	bridges []map[string]bool
}

type membership struct {
	ctx             context.Context
	groups          map[string]*conversationGroup
	aliases         map[string]map[string]bool
	invalidGroups   map[string]bool
	invalidAlias    map[string]bool
	claimed         map[string]bool
	historyReadable bool
}

func newMembership(ctx context.Context) *membership {
	return &membership{ctx: ctx, groups: make(map[string]*conversationGroup), aliases: make(map[string]map[string]bool),
		invalidGroups: make(map[string]bool), invalidAlias: make(map[string]bool),
		claimed: make(map[string]bool), historyReadable: true}
}

func desktopID(id string) string {
	if !strings.HasPrefix(id, "local_") {
		return ""
	}
	return uuid(strings.TrimPrefix(id, "local_"))
}

// A filename can veto a malformed native record, never establish membership.
func (m *membership) reject(name string) {
	if strings.HasSuffix(name, ".json") {
		if id := desktopID(strings.TrimSuffix(name, ".json")); id != "" {
			m.invalidGroups[id] = true
		}
	}
}

func (m *membership) add(name string, data []byte, alias string) {
	var record struct {
		SessionID            string          `json:"sessionId"`
		Created              json.RawMessage `json:"createdAt"`
		Cwd                  string          `json:"cwd"`
		Bridges              json.RawMessage `json:"bridgeSessionIds"`
		ParentSessionID      json.RawMessage `json:"parentSessionId"`
		ParentConversationID json.RawMessage `json:"parentConversationId"`
		ParentID             json.RawMessage `json:"parentId"`
		Parent               json.RawMessage `json:"parent"`
		ForkedFromSessionID  json.RawMessage `json:"forkedFromSessionId"`
		ForkedFrom           json.RawMessage `json:"forkedFrom"`
		IsFork               json.RawMessage `json:"isFork"`
		IsChild              json.RawMessage `json:"isChild"`
		AgentID              json.RawMessage `json:"agentId"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &record) != nil {
		m.invalidAlias[alias] = true
		m.reject(name)
		return
	}
	stable := desktopID(record.SessionID)
	if stable == "" {
		m.invalidAlias[alias] = true
		m.reject(name)
		return
	}
	if m.aliases[alias] == nil {
		m.aliases[alias] = make(map[string]bool)
	}
	m.aliases[alias][stable] = true
	var created int64
	valid := alias != "" && name == record.SessionID+".json" &&
		json.Unmarshal(record.Created, &created) == nil && created > 0 && created <= 253402300799999 && canonicalCwd(record.Cwd)
	for _, lineage := range []json.RawMessage{record.ParentSessionID, record.ParentConversationID, record.ParentID, record.Parent,
		record.ForkedFromSessionID, record.ForkedFrom, record.AgentID} {
		if lineagePresent(lineage, false) {
			valid = false
		}
	}
	for _, flag := range []json.RawMessage{record.IsFork, record.IsChild} {
		if lineagePresent(flag, true) {
			valid = false
		}
	}
	bridges, ok := bridgeSet(record.Bridges)
	if !ok {
		valid = false
	}
	group := m.groups[stable]
	if group == nil {
		group = &conversationGroup{created: created, cwd: record.Cwd}
		m.groups[stable] = group
	} else if group.created != created || group.cwd != record.Cwd {
		valid = false
	}
	if !valid {
		m.invalidGroups[stable] = true
		m.reject(name)
		return
	}
	group.bridges = append(group.bridges, bridges)
}

func canonicalCwd(cwd string) bool {
	return len(cwd) <= 4096 && filepath.IsAbs(cwd) && filepath.Clean(cwd) == cwd && strings.IndexFunc(cwd, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) < 0
}

func lineagePresent(raw json.RawMessage, flag bool) bool {
	value := string(bytes.TrimSpace(raw))
	switch value {
	case "", "null":
		return false
	}
	return !(flag && value == "false" || !flag && value == `""`)
}

func bridgeSet(raw json.RawMessage) (map[string]bool, bool) {
	set := make(map[string]bool)
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return set, true
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil || len(ids) > maxEntries {
		return nil, false
	}
	for _, id := range ids {
		if id == "" || len(id) > 256 || strings.TrimSpace(id) != id || strings.IndexFunc(id, func(r rune) bool {
			return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
		}) >= 0 {
			return nil, false
		}
		set[id] = true
	}
	return set, true
}

func (m *membership) resolve() map[string]string {
	out := make(map[string]string)
	for alias, groups := range m.aliases {
		if alias == "" || m.invalidAlias[alias] || len(groups) != 1 {
			for id := range groups {
				m.invalidGroups[id] = true
			}
		}
	}
	for id, group := range m.groups {
		if !bridgesConnected(m.ctx, group.bridges) {
			m.invalidGroups[id] = true
		}
	}
	for alias, groups := range m.aliases {
		for id := range groups {
			if !m.invalidGroups[id] {
				out[alias] = id
			}
		}
	}
	return out
}

// Nonempty bridge sets must form one connected component. Empty sets do not
// connect disjoint bridge histories. Without bridges, the exact native UUID,
// filename, creation and full cwd remain the required identity evidence.
func bridgesConnected(ctx context.Context, sets []map[string]bool) bool {
	parents := make([]int, len(sets))
	for j := range parents {
		parents[j] = j
	}
	root := func(j int) int {
		for parents[j] != j {
			parents[j] = parents[parents[j]]
			j = parents[j]
		}
		return j
	}
	owners := make(map[string]int)
	first := -1
	for j, set := range sets {
		if ctx.Err() != nil {
			return false
		}
		if len(set) == 0 {
			continue
		}
		if first < 0 {
			first = j
		}
		for id := range set {
			if owner, ok := owners[id]; ok {
				parents[root(j)] = root(owner)
			} else {
				owners[id] = j
			}
		}
	}
	for j, set := range sets {
		if len(set) != 0 && root(j) != root(first) {
			return false
		}
	}
	return true
}
