package desktoprelay

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (m *Manager) ConversationBindings() []ConversationBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ConversationBinding, 0, len(m.state.ConversationBindings))
	for _, b := range m.state.ConversationBindings {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		return taskKey(out[i].ScopeID, out[i].ConversationID) < taskKey(out[j].ScopeID, out[j].ConversationID)
	})
	return out
}

// AssociateConversations links observed request sessions to their Desktop
// conversation using native metadata. Failures leave sessions unlinked.
func (m *Manager) AssociateConversations(ctx context.Context) {
	if m.cfg.Conversations == nil {
		return
	}
	m.mu.Lock()
	scoped := make(map[string][]string)
	for _, s := range m.state.Sessions {
		if s.ConversationID == "" {
			scoped[s.ScopeID] = append(scoped[s.ScopeID], s.SessionID)
		}
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for scope, ids := range scoped {
		resolved, err := m.cfg.Conversations.Resolve(ctx, ids)
		if err != nil {
			continue
		}
		m.mu.Lock()
		changed := false
		for id, conversation := range resolved {
			key := taskKey(scope, id)
			if s, ok := m.state.Sessions[key]; ok && s.ConversationID == "" && validUUID(conversation) {
				s.ConversationID = conversation
				if binding := m.state.ConversationBindings[taskKey(scope, conversation)]; binding.AccountID != "" && s.AccountID != binding.AccountID {
					s.AccountID = binding.AccountID
					s.Revision++
				}
				m.state.Sessions[key] = s
				changed = true
			}
		}
		if changed {
			m.saveSoonLocked()
		}
		m.mu.Unlock()
	}
}

// BindConversation routes every request session of a Desktop conversation,
// including ones it starts later, through the selected account.
func (m *Manager) BindConversation(ctx context.Context, scopeID, conversationID, accountID string) (ConversationBinding, error) {
	scopeID, conversationID = strings.ToLower(scopeID), strings.ToLower(conversationID)
	if accountID == "" || !validUUID(conversationID) {
		return ConversationBinding{}, ErrNotFound
	}
	// With a Switcher host connected the account lives there and is checked
	// when a request uses it.
	if m.remoteInference() == nil {
		if _, err := m.prepare(ctx, accountID); err != nil {
			return ConversationBinding{}, credentialFailure(err)
		}
	}
	m.AssociateConversations(ctx)
	return m.setConversation(scopeID, conversationID, accountID)
}

// UnbindConversation returns a conversation to the caller's own credential.
func (m *Manager) UnbindConversation(ctx context.Context, scopeID, conversationID string) (ConversationBinding, error) {
	scopeID, conversationID = strings.ToLower(scopeID), strings.ToLower(conversationID)
	m.mu.Lock()
	err := m.initializeLocked(false)
	m.mu.Unlock()
	if err != nil {
		return ConversationBinding{}, err
	}
	return m.setConversation(scopeID, conversationID, "")
}

func (m *Manager) setConversation(scopeID, conversationID, accountID string) (ConversationBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state.Scopes[scopeID]; !ok {
		return ConversationBinding{}, ErrNotFound
	}
	key := taskKey(scopeID, conversationID)
	previous, existed := m.state.ConversationBindings[key]
	updated := ConversationBinding{ScopeID: scopeID, ConversationID: conversationID, AccountID: accountID, Revision: previous.Revision + 1}
	oldSessions := make(map[string]Session)
	for k, s := range m.state.Sessions {
		if s.ScopeID == scopeID && s.ConversationID == conversationID && s.AccountID != accountID {
			oldSessions[k] = s
			s.AccountID = accountID
			s.Revision++
			m.state.Sessions[k] = s
		}
	}
	if m.state.ConversationBindings == nil {
		m.state.ConversationBindings = make(map[string]ConversationBinding)
	}
	m.state.ConversationBindings[key] = updated
	if err := m.saveLocked(); err != nil {
		if existed {
			m.state.ConversationBindings[key] = previous
		} else {
			delete(m.state.ConversationBindings, key)
		}
		for k, s := range oldSessions {
			m.state.Sessions[k] = s
		}
		return ConversationBinding{}, err
	}
	return updated, nil
}
