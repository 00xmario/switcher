package proxy

import "time"

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
func (m *Manager) handleExhaustion(providerID, id string, until time.Time, expected requestEpoch) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.requestCurrentLocked(providerID, id, expected) {
		return true, nil
	}
	next, err := m.nextAvailableLocked(id, providerID)
	if err != nil {
		return false, err
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
	return next != "", err
}
