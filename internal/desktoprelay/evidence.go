package desktoprelay

import "time"

func publicSession(s Session) Session {
	if s.LastResponse != nil {
		evidence := *s.LastResponse
		s.LastResponse = &evidence
	}
	return s
}

func (m *Manager) nextResponseSequence() (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.responseSequence == ^uint64(0) {
		return 0, ErrBusy
	}
	m.responseSequence++
	return m.responseSequence, nil
}

func (m *Manager) recordResponse(s Session, credential Credential, status int, sequence uint64) {
	if s.SessionID == "" || sequence == 0 || status < 100 || status > 599 {
		return
	}
	evidence := &ResponseEvidence{Route: "caller", Model: s.Model, Status: status, At: time.Now().UTC(), Sequence: sequence}
	if credential.AccountID != "" {
		evidence.Route, evidence.AccountID = "selected", credential.AccountID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := taskKey(s.ScopeID, s.SessionID)
	current, ok := m.state.Sessions[key]
	if !ok || (current.LastResponse != nil && current.LastResponse.Sequence > sequence) {
		return
	}
	current.LastResponse = evidence
	m.state.Sessions[key] = current
}

func validEvidence(e *ResponseEvidence) bool {
	return e == nil || ((e.Route == "caller" && e.AccountID == "" || e.Route == "selected" && e.AccountID != "") && len(e.AccountID) <= 256 && len(e.Model) <= 256 && e.Status >= 100 && e.Status <= 599 && !e.At.IsZero())
}
