package proxy

import (
	"strings"
	"time"
)

type requestEpoch struct {
	generation, credentials, selection, exhaustion uint64
	reset                                          string
}

func (m *Manager) requestEpochLocked(providerID, id string) requestEpoch {
	return requestEpoch{generation: m.generation[id], credentials: m.accountRevision[id], selection: m.selectionRevision[providerID],
		exhaustion: m.exhaustionRevision[id], reset: m.resetEvents[id].ID}
}

func (m *Manager) requestCurrentLocked(providerID, id string, expected requestEpoch) bool {
	return m.requestEpochLocked(providerID, id) == expected && m.active[providerID] == id
}

// Reject superseded responses before either parking credentials or publishing
// a failover. The exhaustion mark and route change are one persisted decision.
// With holdFree, an account whose only way out is a Free account stays
// selected (held) so the caller can try a banked reset on it first.
func (m *Manager) handleExhaustion(providerID, id string, until time.Time, expected requestEpoch, holdFree bool) (retry, held bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.requestCurrentLocked(providerID, id, expected) {
		return true, false, nil
	}
	next, downgrade, err := m.failoverLocked(id, providerID)
	if err != nil {
		return false, false, err
	}
	if downgrade && holdFree {
		next, held = "", true
	}
	err = m.mutateStateLocked(func() {
		m.exhausted[id] = until
		m.quotaRevision[id]++
		if next != "" {
			m.active[providerID] = next
			m.selectionRevision[providerID]++
		}
	})
	if err == nil {
		// This response belongs to the current reset epoch. A later retry of
		// the old redemption must not clear this newly confirmed exhaustion.
		if event, ok := m.resetEvents[id]; ok {
			event.routingPending = false
			m.resetEvents[id] = event
		}
	}
	return next != "", held, err
}

// failoverLocked picks the account to move to when exclude stops serving:
// another paid account first, then a Free one. downgrade reports that only a
// Free account is left for a paid exclude: Free tiers lack the paid models,
// so a banked reset on exclude serves the user better. Requires m.mu.
func (m *Manager) failoverLocked(exclude, providerID string) (next string, downgrade bool, err error) {
	accounts, err := m.store.List()
	if err != nil {
		return "", false, err
	}
	excludeFree := false
	free := ""
	for _, a := range accounts {
		if a.Provider != providerID {
			continue
		}
		if a.ID == exclude {
			excludeFree = freePlan(a.Plan)
			continue
		}
		if !hasCredentials(a) || m.exhaustedNow(a.ID) {
			continue
		}
		if !freePlan(a.Plan) {
			return a.ID, false, nil
		}
		if free == "" {
			free = a.ID
		}
	}
	return free, free != "" && !excludeFree, nil
}

// freePlan reports a free tier ("free", "copilot_free"). An unknown plan
// counts as paid.
func freePlan(plan string) bool {
	return plan == "free" || strings.HasSuffix(plan, "_free")
}
