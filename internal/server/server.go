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

	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"switcher/internal/codexcfg"
	"switcher/internal/config"
	"switcher/internal/login"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
	"switcher/internal/update"
	"switcher/internal/usage"
	"sync"
	"syscall"
)

// API wraps the JSON API the web UI talks to.
type API struct {
	Store             *store.Store
	Logins            *login.Manager
	Proxy             *proxy.Manager
	Providers         map[string]provider.Provider
	ManagementKey     string
	Version           string
	Updater           *update.Checker
	Usage             *usage.Service
	Settings          *settings.Store
	Port              int
	CodexConfigPath   string // optional test override
	probeCodexForTest func(context.Context) proxy.ProbeCodexResult

	creditsMu    sync.Mutex
	creditsCache map[string]creditsEntry
}

type creditsEntry struct {
	credits []provider.ResetCredit
	ok      bool
	at      time.Time
}

// Register mounts the API on the given mux, initialising the lazily
// allocated caches first.
func (a *API) Register(mux *http.ServeMux) {
	if a.creditsCache == nil {
		a.creditsCache = map[string]creditsEntry{}
	}
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/cli-setup", a.handleCLISetup)
	mux.HandleFunc("POST /api/cli-setup/codex/install", a.handleCLISetupInstall)
	mux.HandleFunc("POST /api/cli-setup/codex/reselect", a.handleCLISetupReselect)
	mux.HandleFunc("POST /api/cli-setup/codex/restore", a.handleCLISetupRestore)
	mux.HandleFunc("POST /api/cli-setup/codex/remove-legacy", a.handleCLISetupRemoveLegacy)
	mux.HandleFunc("POST /api/cli-setup/codex/test", a.handleCodexRouteTest)
	mux.HandleFunc("POST /api/login", a.handleLoginStart)
	a.registerAuthRoutes(mux)
	mux.HandleFunc("POST /api/login/import", a.handleLoginImport)
	mux.HandleFunc("GET /api/login/{state}", a.handleLoginPoll)
	mux.HandleFunc("POST /api/accounts/{id}/activate", a.handleActivate)
	mux.HandleFunc("POST /api/accounts/{id}/refresh", a.handleRefreshUsage)
	mux.HandleFunc("POST /api/accounts/{id}/recheck", a.handleRecheckAccount)
	mux.HandleFunc("POST /api/usage/refresh", a.handleRefreshAll)
	mux.HandleFunc("GET /api/tokens", a.handleUsage)
	mux.HandleFunc("POST /api/tokens/refresh", a.handleUsageRefresh)
	mux.HandleFunc("POST /api/accounts", a.handleAddKey)
	mux.HandleFunc("POST /api/accounts/{id}/use-reset", a.handleUseReset)
	mux.HandleFunc("POST /api/update", a.handleUpdate)
	mux.HandleFunc("PATCH /api/providers/order", a.handleProviderOrder)
	mux.HandleFunc("POST /api/providers/{id}/hide", a.handleProviderHide)
	mux.HandleFunc("POST /api/providers/{id}/show", a.handleProviderShow)
	mux.HandleFunc("DELETE /api/accounts/{id}", a.handleDelete)
}

func (a *API) cliSetupPath() string {
	if a.CodexConfigPath != "" {
		return a.CodexConfigPath
	}
	return config.CodexConfigPath()
}

func (a *API) cliSetupPort() int {
	if a.Port > 0 {
		return a.Port
	}
	return config.DefaultPort
}

// CLI setup describes native clients, not provider logins. Only Codex has
// a configuration adapter today; the other rows are informational until
// their native protocol and configuration precedence have been verified.
type cliSetupClient struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Provider      string               `json:"provider"`
	Capability    string               `json:"capability"`
	Stage         string               `json:"stage"`
	ReasonCode    string               `json:"reason_code,omitempty"`
	NextStep      string               `json:"next_step,omitempty"`
	Installation  cliSetupInstallation `json:"installation"`
	Configuration cliSetupConfig       `json:"configuration"`
	Accounts      cliSetupAccounts     `json:"accounts"`
	Verification  cliSetupVerification `json:"verification"`
}

type cliSetupInstallation struct {
	Evidence string `json:"evidence"`
}

type cliSetupConfig struct {
	Condition        string `json:"condition"`
	Scope            string `json:"scope"`
	ExpectedURL      string `json:"expected_url,omitempty"`
	Ownership        string `json:"ownership,omitempty"`
	InstallAction    string `json:"install_action,omitempty"`
	RestoreAction    string `json:"restore_action,omitempty"`
	PendingOperation string `json:"pending_operation,omitempty"`
}

type cliSetupAccounts struct {
	Evidence string `json:"evidence"`
	Count    *int   `json:"count"`
}

var cliSetupTargets = []struct {
	id, name, provider, stage, reason, nextStep string
}{
	{"codex", "Codex CLI", "codex", "available", "", ""},
	{"claude-code", "Claude Code", "claude", "protocol_validation_pending", "oauth_coexistence_unverified",
		"Keep Claude Code's native login. Independent OAuth refresh and effective CLI routing need an isolated test before setup is offered."},
	{"opencode-go-v2", "OpenCode Go (v2)", "opencode", "protocol_validation_pending", "v2_account_model_transport_unverified",
		"Use OpenCode Go directly until its native account, selected model, background server, and HTTP or WebSocket transport are verified."},
	{"grok-build", "Grok Build", "grok", "research", "entitlement_and_relay_unverified",
		"Keep Grok's native settings. An active plan is needed to validate entitlement and the separate WebSocket relay later."},
	{"copilot-cli", "GitHub Copilot CLI", "copilot", "research", "byok_is_different_billing",
		"Keep native Copilot CLI authentication. Its custom-provider BYOK mode is not proven to use the same Copilot subscription."},
	{"gemini-cli", "Gemini CLI", "gemini", "research", "subscription_route_unverified",
		"Keep Gemini CLI settings unchanged. Account eligibility and a subscription-preserving endpoint need native verification."},
	{"antigravity-cli", "Antigravity CLI (agy)", "antigravity", "research", "agy_protocol_unverified",
		"Keep Antigravity CLI settings unchanged. Its CLI protocol and account-plan mapping need native verification."},
}

func (a *API) handleCLISetup(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	codex := codexcfg.Check(a.cliSetupPath(), a.cliSetupPort())
	counts := map[string]int{}
	accountsKnown := false
	if a.Store != nil {
		if accounts, err := a.Store.List(); err == nil {
			accountsKnown = true
			for _, account := range accounts {
				counts[account.Provider]++
			}
		}
	}
	clients := make([]cliSetupClient, 0, len(cliSetupTargets))
	for _, target := range cliSetupTargets {
		client := cliSetupClient{
			ID: target.id, Name: target.name, Provider: target.provider,
			Capability: "information_only", Stage: target.stage,
			ReasonCode: target.reason, NextStep: target.nextStep,
			Installation:  cliSetupInstallation{Evidence: "not_checked"},
			Configuration: cliSetupConfig{Condition: "not_checked", Scope: "not_inspected"},
			Accounts:      cliSetupAccounts{Evidence: "unknown"},
			Verification:  cliSetupVerification{Condition: "not_tested"},
		}
		if target.id == "codex" {
			client.Capability = "configure_user_file"
			client.Configuration = cliSetupConfig{
				Condition: codex.Condition, Scope: "inspected_user_file", ExpectedURL: codex.ExpectedURL,
				Ownership: codex.Ownership, InstallAction: codex.InstallAction,
				RestoreAction: codex.RestoreAction, PendingOperation: codex.PendingOperation,
			}
			client.Verification = a.codexProbeStatus()
		}
		if accountsKnown {
			count := counts[target.provider]
			client.Accounts = cliSetupAccounts{Evidence: "known", Count: &count}
		}
		clients = append(clients, client)
	}
	writeJSON(w, http.StatusOK, map[string]any{"codex": codex, "clients": clients})
}

func (a *API) handleCLISetupInstall(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	if err := codexcfg.InstallAt(a.cliSetupPath(), a.cliSetupPort()); err != nil {
		writeCodexSetupError(w, err)
		return
	}
	status := codexcfg.Check(a.cliSetupPath(), a.cliSetupPort())
	if status.Condition != "ready" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "Codex configuration did not verify", "codex": status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"codex": status})
}

func writeCodexSetupError(w http.ResponseWriter, err error) {
	code, message := http.StatusInternalServerError, "Could not update Codex configuration"
	switch {
	case errors.Is(err, codexcfg.ErrSelectionChanged):
		code, message = http.StatusConflict, "Codex selected another provider; reselect Switcher explicitly"
	case errors.Is(err, codexcfg.ErrLegacyOwnershipUnknown):
		code, message = http.StatusConflict, "Legacy Switcher setup has no recoverable previous selection"
	case errors.Is(err, codexcfg.ErrInvalidConfig):
		code, message = http.StatusConflict, "Fix the existing Codex configuration before configuring Switcher"
	case errors.Is(err, codexcfg.ErrConfigConflict):
		code, message = http.StatusConflict, "Codex configuration changed or cannot be safely edited"
	}
	writeJSON(w, code, map[string]string{"error": message})
}

func (a *API) codexSetupResult(w http.ResponseWriter, err error) {
	if err != nil {
		writeCodexSetupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"codex": codexcfg.Check(a.cliSetupPath(), a.cliSetupPort())})
}

func (a *API) handleCLISetupReselect(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	a.codexSetupResult(w, codexcfg.ReselectAt(a.cliSetupPath(), a.cliSetupPort()))
}

func (a *API) handleCLISetupRestore(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	a.codexSetupResult(w, codexcfg.Uninstall(a.cliSetupPath()))
}

func (a *API) handleCLISetupRemoveLegacy(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	a.codexSetupResult(w, codexcfg.UninstallLegacy(a.cliSetupPath()))
}

// LocalOnly guards the API against other websites: it rejects requests
// whose Host is not the local listener (DNS rebinding) and, on state
// changing methods, rejects cross-site browser requests via the Origin and
// Sec-Fetch-Site headers. The codex proxy path is intentionally exempt:
// the CLI sends no Origin header.
// LocalOptions configures the LocalOnly middleware: which port the
// listener serves and, when a LAN listener is active, the LAN host that
// counts as local.
type LocalOptions struct {
	Port    int
	LANHost string // bound LAN IP; empty means loopback-only
}

func LocalOnly(port int, next http.Handler) http.Handler {
	return LocalOnlyWith(LocalOptions{Port: port}, next)
}

func LocalOnlyWith(opts LocalOptions, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocalHost(r.Host, opts.LANHost) {
			http.Error(w, "switcher only accepts local connections", http.StatusForbidden)
			return
		}
		// Browser-initiated requests always carry Sec-Fetch-Site. The CLIs
		// send none, so their traffic stays exempt. A cross-site GET would
		// be unreadable cross-origin, but it would still spend the user's
		// subscription on an authenticated upstream call, so it is rejected.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site requests are not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && !isLocalOrigin(origin, opts) {
				http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLocalHost(host string, lanHost string) bool {
	host = strings.ToLower(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]" {
		return true
	}
	return lanHost != "" && host == strings.ToLower(lanHost)
}

// isLocalOrigin parses the origin and requires a loopback host on the
// server's own port; any-port localhost origins are rejected.
func isLocalOrigin(origin string, opts LocalOptions) bool {
	u, err := url.Parse(strings.ToLower(origin))
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		if opts.LANHost == "" || host != strings.ToLower(opts.LANHost) {
			return false
		}
	}
	portText := u.Port()
	return portText == strconv.Itoa(opts.Port)
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
	Health         proxy.Health      `json:"health"`
	ResetCredits   *resetCreditsView `json:"reset_credits,omitempty"`
}

// resetCreditsView surfaces banked usage-limit resets (codex).
type resetCreditsView struct {
	Count         int     `json:"count"`
	NextID        string  `json:"next_id,omitempty"`
	NextExpiresAt int64   `json:"next_expires_at,omitempty"`
	ExpiresAt     []int64 `json:"expires_at,omitempty"`
}

func (a *API) viewOf(acc store.Account) accountView {
	v := viewOfBase(a, acc)
	// Providers with banked resets report how many are available so the UI
	// can offer spending one. The upstream call is TTL-cached: /api/state
	// is polled every few seconds and must never make the request path
	// wait on chatgpt.com.
	if rc, ok := a.Providers[acc.Provider].(provider.ResetCreditProvider); ok {
		if credits, ok := a.cachedResetCredits(rc, acc); ok && len(credits) > 0 {
			expiries := make([]int64, 0, len(credits))
			for _, credit := range credits {
				if credit.ExpiresAt > 0 {
					expiries = append(expiries, credit.ExpiresAt)
				}
			}
			v.ResetCredits = &resetCreditsView{
				Count:         len(credits),
				NextID:        credits[0].ID,
				NextExpiresAt: credits[0].ExpiresAt,
				ExpiresAt:     expiries,
			}
		}
	}
	return v
}

// resetCreditsTTL is how long a banked-reset count stays fresh.
const resetCreditsTTL = 2 * time.Minute

// cachedResetCredits serves reset credits from cache and refreshes them in
// the background when stale. Errors keep the last known value.
func (a *API) cachedResetCredits(rc provider.ResetCreditProvider, acc store.Account) ([]provider.ResetCredit, bool) {
	a.creditsMu.Lock()
	entry, ok := a.creditsCache[acc.ID]
	fresh := ok && time.Since(entry.at) < resetCreditsTTL
	a.creditsMu.Unlock()
	if fresh {
		return entry.credits, entry.ok
	}
	go func() {
		credits, err := rc.ListResetCredits(context.Background(), acc)
		a.creditsMu.Lock()
		a.creditsCache[acc.ID] = creditsEntry{credits: credits, ok: err == nil, at: time.Now()}
		a.creditsMu.Unlock()
	}()
	return entry.credits, entry.ok
}

func viewOfBase(a *API, acc store.Account) accountView {
	v := accountView{
		ID:          acc.ID,
		Provider:    acc.Provider,
		Email:       acc.Email,
		Plan:        acc.Plan,
		Active:      acc.ID == a.Proxy.ActiveID(acc.Provider),
		LastRefresh: acc.LastRefresh,
		Health:      a.Proxy.AccountHealth(acc.ID),
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
	// The management key is only disclosed to cookie-authenticated
	// browsers (or when auth is off): a device token is the same trust
	// tier as the local files, so it earns the state but not the key.
	revealHubKey := AuthKind(r) != AuthDevice
	state := map[string]any{
		"active":              a.Proxy.ActiveAll(),
		"accounts":            views,
		"order":               order,
		"hidden":              hidden,
		"hub_url":             "http://127.0.0.1:8787",
		"version":             a.Version,
		"update":              a.UpdateState(),
		"menu_usage_bars":     a.Settings == nil || a.Settings.MenuUsageBars(),
		"reset_notifications": a.Settings != nil && a.Settings.Load().ResetNotifications,
		"compact_accounts":    a.Settings != nil && a.Settings.Load().CompactAccounts,
	}
	if revealHubKey {
		state["hub_management_key"] = a.ManagementKey
	}
	writeJSON(w, http.StatusOK, state)
}

// importer is the optional provider capability of reusing credentials the
// provider's own CLI already stores on this machine.
type importer interface {
	ImportFromKeychain(ctx context.Context) (store.Account, error)
}

// handleLoginImport creates an account from the provider CLI's locally
// stored credentials (Claude Code's keychain entry). Answers 501 when the
// provider has no importer and 409 when there is nothing to import, so the
// web UI can fall back to the browser flow.
func (a *API) handleLoginImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider  string `json:"provider"`
		ReloginOf string `json:"relogin_of"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	prov, ok := a.Providers[body.Provider]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	imp, ok := prov.(importer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "provider has no import path"})
		return
	}
	account, err := imp.ImportFromKeychain(r.Context())
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if body.ReloginOf != "" {
		if !validID(body.ReloginOf) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid relogin account id"})
			return
		}
	}
	if body.ReloginOf != "" {
		err = a.Proxy.ReplaceReloginAccount(&account, body.ReloginOf)
	} else {
		err = a.Proxy.ReplaceAccount(account)
	}
	if errors.Is(err, proxy.ErrReloginTargetUnavailable) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "relogin account not found"})
		return
	}
	if errors.Is(err, proxy.ErrReloginIdentityMismatch) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "CLI login belongs to a different account"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save account: " + err.Error()})
		return
	}
	if a.Proxy.ActiveID(account.Provider) == "" {
		_ = a.Proxy.Activate(account.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "account": viewOfBase(a, account)})
}

// registerAuthRoutes mounts the optional-authentication endpoints. Login,
// status, and credential operations enforce their own checks; logout and
// session deletion also pass through the cookie/CSRF auth gate.
func (a *API) registerAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/status", a.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", a.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleAuthLogout)
	mux.HandleFunc("DELETE /api/auth/sessions", a.handleAuthLogoutAll)
	mux.HandleFunc("POST /api/auth/password", a.handleAuthPassword)
	mux.HandleFunc("POST /api/auth/disable", a.handleAuthDisable)
	mux.HandleFunc("POST /api/auth/rotate-device-token", a.handleRotateDeviceToken)
	mux.HandleFunc("GET /api/settings", a.handleSettingsGet)
	mux.HandleFunc("PATCH /api/settings", a.handleSettingsPatch)
}

// loopbackOnly refuses requests that did not arrive on a loopback socket.
// The socket is the authority: a LAN client can spoof Host: 127.0.0.1, but
// it cannot spoof its source address. The Host check stays as a second gate.
func loopbackOnly(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	if !loopback || !isLocalHost(r.Host, "") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only local requests may change credentials"})
		return false
	}
	return true
}

func (a *API) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	st := a.Settings.Load()
	if !isLocalHost(r.Host, "") {
		// Pre-auth info disclosure: a LAN client only learns whether auth
		// is on and a password exists.
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_enabled": a.Settings.Enabled(),
			"password_set": a.Settings.HasPassword(),
		})
		return
	}
	response := map[string]any{
		"auth_enabled":        a.Settings.Enabled(),
		"password_set":        a.Settings.HasPassword(),
		"menu_usage_bars":     a.Settings.MenuUsageBars(),
		"reset_notifications": st.ResetNotifications,
		"compact_accounts":    st.CompactAccounts,
		"bind_lan":            st.BindLAN,
		"lan_active":          LANListenerActive(),
		"tls":                 st.TLS,
		"lan_ip":              LANAddress(),
		"sessions":            a.Settings.SessionCount(),
		"device_token_set":    a.Settings.HasDeviceToken(),
	}
	// A valid session cookie re-learns its CSRF token (localStorage was
	// cleared but the browser kept the cookie).
	if cookie, err := r.Cookie(sessionCookie); err == nil && a.Settings.ValidateSession(cookie.Value) {
		response["csrf"] = a.Settings.CSRFToken()
	}
	writeJSON(w, http.StatusOK, response)
}

// handleAuthLogin verifies the password (rate limited, constant time) and
// issues the session cookie plus the CSRF token for the web UI.
func (a *API) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := a.Settings.CheckLockout(); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	if !a.Settings.VerifyPassword(body.Password) {
		a.Settings.RecordFailure()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	a.Settings.ResetFailures()
	token, err := a.Settings.NewSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create session"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
		MaxAge:   int((24 * time.Hour * 7).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "csrf": a.Settings.CSRFToken()})
}

func (a *API) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		a.Settings.DeleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAuthLogoutAll clears every session: all browsers and devices are
// signed out, and the caller's cookie is expired too.
func (a *API) handleAuthLogoutAll(w http.ResponseWriter, r *http.Request) {
	if err := a.Settings.DeleteAllSessions(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleAuthPassword sets the first password or changes it; both are
// loopback-only when no password exists on disk yet (no TOFU from LAN).
func (a *API) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := a.Settings.CheckLockout(); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	if a.Settings.HasPassword() {
		if err := a.Settings.ChangePassword(body.Current, body.Next); err != nil {
			if strings.Contains(err.Error(), "wrong password") {
				// A wrong current password is a failed verification attempt.
				a.Settings.RecordFailure()
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		if err := a.Settings.SetPassword(body.Next); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	a.Settings.ResetFailures()
	// Ensure the device token file so the menu bar app can authenticate.
	_, _ = a.Settings.EnsureDeviceToken()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "csrf": a.Settings.CSRFToken()})
}

func (a *API) handleAuthDisable(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := a.Settings.CheckLockout(); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
		return
	}
	if !a.Settings.VerifyPassword(body.Password) {
		a.Settings.RecordFailure()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	a.Settings.ResetFailures()
	if err := a.Settings.DisableAuth(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Disabling auth tears down the LAN listener too: re-exec so the new
	// topology takes effect instead of leaving an unauthenticated LAN
	// listener up until the next restart.
	go func() {
		time.Sleep(300 * time.Millisecond)
		restartSelf()
	}()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleRotateDeviceToken replaces the local device token. It is
// loopback-only AND requires proof: a valid session (with CSRF) or the
// current device token itself.
func (a *API) handleRotateDeviceToken(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	authorized := false
	if cookie, err := r.Cookie(sessionCookie); err == nil && a.Settings.ValidateSession(cookie.Value) {
		if want := a.Settings.CSRFToken(); want != "" && subtleEqual(r.Header.Get(csrfHeader), want) {
			authorized = true
		}
	}
	if !authorized {
		if token, err := a.Settings.ReadDeviceToken(); err == nil && subtleEqual(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), token) {
			authorized = true
		}
	}
	if !authorized {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authenticate to rotate the device token"})
		return
	}
	if _, err := a.Settings.RotateDeviceToken(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleSettingsGet/Patch manage the network toggles. Enabling LAN requires
// authentication enabled AND a password on disk (no TOFU window), and the
// LAN listener is always TLS. bind_lan reports the ACTIVE listener state:
// a saved-but-not-yet-bound request shows as pending until the restart.
func (a *API) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	st := a.Settings.Load()
	lanActive := LANListenerActive()
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_enabled":        st.AuthEnabled,
		"bind_lan":            lanActive,
		"bind_pending":        st.BindLAN && !lanActive,
		"lan_active":          lanActive,
		"tls":                 st.TLS,
		"lan_ip":              LANAddress(),
		"password_set":        st.PasswordHash != "",
		"menu_usage_bars":     a.Settings.MenuUsageBars(),
		"reset_notifications": st.ResetNotifications,
		"compact_accounts":    st.CompactAccounts,
	})
}

func (a *API) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	var body struct {
		BindLAN            *bool `json:"bind_lan"`
		TLS                *bool `json:"tls"`
		MenuUsageBars      *bool `json:"menu_usage_bars"`
		ResetNotifications *bool `json:"reset_notifications"`
		CompactAccounts    *bool `json:"compact_accounts"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	var restartRequired bool
	err := a.Settings.Update(func(st *settings.Settings) error {
		bindLAN := st.BindLAN
		if body.BindLAN != nil {
			bindLAN = *body.BindLAN
		}
		if bindLAN && (!st.AuthEnabled || st.PasswordHash == "") {
			return errors.New("set a password before binding the LAN")
		}
		tlsEnabled := st.TLS
		if body.TLS != nil {
			tlsEnabled = *body.TLS
		}
		if bindLAN {
			tlsEnabled = true
		}
		restartRequired = st.BindLAN != bindLAN || st.TLS != tlsEnabled
		st.BindLAN, st.TLS = bindLAN, tlsEnabled
		if body.MenuUsageBars != nil {
			st.MenuUsageBars = body.MenuUsageBars
		}
		if body.ResetNotifications != nil {
			st.ResetNotifications = *body.ResetNotifications
		}
		if body.CompactAccounts != nil {
			st.CompactAccounts = *body.CompactAccounts
		}
		return nil
	})
	if err != nil && err.Error() == "set a password before binding the LAN" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "set a password before binding the LAN"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if restartRequired {
		// Network changes need new listeners; visual preferences do not.
		go func() {
			time.Sleep(300 * time.Millisecond)
			restartSelf()
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *API) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "expected application/json body", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		ReloginOf string `json:"relogin_of"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	prov, ok := a.Providers[body.Provider]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown provider"})
		return
	}
	handle, err := a.Logins.Start(r.Context(), prov, body.ReloginOf)
	if errors.Is(err, provider.ErrUnsupported) {
		// Providers without a browser callback (grok) use the device
		// authorization grant instead: also a web login, just with a code.
		handle, err = a.Logins.StartDevice(context.WithoutCancel(r.Context()), prov, body.ReloginOf)
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
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
	if err := a.Proxy.ReplaceAccount(account); err != nil {
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
		if errors.Is(err, proxy.ErrReloginIdentityMismatch) {
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "failed", "error": "Signed in to a different account. Use the selected account to relogin.",
			})
			return
		}
		if errors.Is(err, proxy.ErrReloginTargetUnavailable) {
			writeJSON(w, http.StatusOK, map[string]string{
				"status": "failed", "error": "The selected account no longer exists.",
			})
			return
		}
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

// handleUsage answers with the cost/token summary for a window
// (?days=1|7|30|90). It never scans inline: the latest summary is served
// even when stale, and rescans happen in the background.
func (a *API) handleUsage(w http.ResponseWriter, r *http.Request) {
	days := 30
	switch r.URL.Query().Get("days") {
	case "1", "24h":
		days = 1
	case "7":
		days = 7
	case "90":
		days = 90
	}
	if a.Usage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "usage not configured"})
		return
	}
	summary := a.Usage.Get(days)
	if summary == nil {
		go a.Usage.Scan(days)
		writeJSON(w, http.StatusAccepted, map[string]any{"scanning": true, "days": days})
		return
	}
	stale := a.Usage.Age(days) > 10*time.Minute
	if stale {
		go a.Usage.Scan(days)
	}
	writeJSON(w, http.StatusOK, map[string]any{"scanning": false, "stale": stale, "days": days, "summary": summary})
}

// handleUsageRefresh forces a rescan of every window.
func (a *API) handleUsageRefresh(w http.ResponseWriter, r *http.Request) {
	if a.Usage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "usage not configured"})
		return
	}
	go a.Usage.Scan(30)
	go a.Usage.Scan(90)
	go a.Usage.Scan(7)
	go a.Usage.Scan(1)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleRefreshAll re-queries usage for every account (manual refresh from
// the menu bar or web app) and answers with the full state.
func (a *API) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	a.Proxy.RefreshUsageAll(r.Context())
	a.handleState(w, r)
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "usage": usage, "health": a.Proxy.AccountHealth(id)})
}

// handleRecheckAccount queues a coalesced usage check. The existing state
// poll delivers the result without holding an HTTP request through OAuth.
func (a *API) handleRecheckAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid account id"})
		return
	}
	if err := a.Proxy.QueueRecheck(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such account"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not queue account check"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "checking", "health": a.Proxy.AccountHealth(id)})
}

func (a *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "invalid account id", http.StatusBadRequest)
		return
	}
	if err := a.Proxy.DeleteAccount(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete account"})
		return
	}
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&body); err != nil {
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

// restartForTest, when set, replaces restartSelf in tests so the exec
// restart never fires against the test binary.
var restartForTest func()

// restartSelf re-executes the running binary, preserving argv and env. Used
// after settings changes that affect the listener topology.
func restartSelf() {
	if restartForTest != nil {
		restartForTest()
		return
	}
	current, err := os.Executable()
	if err != nil {
		return
	}
	if current, err = filepath.EvalSymlinks(current); err != nil {
		return
	}
	log.Printf("settings: restarting in place for the new listener topology")
	time.Sleep(200 * time.Millisecond)
	_ = syscall.Exec(current, os.Args, os.Environ())
}
