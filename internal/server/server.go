// Package server exposes Switcher's HTTP surface: the JSON API the web UI
// talks to, plus local-origin protections.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"switcher/internal/login"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
	"switcher/internal/update"
)

// API wraps the JSON API the web UI talks to.
type API struct {
	Store         *store.Store
	Logins        *login.Manager
	Proxy         *proxy.Manager
	Providers     map[string]provider.Provider
	ManagementKey string
	Version       string
	Updater       *update.Checker
}

// Register mounts the API on the given mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("POST /api/login", a.handleLoginStart)
	mux.HandleFunc("GET /api/login/{state}", a.handleLoginPoll)
	mux.HandleFunc("POST /api/accounts/{id}/activate", a.handleActivate)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", a.handleRefreshUsage)
	mux.HandleFunc("POST /api/accounts", a.handleAddKey)
	mux.HandleFunc("POST /api/accounts/{id}/use-reset", a.handleUseReset)
	mux.HandleFunc("POST /api/update", a.handleUpdate)
	mux.HandleFunc("PATCH /api/providers/order", a.handleProviderOrder)
	mux.HandleFunc("POST /api/providers/{id}/hide", a.handleProviderHide)
	mux.HandleFunc("POST /api/providers/{id}/show", a.handleProviderShow)
	mux.HandleFunc("DELETE /api/accounts/{id}", a.handleDelete)
}

// LocalOnly guards the API against other websites: it rejects requests
// whose Host is not the local listener (DNS rebinding) and, on state
// changing methods, rejects cross-site browser requests via the Origin and
// Sec-Fetch-Site headers. The codex proxy path is intentionally exempt:
// the CLI sends no Origin header.
func LocalOnly(port int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocalHost(r.Host, port) {
			http.Error(w, "switcher only accepts local connections", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !isLocalOrigin(origin, port) {
				http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
				return
			}
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site requests are not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalHost(host string, port int) bool {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}

func isLocalOrigin(origin string, port int) bool {
	origin = strings.ToLower(origin)
	return strings.HasPrefix(origin, "http://127.0.0.1:") ||
		strings.HasPrefix(origin, "http://localhost:")
}

// accountView is the API representation of one account, including the
// routing status the UI renders. It deliberately never contains tokens.
type accountView struct {
	ID             string            `json:"id"`
	Provider       string            `json:"provider"`
	Email          string            `json:"email"`
	Plan           string            `json:"plan,omitempty"`
	Active         bool              `json:"active"`
	ExhaustedUntil int64             `json:"exhausted_until,omitempty"`
	LastRefresh    int64             `json:"last_refresh,omitempty"`
	Usage          *provider.Usage   `json:"usage,omitempty"`
	ResetCredits   *resetCreditsView `json:"reset_credits,omitempty"`
}

// resetCreditsView surfaces banked usage-limit resets (codex).
type resetCreditsView struct {
	Count         int    `json:"count"`
	NextID        string `json:"next_id,omitempty"`
	NextExpiresAt int64  `json:"next_expires_at,omitempty"`
}

func (a *API) viewOf(acc store.Account) accountView {
	v := viewOfBase(a, acc)
	// Providers with banked resets report how many are available so the UI
	// can offer spending one.
	if rc, ok := a.Providers[acc.Provider].(provider.ResetCreditProvider); ok {
		if credits, err := rc.ListResetCredits(context.Background(), acc); err == nil && len(credits) > 0 {
			v.ResetCredits = &resetCreditsView{
				Count:         len(credits),
				NextID:        credits[0].ID,
				NextExpiresAt: credits[0].ExpiresAt,
			}
		}
	}
	return v
}

func viewOfBase(a *API, acc store.Account) accountView {
	v := accountView{
		ID:          acc.ID,
		Provider:    acc.Provider,
		Email:       acc.Email,
		Plan:        acc.Plan,
		Active:      acc.ID == a.Proxy.ActiveID(acc.Provider),
		LastRefresh: acc.LastRefresh,
	}
	if until, ok := a.Proxy.Exhausted(acc.ID); ok {
		v.ExhaustedUntil = until.Unix()
	}
	if usage, ok := a.Proxy.LastUsage(acc.ID); ok {
		u := usage
		v.Usage = &u
	}
	return v
}

func (a *API) handleState(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.Store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not list accounts"})
		return
	}
	views := make([]accountView, 0, len(accounts))
	for _, acc := range accounts {
		views = append(views, a.viewOf(acc))
	}
	order, hidden := a.Proxy.Providers()
	if hidden == nil {
		hidden = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active":             a.Proxy.ActiveAll(),
		"accounts":           views,
		"order":              order,
		"hidden":             hidden,
		"hub_url":            "http://127.0.0.1:8787",
		"hub_management_key": a.ManagementKey,
	})
}

func (a *API) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "expected application/json body", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	prov, ok := a.Providers[body.Provider]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	handle, err := a.Logins.Start(r.Context(), prov)
	if errors.Is(err, provider.ErrUnsupported) {
		// Providers without a browser callback (grok) use the device
		// authorization grant instead: also a web login, just with a code.
		handle, err = a.Logins.StartDevice(context.WithoutCancel(r.Context()), prov)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not start login"})
		return
	}
	writeJSON(w, http.StatusOK, handle)
}

// handleAddKey registers an API-key account (e.g. OpenCode).
func (a *API) handleAddKey(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "expected application/json body", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Key      string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	prov, ok := a.Providers[body.Provider]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	account, err := prov.AddByKey(r.Context(), body.Key)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that key did not work: " + err.Error()})
		return
	}
	if err := a.Store.Save(account); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not store account"})
		return
	}
	if a.Proxy.ActiveID(account.Provider) == "" {
		_ = a.Proxy.Activate(account.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "account": a.viewOf(account)})
}

func (a *API) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.PathValue("state")
	account, err, finished := a.Logins.Outcome(state, time.Second)
	if !finished {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	}
	if errors.Is(err, login.ErrUnknown) {
		// Already delivered or never started: the UI stops polling.
		writeJSON(w, http.StatusOK, map[string]string{"status": "finished"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "error": "login did not complete"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "done",
		"account": a.viewOf(account),
	})
}

func (a *API) handleActivate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}
	if err := a.Proxy.Activate(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such account"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not activate account"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "active": id})
}

func (a *API) handleRefreshUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}
	account, err := a.Store.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such account"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read account"})
		return
	}
	usage := a.Proxy.RefreshUsage(r.Context(), account)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "usage": usage})
}

func (a *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}
	if err := a.Store.Delete(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete account"})
		return
	}
	a.Proxy.Remove(id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// UpdateState wraps the updater's cached state.
func (a *API) UpdateState() update.State {
	if a.Updater == nil || a.Version == "" {
		return update.State{}
	}
	return a.Updater.State()
}

// handleUpdate installs the latest release and restarts in place. The
// exec-restart replaces the process image, so the response may never
// reach the client; the UI polls /api/state until the new version shows.
func (a *API) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if err := a.Updater.InstallAndRestart(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Unreachable: InstallAndRestart never returns on success.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleProviderOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	a.Proxy.ReorderProviders(body.Order)
	order, _ := a.Proxy.Providers()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "order": order})
}

func (a *API) handleProviderHide(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid provider id", http.StatusBadRequest)
		return
	}
	a.Proxy.HideProvider(id)
	_, hidden := a.Proxy.Providers()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "hidden": hidden})
}

func (a *API) handleProviderShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid provider id", http.StatusBadRequest)
		return
	}
	a.Proxy.ShowProvider(id)
	order, hidden := a.Proxy.Providers()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "order": order, "hidden": hidden})
}

// handleUseReset redeems one banked reset for the account and clears its
// local exhaustion park so routing resumes immediately.
func (a *API) handleUseReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}
	account, err := a.Store.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such account"})
		return
	}
	rc, ok := a.Providers[account.Provider].(provider.ResetCreditProvider)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this provider has no banked resets"})
		return
	}
	credits, err := rc.ListResetCredits(r.Context(), account)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not list banked resets"})
		return
	}
	if len(credits) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no banked reset available for this account"})
		return
	}
	outcome, err := rc.ConsumeResetCredit(r.Context(), account, credits[0].ID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the reset did not go through"})
		return
	}
	switch outcome {
	case "reset", "already_redeemed":
		a.Proxy.ClearExhausted(account.ID)
		usage := a.Proxy.RefreshUsage(r.Context(), account)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "outcome": outcome, "usage": usage})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "outcome": outcome})
	}
}

// validID accepts only the identifiers Switcher itself generates
// (provider + hex suffix), which keeps store paths inside the accounts dir.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
