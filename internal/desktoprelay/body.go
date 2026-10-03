package desktoprelay

import (
	"net/http"
	"time"
)

func (m *Manager) bodyReadTimeout() time.Duration {
	if m.cfg.FixtureReadTimeout > 0 {
		return m.cfg.FixtureReadTimeout
	}
	return 30 * time.Second
}

func (m *Manager) rejectDrainTimeout() time.Duration {
	return min(time.Second, m.bodyReadTimeout())
}

// net/http can drain an incomplete request body both while writing a rejection
// and during handler cleanup. Arm the read deadline before any rejection and
// keep it armed through Body.Close. Only a complete, explicitly closed body may
// clear it for a long-lived upstream response or keepalive.
func (m *Manager) guardBody(w http.ResponseWriter, r *http.Request) func() {
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(m.rejectDrainTimeout()))
	closed := false
	return func() {
		if !closed {
			r.Body.Close()
			closed = true
		}
	}
}
