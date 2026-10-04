package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"switcher/internal/desktoprelay"
	"switcher/internal/sessionmeta"
	"switcher/internal/store"
)

const desktopControlHeader = "X-Switcher-Desktop-Control"

var errDesktopSetupConflict = errors.New("desktop relay setup conflict")

func desktopRelayLocalRequest(r *http.Request) bool {
	if !isLoopbackRequest(r) || strings.ContainsAny(r.Host, "@/\\?# \t\r\n") {
		return false
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		value, err := strconv.Atoi(port)
		return err == nil && value > 0 && value <= 65535
	}
	return true
}

func desktopRelayRoute(path string) bool {
	switch path {
	case "/api/desktop-relay", "/api/desktop-relay/start", "/api/desktop-relay/stop", "/api/desktop-relay/scopes",
		"/api/desktop-relay/configure", "/api/desktop-relay/restore", "/api/desktop-relay/restart-desktop":
		return true
	}
	parts := strings.Split(path, "/")
	if len(parts) != 5 && len(parts) != 8 {
		return false
	}
	if parts[0] != "" || parts[1] != "api" || parts[2] != "desktop-relay" || parts[3] != "scopes" || !validID(parts[4]) {
		return false
	}
	if len(parts) == 5 {
		return true
	}
	return parts[7] == "account" && ((parts[5] == "sessions" && validID(parts[6])) ||
		(parts[5] == "conversations" && validDesktopConversationID(parts[6])))
}

func (a *API) registerDesktopRelayRoutes(mux *http.ServeMux) {
	for _, route := range []string{
		"GET /api/desktop-relay",
		"POST /api/desktop-relay/start",
		"POST /api/desktop-relay/stop",
		"POST /api/desktop-relay/configure",
		"POST /api/desktop-relay/restore",
		"POST /api/desktop-relay/restart-desktop",
		"POST /api/desktop-relay/scopes",
		"DELETE /api/desktop-relay/scopes/{scope}",
		"POST /api/desktop-relay/scopes/{scope}/sessions/{session}/account",
		"DELETE /api/desktop-relay/scopes/{scope}/sessions/{session}/account",
		"POST /api/desktop-relay/scopes/{scope}/conversations/{conversation}/account",
		"DELETE /api/desktop-relay/scopes/{scope}/conversations/{conversation}/account",
	} {
		mux.HandleFunc(route, a.handleDesktopRelay)
	}
}

func (a *API) handleDesktopRelay(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !desktopRelayLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "desktop relay controls require a local connection"})
		return
	}
	switch AuthKind(r) {
	case AuthCookie, AuthDevice:
		// Cookie requests already passed the auth gate's CSRF check.
	default:
		keys := r.Header.Values(desktopControlHeader)
		if len(keys) != 1 || !desktopControlKeyMatches(keys[0], a.ManagementKey) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "desktop relay control authority required"})
			return
		}
	}
	setupMutation := r.URL.Path == "/api/desktop-relay/configure" || r.URL.Path == "/api/desktop-relay/restore" || r.URL.Path == "/api/desktop-relay/restart-desktop"
	if a.DesktopRelay == nil && !setupMutation {
		writeDesktopRelayError(w, desktoprelay.ErrUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	switch r.URL.Path {
	case "/api/desktop-relay":
		a.DesktopRelay.AssociateConversations(ctx)
		scopes, sessions, bindings := a.DesktopRelay.Scopes(), a.DesktopRelay.Sessions(), a.DesktopRelay.ConversationBindings()
		var info map[string]sessionmeta.Info
		if a.DesktopSessionMetadata != nil && len(sessions) != 0 {
			ids := make([]string, len(sessions))
			for j, session := range sessions {
				ids[j] = session.SessionID
			}
			info = a.DesktopSessionMetadata.Lookup(ctx, ids)
		}
		views := make([]desktopRelaySessionView, len(sessions))
		for j, session := range sessions {
			views[j] = desktopRelaySessionView{Session: session, Info: info[session.SessionID]}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": a.desktopRelayStatus(), "setup": a.desktopRelaySetup(), "scopes": scopes,
			"sessions": views, "conversation_bindings": bindings})
	case "/api/desktop-relay/configure", "/api/desktop-relay/restore":
		if !decodeDesktopRelayBody(w, r, &struct{}{}, true) {
			return
		}
		if a.DesktopRelay == nil {
			writeDesktopRelayError(w, desktoprelay.ErrUnavailable)
			return
		}
		if !desktopAppOperation.TryLock() {
			writeDesktopRelayError(w, desktoprelay.ErrBusy)
			return
		}
		defer desktopAppOperation.Unlock()
		var setup desktoprelay.SetupStatus
		var err error
		if r.URL.Path == "/api/desktop-relay/configure" {
			setup, err = a.DesktopRelay.Configure(ctx, a.DesktopSettingsPath)
		} else {
			setup, err = a.DesktopRelay.RestoreSetup(ctx, a.DesktopSettingsPath)
		}
		if err != nil {
			writeDesktopRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"setup": publicDesktopRelaySetup(setup), "status": a.desktopRelayStatus()})
	case "/api/desktop-relay/restart-desktop":
		a.handleDesktopRestart(w, r)
	case "/api/desktop-relay/start", "/api/desktop-relay/stop":
		var err error
		if r.URL.Path == "/api/desktop-relay/start" {
			err = a.DesktopRelay.Start(ctx)
		} else {
			err = a.DesktopRelay.Stop(ctx)
		}
		if err != nil {
			writeDesktopRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": a.desktopRelayStatus()})
	case "/api/desktop-relay/scopes":
		var body struct {
			Label string `json:"label"`
		}
		if !decodeDesktopRelayBody(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.Label) == "" || len(body.Label) > 256 || !utf8.ValidString(body.Label) {
			writeDesktopRelayBadRequest(w)
			return
		}
		setup, err := a.DesktopRelay.CreateScope(body.Label)
		if err != nil {
			writeDesktopRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, setup)
	default:
		a.handleDesktopRelaySelection(w, r, ctx)
	}
}

// desktopRelaySessionView adds saved Desktop titles to a request session. The
// session's own conversation_id wins over the metadata's (which is not serialized).
type desktopRelaySessionView struct {
	desktoprelay.Session
	sessionmeta.Info
}

func (a *API) desktopRelayStatus() desktoprelay.Status {
	if a.DesktopRelay == nil {
		return desktoprelay.Status{Condition: "unavailable", Validation: "fixture_tested"}
	}
	status := a.DesktopRelay.Status()
	switch status.Condition {
	case "store_error", "listen_error":
		status.Condition = "unavailable"
	}
	return status
}

func (a *API) desktopRelaySetup() desktoprelay.SetupStatus {
	if a.DesktopRelay == nil {
		return desktoprelay.SetupStatus{Condition: "unavailable"}
	}
	return publicDesktopRelaySetup(a.DesktopRelay.SetupStatus(a.DesktopSettingsPath))
}

func publicDesktopRelaySetup(setup desktoprelay.SetupStatus) desktoprelay.SetupStatus {
	// Do not serialize filesystem or actor error strings, even if a future
	// module change attaches one to its optional message field.
	setup.Message = ""
	return setup
}

func decodeDesktopRelayBody(w http.ResponseWriter, r *http.Request, body any, allowEmpty ...bool) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeDesktopRelayBadRequest(w)
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 && (len(allowEmpty) != 0 && allowEmpty[0]) {
		return true
	}
	if json.Unmarshal(raw, body) != nil {
		writeDesktopRelayBadRequest(w)
		return false
	}
	return true
}

func validDesktopConversationID(id string) bool {
	if len(id) != 36 || id == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	for j, c := range id {
		if j == 8 || j == 13 || j == 18 || j == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func writeDesktopRelayBadRequest(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid desktop relay request body or identifier"})
}

func (a *API) handleDesktopRelaySelection(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if conversation := r.PathValue("conversation"); conversation != "" {
		a.handleDesktopRelayConversationSelection(w, r, ctx, conversation)
		return
	}
	scope, session := r.PathValue("scope"), r.PathValue("session")
	if !validID(scope) || (session != "" && !validID(session)) {
		writeDesktopRelayBadRequest(w)
		return
	}
	if session == "" {
		if err := a.DesktopRelay.DeleteScope(scope); err != nil {
			writeDesktopRelayError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	var selected desktoprelay.Session
	var err error
	if r.Method == http.MethodPost {
		var body struct {
			AccountID string  `json:"account_id"`
			Revision  *uint64 `json:"revision"`
		}
		if !decodeDesktopRelayBody(w, r, &body) {
			return
		}
		if !validID(body.AccountID) || body.Revision == nil {
			writeDesktopRelayBadRequest(w)
			return
		}
		if !a.desktopRelayClaudeAccount(w, body.AccountID) {
			return
		}
		selected, err = a.DesktopRelay.Bind(ctx, scope, session, body.AccountID, *body.Revision)
	} else {
		var body struct {
			Revision *uint64 `json:"revision"`
		}
		if !decodeDesktopRelayBody(w, r, &body) {
			return
		}
		if body.Revision == nil {
			writeDesktopRelayBadRequest(w)
			return
		}
		selected, err = a.DesktopRelay.Unbind(scope, session, *body.Revision)
	}
	if err != nil {
		writeDesktopRelayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": selected})
}

func (a *API) handleDesktopRelayConversationSelection(w http.ResponseWriter, r *http.Request, ctx context.Context, conversation string) {
	scope := r.PathValue("scope")
	if !validID(scope) || !validDesktopConversationID(conversation) {
		writeDesktopRelayBadRequest(w)
		return
	}
	var selected desktoprelay.ConversationBinding
	var err error
	if r.Method == http.MethodPost {
		var body struct {
			AccountID string `json:"account_id"`
		}
		if !decodeDesktopRelayBody(w, r, &body) {
			return
		}
		if !validID(body.AccountID) {
			writeDesktopRelayBadRequest(w)
			return
		}
		if !a.desktopRelayClaudeAccount(w, body.AccountID) {
			return
		}
		selected, err = a.DesktopRelay.BindConversation(ctx, scope, conversation, body.AccountID)
	} else {
		selected, err = a.DesktopRelay.UnbindConversation(ctx, scope, conversation)
	}
	if err != nil {
		writeDesktopRelayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conversation_binding": selected})
}

func (a *API) desktopRelayClaudeAccount(w http.ResponseWriter, id string) bool {
	if a.Store == nil {
		writeDesktopRelayError(w, desktoprelay.ErrUnavailable)
		return false
	}
	account, err := a.Store.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = desktoprelay.ErrNotFound
		}
		writeDesktopRelayError(w, err)
		return false
	}
	if account.ID != id || account.Provider != "claude" {
		writeDesktopRelayBadRequest(w)
		return false
	}
	return true
}

func writeDesktopRelayError(w http.ResponseWriter, err error) {
	code, message := http.StatusServiceUnavailable, "desktop relay unavailable"
	var setupError *desktoprelay.SetupError
	isSetupError := errors.As(err, &setupError)
	switch {
	case errors.Is(err, errDesktopSetupConflict) || (isSetupError && errors.Is(err, desktoprelay.ErrConflict)):
		code, message = http.StatusConflict, "desktop relay setup changed; reload setup before retrying"
	case errors.Is(err, desktoprelay.ErrNotFound):
		code, message = http.StatusNotFound, "desktop relay account, scope, session or conversation not found"
	case errors.Is(err, desktoprelay.ErrConflict):
		code, message = http.StatusConflict, "desktop relay selection changed; reload and try again"
	case errors.Is(err, desktoprelay.ErrBusy):
		code, message = http.StatusConflict, "desktop relay is busy; reload status before retrying"
	}
	reply := map[string]string{"error": message}
	if isSetupError {
		// The code is a public classification, never an arbitrary actor string.
		switch setupError.ErrorCode() {
		case "setup_owned", "setup_changed", "setup_invalid_settings", "setup_invalid_path", "setup_lock_busy", "setup_busy", "setup_unavailable":
			reply["error_code"] = setupError.ErrorCode()
		}
	}
	var credentialError *desktoprelay.CredentialError
	if errors.As(err, &credentialError) {
		reply["error_code"] = credentialError.ErrorCode()
	}
	writeJSON(w, code, reply)
}

func desktopControlKeyMatches(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
