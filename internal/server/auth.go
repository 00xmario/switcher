package server

import (
	"context"
	"net/http"
	"strings"

	"switcher/internal/settings"
)

// authGate is the optional authentication middleware. When auth is
// disabled it passes everything through unchanged. When enabled, neither
// the app shell nor its assets are served without a session. Login assets
// and the independently authenticated CLI proxy/hub routes remain reachable.
type authGate struct {
	store                *settings.Store
	lan                  bool
	desktopManagementKey string
}

type ctxKey string

const authKindKey ctxKey = "switcher.auth.kind"

// AuthKind marks how a request authenticated.
const (
	AuthCookie = "cookie"
	AuthDevice = "device"
)

const sessionCookie = "switcher_session"
const csrfHeader = "X-Switcher-CSRF"

// gate is the middleware.
func (a *authGate) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st, err := a.store.Snapshot()
		// A bound LAN socket never inherits the local no-auth default. This
		// also closes access immediately while a topology restart is pending
		// or has failed, including independently authenticated native routes.
		if a.lan && (err != nil || !st.AuthEnabled || st.PasswordHash == "" || !st.BindLAN || !st.TLS) {
			http.Error(w, "LAN access is disabled", http.StatusServiceUnavailable)
			return
		}
		if nativeRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if desktopRelayRoute(r.URL.Path) && !desktopRelayLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "desktop relay controls require a local connection"})
			return
		}
		if err != nil {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "authentication settings unavailable", http.StatusServiceUnavailable)
			return
		}
		if !st.AuthEnabled {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Device token: Bearer auth is CSRF-immune (no ambient cookie).
		if token, err := a.store.ReadDeviceToken(); err == nil && token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != "" && subtleEqual(got, token) {
				ctx := context.WithValue(r.Context(), authKindKey, AuthDevice)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if cookie, err := r.Cookie(sessionCookie); err == nil && (a.store.ValidateSession(cookie.Value) ||
			(r.Method == http.MethodPost && r.URL.Path == "/api/auth/logout" && a.store.CanRetrySessionDeletion(cookie.Value))) {
			// Cookie-authenticated state-changing requests must carry the
			// CSRF token; a Bearer device token is already CSRF-immune.
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				want := a.store.CSRFToken()
				got := r.Header.Get(csrfHeader)
				if want == "" || got == "" || !subtleEqual(got, want) {
					http.Error(w, "missing csrf token", http.StatusForbidden)
					return
				}
			}
			ctx := context.WithValue(r.Context(), authKindKey, AuthCookie)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		// Only the Desktop relay accepts this independent, non-cookie key.
		// Check it after cookies so a cookie-authenticated mutation retains CSRF.
		if desktopRelayRoute(r.URL.Path) && desktopRelayLocalRequest(r) {
			keys := r.Header.Values(desktopControlHeader)
			if len(keys) == 1 && desktopControlKeyMatches(keys[0], a.desktopManagementKey) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="switcher"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"auth_required": true})
	})
}

// exempt lets the standalone login load without shipping the app's HTML or
// JavaScript. Credential endpoints that require a password or device token
// retain their per-handler checks. Logout and session deletion go through
// the gate, including its cookie CSRF check.
func exempt(r *http.Request) bool {
	path := r.URL.Path
	switch path {
	case "/api/auth/status":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case "/api/auth/login", "/api/auth/password", "/api/auth/disable", "/api/auth/rotate-device-token":
		return r.Method == http.MethodPost
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		switch path {
		case "/login", "/login.js", "/login.css":
			return true
		}
	}
	return false
}

// Native routes have their own provider or management-key authentication.
// Changing the browser gate must not make CLI traffic require a cookie.
func nativeRoute(path string) bool {
	switch path {
	case "/v0/management/auth-files", "/v0/management/api-call", "/v0/management/reset-quota":
		return true
	}
	for _, provider := range []string{"codex", "claude", "grok", "opencode", "antigravity", "gemini", "copilot"} {
		if strings.HasPrefix(path, "/"+provider+"/") {
			return true
		}
	}
	return false
}

// AuthKind reports how the request authenticated ("", "cookie", or "device").
func AuthKind(r *http.Request) string {
	kind, _ := r.Context().Value(authKindKey).(string)
	return kind
}

func subtleEqual(a, b string) bool {
	return len(a) == len(b) && (len(a) == 0 || func() bool {
		var acc byte
		for i := 0; i < len(a); i++ {
			acc |= a[i] ^ b[i]
		}
		return acc == 0
	}())
}

// AuthGate wraps the mux behind the optional authentication middleware.
// With auth disabled it is a pass-through, keeping the historic behavior.
type AuthGate struct {
	Store                *settings.Store
	DesktopManagementKey string
}

// Wrap returns next behind the auth gate.
func (g *AuthGate) Wrap(next http.Handler) http.Handler {
	if g == nil || g.Store == nil {
		return next
	}
	gate := &authGate{store: g.Store, desktopManagementKey: g.DesktopManagementKey}
	return gate.gate(next)
}

// WrapLAN keeps the LAN listener closed whenever its saved security and
// network prerequisites are no longer valid, without waiting for re-exec.
func (g *AuthGate) WrapLAN(next http.Handler) http.Handler {
	if g == nil || g.Store == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "LAN authentication unavailable", http.StatusServiceUnavailable)
		})
	}
	return (&authGate{store: g.Store, lan: true, desktopManagementKey: g.DesktopManagementKey}).gate(next)
}
