package desktoprelay

import (
	"context"
	"sort"
	"strings"
	"time"
)

// AssociationError retains no resolver error or source-specific text.
type AssociationError struct {
	Code string `json:"error_code"`
	kind error
}

func (e *AssociationError) Error() string {
	return "desktop relay conversation association could not be verified"
}
func (e *AssociationError) Unwrap() error     { return e.kind }
func (e *AssociationError) ErrorCode() string { return e.Code }

func associationFailure(code string, kind error) *AssociationError {
	return &AssociationError{Code: code, kind: kind}
}

// Resolver work is always outside the state lock and bounded by the caller's
// context. Validate every returned entry, including unsolicited aliases.
func (m *Manager) resolveConversations(ctx context.Context, scope string, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.cfg.Conversations == nil {
		return map[string]string{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result map[string]string
	var err error
	if historical, ok := m.cfg.Conversations.(HistoricalConversationResolver); ok {
		known := m.knownConversationAssociations(scope, ids)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err = historical.ResolveHistorical(ctx, append([]string(nil), ids...), known)
	} else {
		result, err = m.cfg.Conversations.Resolve(ctx, append([]string(nil), ids...))
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, associationFailure("conversation_association_unavailable", ErrUnavailable)
	}
	return validateConversations(ids, result)
}

func validateConversations(ids []string, result map[string]string) (map[string]string, error) {
	if len(result) > len(ids) {
		return nil, associationFailure("conversation_association_invalid", ErrUnavailable)
	}
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	out := make(map[string]string, len(result))
	for id, conversation := range result {
		if !wanted[id] || !validUUID(id) || !validUUID(conversation) {
			return nil, associationFailure("conversation_association_invalid", ErrUnavailable)
		}
		out[id] = conversation
	}
	return out, nil
}

func verifiedAssociation(s Session, resolved string) error {
	if s.ConversationID != "" && s.ConversationID != resolved {
		return associationFailure("conversation_association_changed", ErrConflict)
	}
	return nil
}

func (m *Manager) revalidateAssociation(ctx context.Context, s Session) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.AccountID == "" || s.conversationRevision == 0 {
		return nil
	}
	resolved, err := m.resolveConversations(ctx, s.ScopeID, []string{s.SessionID})
	if err != nil {
		return err
	}
	if resolved[s.SessionID] != s.ConversationID {
		return associationFailure("conversation_association_changed", ErrConflict)
	}
	return nil
}

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

func (m *Manager) BindConversation(ctx context.Context, scopeID, conversationID, accountID string, expectedRevision uint64, expectedMemberRevisions map[string]uint64) (ConversationBinding, error) {
	return m.changeConversation(ctx, scopeID, conversationID, accountID, expectedRevision, expectedMemberRevisions)
}

func (m *Manager) UnbindConversation(ctx context.Context, scopeID, conversationID string, expectedRevision uint64, expectedMemberRevisions map[string]uint64) (ConversationBinding, error) {
	return m.resetConversation(ctx, scopeID, conversationID, expectedRevision, expectedMemberRevisions)
}

func normalizeMembers(input map[string]uint64) (map[string]uint64, error) {
	if len(input) > 4096 {
		return nil, ErrBusy
	}
	out := make(map[string]uint64, len(input))
	for id, revision := range input {
		id = strings.ToLower(id)
		if !validUUID(id) || revision == 0 {
			return nil, ErrConflict
		}
		if _, duplicate := out[id]; duplicate {
			return nil, ErrConflict
		}
		out[id] = revision
	}
	return out, nil
}

func (m *Manager) changeConversation(ctx context.Context, scopeID, conversationID, accountID string, expectedRevision uint64, expectedMemberRevisions map[string]uint64) (ConversationBinding, error) {
	scopeID, conversationID = strings.ToLower(scopeID), strings.ToLower(conversationID)
	if !validUUID(scopeID) || !validUUID(conversationID) {
		return ConversationBinding{}, ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return ConversationBinding{}, err
	}
	expected, err := normalizeMembers(expectedMemberRevisions)
	if err != nil {
		return ConversationBinding{}, err
	}
	if m.cfg.Conversations == nil {
		return ConversationBinding{}, associationFailure("conversation_association_unavailable", ErrUnavailable)
	}
	m.mu.Lock()
	if m.run == nil || m.run.ctx.Err() != nil || m.failed || !m.state.Enabled {
		m.mu.Unlock()
		return ConversationBinding{}, ErrUnavailable
	}
	if _, ok := m.state.Scopes[scopeID]; !ok {
		m.mu.Unlock()
		return ConversationBinding{}, ErrNotFound
	}
	if m.state.ConversationBindings[taskKey(scopeID, conversationID)].Revision != expectedRevision {
		m.mu.Unlock()
		return ConversationBinding{}, ErrConflict
	}
	run := m.run
	snapshot := make(map[string]Session)
	ids := make([]string, 0)
	for _, s := range m.state.Sessions {
		if s.ScopeID == scopeID {
			snapshot[s.SessionID] = s
			ids = append(ids, s.SessionID)
		}
	}
	m.mu.Unlock()
	sort.Strings(ids)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(run.ctx, cancel)
	defer stop()
	resolved, err := m.resolveConversations(ctx, scopeID, ids)
	if err != nil {
		return ConversationBinding{}, err
	}
	members, err := conversationMembers(snapshot, resolved, conversationID, expected)
	if err != nil {
		return ConversationBinding{}, err
	}
	if m.cfg.Source == nil || accountID == "" || len(accountID) > 256 {
		return ConversationBinding{}, ErrUnavailable
	}
	credential, err := m.cfg.Source.Prepare(ctx, accountID)
	if ctx.Err() != nil {
		return ConversationBinding{}, ctx.Err()
	}
	if err != nil || !usable(credential, accountID) {
		return ConversationBinding{}, credentialFailure(err)
	}
	// A delayed credential preparation cannot publish stale metadata membership.
	checked, err := m.resolveConversations(ctx, scopeID, ids)
	if err != nil {
		return ConversationBinding{}, err
	}
	for _, id := range ids {
		if (snapshot[id].ConversationID == conversationID || resolved[id] == conversationID || checked[id] == conversationID) && checked[id] != resolved[id] {
			return ConversationBinding{}, ErrConflict
		}
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ConversationBinding{}, err
	}
	if m.run != run || run.ctx.Err() != nil || m.failed || !m.state.Enabled {
		return ConversationBinding{}, ErrUnavailable
	}
	if _, ok := m.state.Scopes[scopeID]; !ok {
		return ConversationBinding{}, ErrNotFound
	}
	key := taskKey(scopeID, conversationID)
	previous := m.state.ConversationBindings[key]
	if previous.Revision != expectedRevision {
		return ConversationBinding{}, ErrConflict
	}
	// Recheck the complete observed alias set. A newly observed alias must be
	// included by a fresh management view, even if it has no stored association.
	current := make(map[string]Session)
	for _, s := range m.state.Sessions {
		if s.ScopeID == scopeID {
			current[s.SessionID] = s
		}
	}
	if len(current) != len(snapshot) {
		return ConversationBinding{}, ErrConflict
	}
	for id, s := range current {
		old, ok := snapshot[id]
		if !ok || ((old.ConversationID == conversationID || s.ConversationID == conversationID || checked[id] == conversationID) && old.ConversationID != s.ConversationID) {
			return ConversationBinding{}, ErrConflict
		}
	}
	members, err = conversationMembers(current, checked, conversationID, expected)
	if err != nil {
		return ConversationBinding{}, err
	}
	return m.publishConversationLocked(ctx, scopeID, conversationID, accountID, expectedRevision, members)
}

// Callers hold both lifecycle and state locks and have checked every member's
// identity and revision. Publish group and member changes in one durable record.
func (m *Manager) publishConversationLocked(ctx context.Context, scopeID, conversationID, accountID string, expectedRevision uint64, members map[string]Session) (ConversationBinding, error) {
	key := taskKey(scopeID, conversationID)
	previous, exists := m.state.ConversationBindings[key]
	if previous.Revision != expectedRevision {
		return ConversationBinding{}, ErrConflict
	}
	if previous.Revision == ^uint64(0) || (!exists && len(m.state.Sessions)+len(m.state.ConversationBindings) >= 4096) {
		return ConversationBinding{}, ErrBusy
	}
	updated := ConversationBinding{ScopeID: scopeID, ConversationID: conversationID, AccountID: accountID, Revision: previous.Revision + 1}
	oldMembers := make(map[string]Session, len(members))
	for id := range members {
		s := members[id]
		oldMembers[id] = s
		if s.AccountID != accountID || s.ConversationID != conversationID {
			if s.Revision == ^uint64(0) {
				return ConversationBinding{}, ErrBusy
			}
			s.AccountID, s.ConversationID = accountID, conversationID
			s.Revision++
		}
		members[id] = s
	}
	if err := ctx.Err(); err != nil {
		return ConversationBinding{}, err
	}
	if m.state.ConversationBindings == nil {
		m.state.ConversationBindings = make(map[string]ConversationBinding)
	}
	m.state.ConversationBindings[key] = updated
	for id, s := range members {
		m.state.Sessions[taskKey(scopeID, id)] = s
	}
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			if exists {
				m.state.ConversationBindings[key] = previous
			} else {
				delete(m.state.ConversationBindings, key)
			}
			for id, s := range oldMembers {
				m.state.Sessions[taskKey(scopeID, id)] = s
			}
		}
		return ConversationBinding{}, err
	}
	return updated, nil
}

func conversationMembers(sessions map[string]Session, resolved map[string]string, conversation string, expected map[string]uint64) (map[string]Session, error) {
	members := make(map[string]Session)
	for id, s := range sessions {
		if s.ConversationID != conversation && resolved[id] != conversation {
			continue
		}
		if err := verifiedAssociation(s, resolved[id]); err != nil {
			return nil, err
		}
		if resolved[id] == conversation {
			members[id] = s
		}
	}
	if len(members) == 0 {
		return nil, ErrNotFound
	}
	if len(members) != len(expected) {
		return nil, ErrConflict
	}
	for id, s := range members {
		if expected[id] != s.Revision {
			return nil, ErrConflict
		}
	}
	return members, nil
}
