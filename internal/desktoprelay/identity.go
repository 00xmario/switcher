package desktoprelay

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type identityView struct {
	SessionID string
	ParentID  string
	AgentID   string
	Model     string
	Thread    string // "create", "continue", or "" without a message thread
}

// identity reads the routing fields Claude Code sends. It never rejects a
// request: anything it cannot read simply leaves the caller's own credential
// in place.
func identity(r *http.Request, body []byte) identityView {
	var v identityView
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		if z, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			body, _ = io.ReadAll(z)
			z.Close()
		}
	}
	var top struct {
		Thread *struct {
			Type string `json:"type"`
		} `json:"thread"`
		Model    string `json:"model"`
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	json.Unmarshal(body, &top)
	v.Model = top.Model
	if top.Thread != nil {
		v.Thread = "create"
		if top.Thread.Type == "continue" {
			v.Thread = "continue"
		}
	}
	var user struct {
		SessionID       string `json:"session_id"`
		ParentSessionID string `json:"parent_session_id"`
		AgentID         string `json:"agent_id"`
	}
	json.Unmarshal([]byte(top.Metadata.UserID), &user)
	v.SessionID = strings.ToLower(r.Header.Get("X-Claude-Code-Session-Id"))
	if v.SessionID == "" {
		v.SessionID = strings.ToLower(user.SessionID)
	}
	if !validUUID(v.SessionID) {
		v.SessionID = ""
	}
	if parent := strings.ToLower(user.ParentSessionID); validUUID(parent) {
		v.ParentID = parent
	}
	v.AgentID = user.AgentID
	return v
}
