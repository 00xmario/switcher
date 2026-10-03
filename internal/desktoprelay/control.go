package desktoprelay

import (
	"context"
	"strings"
	"time"
)

func usable(c Credential, account string) bool {
	return c.AccountID == account && c.AccessToken != "" && len(c.AccessToken) <= 16384 && !strings.ContainsAny(c.AccessToken, " \r\n\t")
}

func (m *Manager) Bind(ctx context.Context, scopeID, sessionID, accountID string, expectedRevision uint64) (Session, error) {
	scopeID, sessionID = strings.ToLower(scopeID), strings.ToLower(sessionID)
	key := taskKey(scopeID, sessionID)
	m.mu.Lock()
	s, ok := m.state.Sessions[key]
	if !ok {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if s.Revision != expectedRevision {
		m.mu.Unlock()
		return Session{}, ErrConflict
	}
	if m.activeConversationLocked(s) {
		m.mu.Unlock()
		return Session{}, ErrConflict
	}
	if m.run == nil || m.run.ctx.Err() != nil || m.failed || !m.state.Enabled || m.cfg.Source == nil || accountID == "" || len(accountID) > 256 {
		m.mu.Unlock()
		return Session{}, ErrUnavailable
	}
	runCtx := m.run.ctx
	recordedConversationKey := taskKey(scopeID, s.ConversationID)
	recordedConversationRevision := m.state.ConversationBindings[recordedConversationKey].Revision
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	resolved, err := m.resolveConversations(ctx, scopeID, []string{sessionID})
	if err != nil {
		return Session{}, err
	}
	if m.cfg.Conversations != nil {
		if err := verifiedAssociation(s, resolved[sessionID]); err != nil {
			return Session{}, err
		}
	}
	m.mu.Lock()
	if m.state.ConversationBindings[recordedConversationKey].Revision != recordedConversationRevision {
		m.mu.Unlock()
		return Session{}, ErrConflict
	}
	conversationID := resolved[sessionID]
	if conversationID == "" {
		conversationID = s.ConversationID
	}
	conversationKey := taskKey(scopeID, conversationID)
	conversationBinding := m.state.ConversationBindings[conversationKey]
	active := conversationBinding.AccountID != ""
	m.mu.Unlock()
	if active {
		return Session{}, ErrConflict
	}
	c, err := m.cfg.Source.Prepare(ctx, accountID)
	if err != nil || !usable(c, accountID) {
		return Session{}, credentialFailure(err)
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	checked, err := m.resolveConversations(ctx, scopeID, []string{sessionID})
	if err != nil {
		return Session{}, err
	}
	if checked[sessionID] != resolved[sessionID] {
		return Session{}, ErrConflict
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.state.Sessions[key]
	if !ok {
		return Session{}, ErrNotFound
	}
	if current.Revision != expectedRevision {
		return Session{}, ErrConflict
	}
	// Empty-account records are reset epochs too. A reset must fence an older
	// exact selection even when clearing an already-default member changed no
	// member revision and the conversation is still inactive.
	if m.state.ConversationBindings[recordedConversationKey].Revision != recordedConversationRevision || m.state.ConversationBindings[conversationKey].Revision != conversationBinding.Revision {
		return Session{}, ErrConflict
	}
	if m.activeConversationLocked(current) || m.state.ConversationBindings[taskKey(scopeID, checked[sessionID])].AccountID != "" {
		return Session{}, ErrConflict
	}
	if m.run == nil || m.run.ctx != runCtx || m.run.ctx.Err() != nil || m.failed || !m.state.Enabled {
		return Session{}, ErrUnavailable
	}
	if current.Revision == ^uint64(0) {
		return Session{}, ErrBusy
	}
	updated := current
	updated.AccountID = accountID
	updated.Revision++
	// Waiting for the final lock can outlive management cancellation even
	// after preparation succeeded. Check at the mutation's commit boundary.
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	m.state.Sessions[key] = updated
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Sessions[key] = current
		}
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
	if m.store == nil || m.failed {
		return Session{}, ErrUnavailable
	}
	if s.Revision != expectedRevision {
		return Session{}, ErrConflict
	}
	if m.activeConversationLocked(s) {
		return Session{}, ErrConflict
	}
	if s.Revision == ^uint64(0) {
		return Session{}, ErrBusy
	}
	updated := s
	updated.AccountID = ""
	updated.Revision++
	m.state.Sessions[key] = updated
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Sessions[key] = s
		}
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
	s, ok := m.state.Scopes[id]
	if !ok {
		return ErrNotFound
	}
	if m.store == nil || m.failed {
		return ErrUnavailable
	}
	removed := make(map[string]Session)
	removedBindings := make(map[string]ConversationBinding)
	delete(m.state.Scopes, id)
	for k, v := range m.state.Sessions {
		if v.ScopeID == id {
			removed[k] = v
			delete(m.state.Sessions, k)
		}
	}
	for k, v := range m.state.ConversationBindings {
		if v.ScopeID == id {
			removedBindings[k] = v
			delete(m.state.ConversationBindings, k)
		}
	}
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Scopes[id] = s
			for k, v := range removed {
				m.state.Sessions[k] = v
			}
			for k, v := range removedBindings {
				m.state.ConversationBindings[k] = v
			}
		}
		return err
	}
	// Opaque tunnels have no visible HTTP admission boundary to recheck.
	if m.run != nil {
		m.run.mu.Lock()
		for c, record := range m.run.conns {
			if record.scope == id && record.blind {
				c.Close()
			}
		}
		m.run.mu.Unlock()
	}
	return nil
}

func (m *Manager) observe(ctx context.Context, scope string, v identityView) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	m.mu.Lock()
	activeScope := false
	for _, binding := range m.state.ConversationBindings {
		if binding.ScopeID == scope && binding.AccountID != "" {
			activeScope = true
			break
		}
	}
	m.mu.Unlock()
	resolved := map[string]string{}
	var lookupErr error
	if v.SessionID != "" {
		ids := []string{v.SessionID}
		if activeScope {
			resolved, lookupErr = m.resolveConversations(ctx, scope, ids)
		} else if cached, ok := m.cfg.Conversations.(CachedConversationResolver); ok {
			resolved, lookupErr = validateConversations(ids, cached.ResolveCached(ids, m.knownConversationAssociations(scope, ids)))
		}
		if lookupErr != nil {
			resolved = map[string]string{}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed {
		return Session{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	if _, ok := m.state.Scopes[scope]; !ok {
		return Session{}, ErrNotFound
	}
	if v.SessionID == "" {
		return Session{}, nil
	}
	key := taskKey(scope, v.SessionID)
	s, exists := m.state.Sessions[key]
	previous := s
	if !exists {
		if len(m.state.Sessions)+len(m.state.ConversationBindings) >= 4096 {
			return Session{}, ErrBusy
		}
		s = Session{ScopeID: scope, SessionID: v.SessionID, Revision: 1}
	}
	if m.activeConversationLocked(s) {
		if lookupErr != nil {
			return Session{}, lookupErr
		}
		if !activeScope {
			return Session{}, ErrConflict
		}
		if err := verifiedAssociation(s, resolved[v.SessionID]); err != nil {
			return Session{}, err
		}
	}
	// Stale or unavailable metadata has no authority over caller/exact routes.
	// Keep a prior observation rather than reassociating it to another chat.
	changed := false
	if conversation := resolved[v.SessionID]; conversation != "" && s.ConversationID == "" {
		s.ConversationID = conversation
		changed = true
	}
	s.conversationRevision = 0
	if binding, ok := m.state.ConversationBindings[taskKey(scope, s.ConversationID)]; ok && binding.AccountID != "" {
		if !activeScope {
			return Session{}, ErrConflict
		}
		if m.cfg.Conversations == nil {
			return Session{}, associationFailure("conversation_association_unavailable", ErrUnavailable)
		}
		if s.AccountID != binding.AccountID {
			s.AccountID = binding.AccountID
			changed = true
		}
		s.conversationRevision = binding.Revision
	}
	if changed && exists {
		if s.Revision == ^uint64(0) {
			return Session{}, ErrBusy
		}
		s.Revision++
	}
	s.LastSeen = time.Now().UTC()
	s.Requests++
	s.Model = v.Model
	if v.AgentID != "" {
		s.AgentID = v.AgentID
	}
	if v.ParentID != "" {
		s.ParentSessionID = v.ParentID
	}
	m.state.Sessions[key] = s
	if !exists || changed {
		if err := m.saveLocked(); err != nil {
			if rollbackAllowed(err) {
				if exists {
					m.state.Sessions[key] = previous
				} else {
					delete(m.state.Sessions, key)
				}
			}
			return Session{}, err
		}
	}
	return s, nil
}

// admit is the account snapshot's linearization point. No credential is sent
// until preparation and the revision check have both completed.
func (m *Manager) admit(scope string, s Session) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failed || m.run == nil || m.run.ctx.Err() != nil || !m.state.Enabled {
		return nil, ErrUnavailable
	}
	if _, ok := m.state.Scopes[scope]; !ok {
		return nil, ErrNotFound
	}
	if s.SessionID == "" {
		return func() {}, nil
	}
	key := taskKey(scope, s.SessionID)
	current, ok := m.state.Sessions[key]
	if !ok {
		return nil, ErrNotFound
	}
	if current.Revision != s.Revision {
		return nil, ErrConflict
	}
	if m.state.ConversationBindings[taskKey(scope, s.ConversationID)].Revision != s.conversationRevision && m.activeConversationLocked(s) {
		return nil, ErrConflict
	}
	current.InFlight++
	m.state.Sessions[key] = current
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if current, ok := m.state.Sessions[key]; ok && current.InFlight > 0 {
			current.InFlight--
			m.state.Sessions[key] = current
		}
	}, nil
}

func (m *Manager) bindingCurrent(s Session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.state.Sessions[taskKey(s.ScopeID, s.SessionID)]
	return ok && current.Revision == s.Revision && current.AccountID == s.AccountID && (!m.activeConversationLocked(s) || m.state.ConversationBindings[taskKey(s.ScopeID, s.ConversationID)].Revision == s.conversationRevision)
}

func (m *Manager) activeConversationLocked(s Session) bool {
	return s.ConversationID != "" && m.state.ConversationBindings[taskKey(s.ScopeID, s.ConversationID)].AccountID != ""
}
