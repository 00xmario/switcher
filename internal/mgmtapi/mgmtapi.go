// Package mgmtapi implements the subset of CLIProxyAPI's Management API
// that T3 Code uses for its "Add a CLIProxyAPI hub" feature. Switcher
// speaks the same three routes so it can be added as a hub directly:
//
//	GET  /v0/management/auth-files   list pooled accounts
//	POST /v0/management/api-call     run a control-plane request with one
//	                                 account's credentials ($TOKEN$ swap)
//	POST /v0/management/reset-quota  clear an account's local exhaustion
//
// Every route requires the management key via Authorization: Bearer <key>.
package mgmtapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"crypto/subtle"
	"fmt"
	"switcher/internal/login"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parseURL(raw string) (*url.URL, error) {
	return url.Parse(raw)
}

// API wires the management compatibility surface.
type API struct {
	Store         *store.Store
	Proxy         *proxy.Manager
	Logins        *login.Manager
	ManagementKey string
}

// Register mounts the management routes.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v0/management/auth-files", a.authGuard(a.handleAuthFiles))
	mux.HandleFunc("POST /v0/management/api-call", a.authGuard(a.handleAPICall))
	mux.HandleFunc("POST /v0/management/reset-quota", a.authGuard(a.handleResetQuota))
}

// authGuard enforces the management key. CLIProxyAPI accepts both a bearer
// token and its X-Management-Key header; support both shapes.
func (a *API) authGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-Management-Key")
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.ManagementKey)) != 1 {
			http.Error(w, "management key required", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// validAuthIndex accepts only plain account ids: path traversal in the
// store file names is impossible through the hub surface.
func validAuthIndex(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// handleAuthFiles lists every stored account in CLIProxyAPI's shape. T3
// filters for codex and claude itself, so all providers are listed.
func (a *API) handleAuthFiles(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not list accounts"})
		return
	}
	files := make([]map[string]any, 0, len(accounts))
	for _, acc := range accounts {
		file := map[string]any{
			"id":         acc.ID,
			"auth_index": acc.ID,
			"name":       acc.ID + ".json",
			"provider":   acc.Provider,
			"email":      acc.Email,
			"disabled":   false,
			"status":     "ready",
			"source":     "file",
		}
		if acc.Provider == "codex" {
			// T3 reads the plan and the account id from the id_token object.
			file["id_token"] = map[string]any{
				"chatgpt_account_id": acc.Token.AccountID,
				"chatgpt_plan_type":  acc.Plan,
			}
		}
		files = append(files, file)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

// apiCallRequest mirrors CLIProxyAPI's management api-call body. Header
// values may contain the $TOKEN$ placeholder, which CPA swaps for the
// account's credentials before dispatching.
type apiCallRequest struct {
	AuthIndex string            `json:"auth_index"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header"`
	Data      json.RawMessage   `json:"data"`
}

// allowedControlPlaneHosts limits which upstream hosts a hub-driven
// api-call may touch. The management API is a credential-bearing
// primitive; keeping it on the control-plane hosts known to T3 keeps a
// leaked management key from becoming a token exfiltration tool.
var allowedControlPlaneHosts = map[string]bool{
	"api.anthropic.com": true,
	"chatgpt.com":       true,
}

func (a *API) handleAPICall(w http.ResponseWriter, r *http.Request) {
	var req apiCallRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body"})
		return
	}
	if req.AuthIndex == "" || req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "auth_index and url are required"})
		return
	}
	parsed, err := parseURL(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid url"})
		return
	}
	if !allowedControlPlaneHosts[strings.ToLower(parsed.Hostname())] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "url host is not allowed"})
		return
	}

	if !validAuthIndex(req.AuthIndex) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid auth_index"})
		return
	}
	account, err := a.Store.Get(req.AuthIndex)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown auth_index"})
		return
	}

	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if method == "" {
		method = http.MethodGet
	}
	var bodyReader io.Reader
	if len(req.Data) > 0 && string(req.Data) != "null" {
		bodyReader = bytes.NewReader(req.Data)
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), method, req.URL, bodyReader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	token := account.Token.AccessToken
	for key, value := range req.Header {
		httpReq.Header.Set(key, strings.ReplaceAll(value, "$TOKEN$", token))
	}

	// Redirects re-validate the allowlist, and headers that carried the
	// account token are stripped on any cross-host hop so the token can
	// never leak to a host the allowlist did not bless.
	tokenHeaderNames := []string{}
	for key, value := range req.Header {
		if strings.Contains(value, "$TOKEN$") {
			tokenHeaderNames = append(tokenHeaderNames, key)
		}
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many redirects")
		}
		if !allowedControlPlaneHosts[strings.ToLower(req.URL.Hostname())] {
			return fmt.Errorf("redirect host %s is not allowed", req.URL.Hostname())
		}
		for _, name := range tokenHeaderNames {
			req.Header.Del(name)
		}
		return nil
	}}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream request failed"})
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not read upstream response"})
		return
	}
	log.Printf("mgmt: api-call %s %s -> %d", req.Method, parsed.Host, resp.StatusCode)
	writeJSON(w, http.StatusOK, map[string]any{
		"status_code": resp.StatusCode,
		"body":        string(raw),
	})
}

func (a *API) handleResetQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || !validAuthIndex(req.AuthIndex) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid auth_index"})
		return
	}
	account, err := a.Store.Get(req.AuthIndex)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown auth_index"})
		return
	}
	a.Proxy.ClearExhausted(account.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"auth_index": req.AuthIndex,
	})
}
