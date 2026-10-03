package desktoprelay

import (
	"context"
	"sort"
	"time"
)

// A network alias has no scope in the source API. Conflicting saved associations
// across scopes must therefore supply no historical hint for that alias.
func (m *Manager) knownConversationAssociations(scope string, ids []string) map[string]string {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	known := make(map[string]string)
	seen := make(map[string]string)
	ambiguous := make(map[string]bool)
	for _, s := range m.state.Sessions {
		if !wanted[s.SessionID] || s.ConversationID == "" || ambiguous[s.SessionID] {
			continue
		}
		if previous, exists := seen[s.SessionID]; exists && previous != s.ConversationID {
			delete(known, s.SessionID)
			ambiguous[s.SessionID] = true
		} else {
			seen[s.SessionID] = s.ConversationID
			if s.ScopeID == scope {
				known[s.SessionID] = s.ConversationID
			}
		}
	}
	return known
}

// VerifiedConversationAssociations returns source-verified current or historical
// proof keyed by scope UUID + "/" + network session UUID. It reads cached core
// observations only, does not initialize or save state, and acquires no credentials.
func (m *Manager) VerifiedConversationAssociations(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m.mu.Lock()
	if m.failed {
		m.mu.Unlock()
		return nil, ErrUnavailable
	}
	snapshot := make(map[string]Session, len(m.state.Sessions))
	scoped := make(map[string][]string)
	for key, s := range m.state.Sessions {
		snapshot[key] = s
		scoped[s.ScopeID] = append(scoped[s.ScopeID], s.SessionID)
	}
	m.mu.Unlock()
	scopes := make([]string, 0, len(scoped))
	for scope := range scoped {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	out := make(map[string]string)
	for _, scope := range scopes {
		ids := scoped[scope]
		sort.Strings(ids)
		resolved, err := m.resolveConversations(ctx, scope, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			conversation := resolved[id]
			key := taskKey(scope, id)
			if conversation != "" && (snapshot[key].ConversationID == "" || snapshot[key].ConversationID == conversation) {
				out[key] = conversation
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.failed {
		return nil, ErrUnavailable
	}
	for key, conversation := range out {
		current, exists := m.state.Sessions[key]
		if !exists || current.ConversationID != "" && current.ConversationID != conversation {
			delete(out, key)
		}
	}
	return out, nil
}
