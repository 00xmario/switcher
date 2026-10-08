package desktoprelay

import (
	"context"
	"strings"
	"time"
)

// Bind selects an account for one request session. The account is prepared
// first so a broken account is reported here rather than on the next request.
func (m *Manager) Bind(ctx context.Context, scopeID, sessionID, accountID string, expectedRevision uint64) (Session, error) {
	scopeID, sessionID = strings.ToLower(scopeID), strings.ToLower(sessionID)
	if accountID == "" {
		return Session{}, ErrUnavailable
	}
	// With a Switcher host connected the account lives there and is checked
	// when a request uses it.
	if m.remoteInference() == nil {
		if _, err := m.prepare(ctx, accountID); err != nil {
			return Session{}, credentialFailure(err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskKey(scopeID, sessionID)
	s, ok := m.state.Sessions[key]
	if !ok {
		return Session{}, ErrNotFound
	}
	if s.Revision != expectedRevision || m.activeConversationLocked(s) {
		return Session{}, ErrConflict
	}
	updated := s
	updated.AccountID = accountID
	updated.Revision++
	m.state.Sessions[key] = updated
	if err := m.saveLocked(); err != nil {
		m.state.Sessions[key] = s
		return Session{}, err
	}
	return publicSession(updated), nil
}

func (m *Manager) Unbind(scopeID, sessionID string, expectedRevision uint64) (Session, error) {
	scopeID, sessionID = strings.ToLower(scopeID), strings.ToLower(sessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.initializeLocked(false); err != nil {
		return Session{}, err
	}
	key := taskKey(scopeID, sessionID)
	s, ok := m.state.Sessions[key]
	if !ok {
		return Session{}, ErrNotFound
	}
	if s.Revision != expectedRevision || m.activeConversationLocked(s) {
		return Session{}, ErrConflict
	}
	updated := s
	updated.AccountID = ""
	updated.Revision++
	m.state.Sessions[key] = updated
	if err := m.saveLocked(); err != nil {
		m.state.Sessions[key] = s
		return Session{}, err
	}
	return publicSession(updated), nil
}

func (m *Manager) DeleteScope(id string) error {
	id = strings.ToLower(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.initializeLocked(false); err != nil {
		return err
	}
	for _, setup := range m.state.Setups {
		if setup.ScopeID == id && setup.Phase != "restored" {
			return setupError("setup_owned", ErrConflict)
		}
	}
	if _, ok := m.state.Scopes[id]; !ok {
		return ErrNotFound
	}
	delete(m.state.Scopes, id)
	for k, v := range m.state.Sessions {
		if v.ScopeID == id {
			delete(m.state.Sessions, k)
		}
	}
	for k, v := range m.state.ConversationBindings {
		if v.ScopeID == id {
			delete(m.state.ConversationBindings, k)
		}
	}
	return m.saveLocked()
}

// route records the request's session and returns it with the account to use.
// An empty AccountID means the caller's own credential. Routing never fails a
// request: unknown sessions and unavailable metadata keep the caller credential.
func (m *Manager) route(ctx context.Context, scope string, v identityView) Session {
	if v.SessionID == "" {
		return Session{}
	}
	key := taskKey(scope, v.SessionID)
	conversation := m.lookupConversation(ctx, scope, key, v.SessionID)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, exists := m.state.Sessions[key]
	if !exists {
		s = Session{ScopeID: scope, SessionID: v.SessionID, Revision: 1}
	}
	changed := !exists
	if s.ConversationID == "" && conversation != "" {
		s.ConversationID = conversation
		changed = true
	}
	if v.ParentID != "" {
		s.ParentSessionID = v.ParentID
	}
	// A subagent session belongs to its parent's conversation.
	parent := m.state.Sessions[taskKey(scope, s.ParentSessionID)]
	if s.ConversationID == "" && parent.ConversationID != "" {
		s.ConversationID = parent.ConversationID
		changed = true
	}
	if binding := m.state.ConversationBindings[taskKey(scope, s.ConversationID)]; s.ConversationID != "" && binding.AccountID != "" && s.AccountID != binding.AccountID {
		s.AccountID = binding.AccountID
		s.Revision++
		changed = true
	}
	s.LastSeen = time.Now().UTC()
	s.Requests++
	s.InFlight++
	s.Model = v.Model
	if v.AgentID != "" {
		s.AgentID = v.AgentID
	}
	m.state.Sessions[key] = s
	if changed {
		m.saveSoonLocked()
	}
	// A subagent without its own selection follows its parent's exact selection.
	if s.AccountID == "" && parent.AccountID != "" && !m.activeConversationLocked(s) {
		s.AccountID = parent.AccountID
	}
	return s
}

// lookupConversation finds the Desktop conversation of an unlinked session.
// Only a brand-new session in a scope with a selected conversation waits, for
// at most two seconds, because Desktop may write its metadata just after the
// first request. Everything else is linked in the background. Failures are
// ignored: the request keeps the caller's credential.
func (m *Manager) lookupConversation(ctx context.Context, scope, key, id string) string {
	if m.cfg.Conversations == nil {
		return ""
	}
	m.mu.Lock()
	session, exists := m.state.Sessions[key]
	if session.ConversationID != "" {
		m.mu.Unlock()
		return ""
	}
	active := false
	for _, binding := range m.state.ConversationBindings {
		if binding.ScopeID == scope && binding.AccountID != "" {
			active = true
			break
		}
	}
	m.mu.Unlock()
	ids := []string{id}
	if cached, ok := m.cfg.Conversations.(CachedConversationResolver); ok {
		if conversation := cached.ResolveCached(ids)[id]; validUUID(conversation) {
			return conversation
		}
	}
	if exists || !active {
		m.associateSoon()
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		if resolved, err := m.cfg.Conversations.Resolve(ctx, ids); err == nil && validUUID(resolved[id]) {
			return resolved[id]
		}
		select {
		case <-ctx.Done():
			m.associateSoon()
			return ""
		case <-time.After(400 * time.Millisecond):
		}
	}
}

// associateSoon links unlinked sessions in the background, at most every
// fifteen seconds, so one Desktop conversation shows up as one entry.
func (m *Manager) associateSoon() {
	m.mu.Lock()
	due := time.Since(m.associated) > 15*time.Second
	if due {
		m.associated = time.Now()
	}
	m.mu.Unlock()
	if due && m.associating.CompareAndSwap(false, true) {
		go func() {
			defer m.associating.Store(false)
			m.AssociateConversations(context.Background())
		}()
	}
}

// threadOnAccount reports whether a message thread may be sent with the
// session's current account. A continued thread is refused only when the
// session's last thread request used a different account.
func (m *Manager) threadOnAccount(s Session, thread string) bool {
	if thread == "" || s.SessionID == "" {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskKey(s.ScopeID, s.SessionID)
	if previous, seen := m.threads[key]; thread == "continue" && seen && previous != s.AccountID {
		return false
	}
	if m.threads == nil {
		m.threads = make(map[string]string)
	}
	m.threads[key] = s.AccountID
	return true
}

// moveTo puts a session's conversation, or the session alone when its
// conversation is unknown, on account, so its later requests and subagents
// follow. A thread the request was creating now lives on account.
func (m *Manager) moveTo(s Session, account, thread string) error {
	if s.ConversationID != "" {
		if _, err := m.setConversation(s.ScopeID, s.ConversationID, account); err != nil {
			return err
		}
	} else {
		m.mu.Lock()
		key := taskKey(s.ScopeID, s.SessionID)
		current, ok := m.state.Sessions[key]
		if !ok {
			m.mu.Unlock()
			return ErrNotFound
		}
		current.AccountID = account
		current.Revision++
		m.state.Sessions[key] = current
		m.saveSoonLocked()
		m.mu.Unlock()
	}
	if thread == "create" {
		m.mu.Lock()
		if m.threads == nil {
			m.threads = make(map[string]string)
		}
		m.threads[taskKey(s.ScopeID, s.SessionID)] = account
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) finish(s Session) {
	if s.SessionID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskKey(s.ScopeID, s.SessionID)
	if current, ok := m.state.Sessions[key]; ok && current.InFlight > 0 {
		current.InFlight--
		m.state.Sessions[key] = current
	}
}

func (m *Manager) activeConversationLocked(s Session) bool {
	return s.ConversationID != "" && m.state.ConversationBindings[taskKey(s.ScopeID, s.ConversationID)].AccountID != ""
}
