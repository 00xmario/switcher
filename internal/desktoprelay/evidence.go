package desktoprelay

import "time"

func publicSession(s Session) Session {
	if s.LastResponse != nil {
		evidence := *s.LastResponse
		s.LastResponse = &evidence
	}
	return s
}

// recordResponse keeps the last upstream status per session for the UI, so the
// user can see which account actually answered.
func (m *Manager) recordResponse(s Session, credential Credential, status int) {
	if s.SessionID == "" {
		return
	}
	evidence := &ResponseEvidence{Route: "caller", Model: s.Model, Status: status, At: time.Now().UTC()}
	if credential.AccountID != "" {
		evidence.Route, evidence.AccountID = "selected", credential.AccountID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskKey(s.ScopeID, s.SessionID)
	if current, ok := m.state.Sessions[key]; ok {
		current.LastResponse = evidence
		m.state.Sessions[key] = current
	}
}
