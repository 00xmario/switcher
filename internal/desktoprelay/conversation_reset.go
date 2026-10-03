package desktoprelay

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// Reset removes selection authority from saved members plus freshly verified
// observed aliases. Saved members need no current proof; additional aliases do.
// A reset without a saved group still requires live, verified associations.
func (m *Manager) resetConversation(ctx context.Context, scopeID, conversationID string, expectedRevision uint64, expectedMemberRevisions map[string]uint64) (ConversationBinding, error) {
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
	m.lifecycle.Lock()
	m.mu.Lock()
	err = m.initializeLocked(false)
	if err == nil {
		if m.store == nil || m.failed {
			err = ErrUnavailable
		} else if _, ok := m.state.Scopes[scopeID]; !ok {
			err = ErrNotFound
		}
	}
	key := taskKey(scopeID, conversationID)
	previous, exists := m.state.ConversationBindings[key]
	if err == nil && previous.Revision != expectedRevision {
		err = ErrConflict
	}
	running := m.run != nil && m.run.ctx.Err() == nil && m.state.Enabled
	if err == nil && !running && !exists {
		err = ErrNotFound
	}
	snapshot := make(map[string]Session)
	ids := make([]string, 0)
	if err == nil {
		for _, s := range m.state.Sessions {
			if s.ScopeID == scopeID {
				snapshot[s.SessionID] = s
				ids = append(ids, s.SessionID)
			}
		}
	}
	m.mu.Unlock()
	m.lifecycle.Unlock()
	if err != nil {
		return ConversationBinding{}, err
	}
	sort.Strings(ids)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// This context belongs to management, not the listener. Stop can complete
	// while a reset is resolving metadata without cancelling removal of authority.
	resolved, lookupErr := m.resolveConversations(ctx, scopeID, ids)
	if err := resetLookupCancellation(ctx, lookupErr); err != nil {
		return ConversationBinding{}, err
	}
	if _, err := resetMembers(snapshot, resolved, lookupErr, m.cfg.Conversations != nil, exists, running, conversationID, expected); err != nil {
		return ConversationBinding{}, err
	}
	checked, lookupErr := m.resolveConversations(ctx, scopeID, ids)
	if err := resetLookupCancellation(ctx, lookupErr); err != nil {
		return ConversationBinding{}, err
	}
	if _, err := resetMembers(snapshot, checked, lookupErr, m.cfg.Conversations != nil, exists, running, conversationID, expected); err != nil {
		return ConversationBinding{}, err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ConversationBinding{}, err
	}
	// Stop/Close may have released the store during either lookup. Reopen the
	// existing state without enabling it or starting a listener.
	if err := m.initializeLocked(false); err != nil {
		return ConversationBinding{}, err
	}
	if m.store == nil || m.failed {
		return ConversationBinding{}, ErrUnavailable
	}
	if _, ok := m.state.Scopes[scopeID]; !ok {
		return ConversationBinding{}, ErrNotFound
	}
	currentBinding, currentExists := m.state.ConversationBindings[key]
	if currentBinding.Revision != expectedRevision {
		return ConversationBinding{}, ErrConflict
	}
	current := make(map[string]Session)
	for _, s := range m.state.Sessions {
		if s.ScopeID == scopeID {
			current[s.SessionID] = s
		}
	}
	if lookupErr == nil && m.cfg.Conversations != nil {
		// Available metadata must cover the same observed aliases it resolved.
		// An unseen alias could be another member of this group.
		if len(current) != len(snapshot) {
			return ConversationBinding{}, ErrConflict
		}
		for id := range current {
			if _, ok := snapshot[id]; !ok {
				return ConversationBinding{}, ErrConflict
			}
		}
	}
	running = m.run != nil && m.run.ctx.Err() == nil && m.state.Enabled
	members, err := resetMembers(current, checked, lookupErr, m.cfg.Conversations != nil, currentExists, running, conversationID, expected)
	if err != nil {
		return ConversationBinding{}, err
	}
	return m.publishConversationLocked(ctx, scopeID, conversationID, "", expectedRevision, members)
}

func resetLookupCancellation(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func resetMembers(sessions map[string]Session, resolved map[string]string, lookupErr error, hasResolver, hasBinding, running bool, conversation string, expected map[string]uint64) (map[string]Session, error) {
	available := hasResolver && lookupErr == nil
	if !hasBinding {
		if !running {
			return nil, ErrNotFound
		}
		if available {
			return conversationMembers(sessions, resolved, conversation, expected)
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, associationFailure("conversation_association_unavailable", ErrUnavailable)
	}
	members := make(map[string]Session)
	for id, s := range sessions {
		if s.ConversationID == conversation {
			members[id] = s
			continue
		}
		// Reset removes authority from the union of saved owners and freshly
		// verified observed aliases. Missing proof for a saved owner cannot
		// exclude it, and an unrecorded alias needs explicit current proof.
		if available && resolved[id] == conversation {
			if err := verifiedAssociation(s, resolved[id]); err != nil {
				return nil, err
			}
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
