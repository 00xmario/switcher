package proxy

import (
	"maps"
	"time"
)

// Routing writes publish only after persistence. Revision counters must roll
// back with the decision, otherwise an unsuccessful write invalidates requests.
type routingSnapshot struct {
	active                map[string]string
	exhausted             map[string]time.Time
	selection, exhaustion map[string]uint64
	quota                 map[string]uint64
	order, hidden         []string
	managementKey         string
	nativeCommit          string
}

func (m *Manager) routingSnapshotLocked() routingSnapshot {
	return routingSnapshot{active: maps.Clone(m.active), exhausted: maps.Clone(m.exhausted),
		selection: maps.Clone(m.selectionRevision), exhaustion: maps.Clone(m.exhaustionRevision), quota: maps.Clone(m.quotaRevision),
		order: append([]string(nil), m.order...), hidden: append([]string(nil), m.hidden...),
		managementKey: m.managementKey, nativeCommit: m.nativeClaudeCommit}
}

func (s routingSnapshot) restore(m *Manager) {
	m.active, m.exhausted = s.active, s.exhausted
	m.selectionRevision, m.exhaustionRevision, m.quotaRevision = s.selection, s.exhaustion, s.quota
	m.order, m.hidden, m.managementKey, m.nativeClaudeCommit = s.order, s.hidden, s.managementKey, s.nativeCommit
}

func (m *Manager) mutateStateLocked(change func()) error {
	before := m.routingSnapshotLocked()
	change()
	if err := m.persistLocked(); err != nil {
		before.restore(m)
		return err
	}
	return nil
}

func (m *Manager) removeRoutingLocked(id string) {
	delete(m.exhausted, id)
	m.exhaustionRevision[id]++
	for providerID, activeID := range m.active {
		if activeID == id {
			delete(m.active, providerID)
			m.selectionRevision[providerID]++
		}
	}
}
