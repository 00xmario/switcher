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
	store *settings.Store
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
		if !a.store.Enabled() || nativeRoute(r.URL.Path) {
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
		if cookie, err := r.Cookie(sessionCookie); err == nil && a.store.ValidateSession(cookie.Value) {
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
	Store *settings.Store
}

// Wrap returns next behind the auth gate.
func (g *AuthGate) Wrap(next http.Handler) http.Handler {
	if g == nil || g.Store == nil {
		return next
	}
	gate := &authGate{store: g.Store}
	return gate.gate(next)
}
