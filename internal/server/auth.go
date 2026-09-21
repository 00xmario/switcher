package server

import (
	"context"
	"net/http"
	"strings"

	"switcher/internal/settings"
)

// authGate is the optional authentication middleware. When auth is
// disabled it passes everything through unchanged, so the historic
// behavior is bit-identical. When enabled it requires a browser session
// cookie or the local device token on every /api and static request,
// except the paths listed below.
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
		if !a.store.Enabled() || exempt(r) {
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
		w.Header().Set("WWW-Authenticate", `Bearer realm="switcher"`)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"auth_required": true})
	})
}

// exempt reports whether this request never requires authentication.
// The gate guards only /api/*: the static UI must load so the login view
// can render, and the CLI proxy + hub paths have their own story.
func exempt(r *http.Request) bool {
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/auth/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return true
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
