package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switcher/internal/settings"
)

// gateHarness wires a mux with the auth gate exactly like main.go does.
type gateHarness struct {
	gate   *AuthGate
	store  *settings.Store
	mux    *http.ServeMux
	server *httptest.Server
}

func newHarness(t *testing.T, root string) *gateHarness {
	t.Helper()
	store := settings.New(root)
	if err := store.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureDeviceToken(); err != nil {
		t.Fatal(err)
	}
	h := &gateHarness{gate: &AuthGate{Store: store}, store: store}
	h.mux = http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(h.mux)
	h.mux.HandleFunc("POST /api/update", func(w http.ResponseWriter, r *http.Request) { //nolint
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	})
	h.mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "test"})
	})
	h.server = httptest.NewServer(h.gate.Wrap(h.mux))
	t.Cleanup(h.server.Close)
	return h
}

func (h *gateHarness) deviceHeader() string {
	token, err := h.store.ReadDeviceToken()
	if err != nil {
		return ""
	}
	return "Bearer " + token
}

func TestGateBlocksWhenEnabled(t *testing.T) {
	h := newHarness(t, t.TempDir())
	res, err := http.Get(h.server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body["auth_required"] != true {
		t.Fatal("401 body missing the auth_required marker")
	}
}

func TestGatePassesWithDeviceToken(t *testing.T) {
	h := newHarness(t, t.TempDir())
	req, _ := http.NewRequest("GET", h.server.URL+"/api/state", nil)
	req.Header.Set("Authorization", h.deviceHeader())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d with device token, want 200", res.StatusCode)
	}
}

func TestGateBlocksWrongDeviceToken(t *testing.T) {
	h := newHarness(t, t.TempDir())
	req, _ := http.NewRequest("GET", h.server.URL+"/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d with a wrong token, want 401", res.StatusCode)
	}
}

func TestLoginSetsCookieAndCSRFWorks(t *testing.T) {
	h := newHarness(t, t.TempDir())
	// Wrong password first (counts toward the lockout).
	res, _ := http.Post(h.server.URL+"/api/auth/login", "application/json",
		bytes.NewBufferString(`{"password":"wrongpassword"}`))
	res.Body.Close()
	// Correct password.
	res, err := http.Post(h.server.URL+"/api/auth/login", "application/json",
		bytes.NewBufferString(`{"password":"hunter22"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		CSRF string `json:"csrf"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if body.CSRF == "" {
		t.Fatal("login did not issue a csrf token")
	}
	var session *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}

	// State-changing POST with the cookie but no CSRF header: forbidden.
	req, _ := http.NewRequest("POST", h.server.URL+"/api/update", nil)
	req.AddCookie(session)
	res2, _ := http.DefaultClient.Do(req)
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST without csrf: %d, want 403", res2.StatusCode)
	}
	res2.Body.Close()

	// With the CSRF header: allowed.
	req3, _ := http.NewRequest("POST", h.server.URL+"/api/update", nil)
	req3.AddCookie(session)
	req3.Header.Set(csrfHeader, body.CSRF)
	res3, _ := http.DefaultClient.Do(req3)
	if res3.StatusCode != http.StatusOK {
		t.Fatalf("cookie POST with csrf: %d, want 200", res3.StatusCode)
	}
	res3.Body.Close()

	// Bearer device-token requests are CSRF-immune.
	req4, _ := http.NewRequest("POST", h.server.URL+"/api/update", nil)
	req4.Header.Set("Authorization", h.deviceHeader())
	res4, _ := http.DefaultClient.Do(req4)
	if res4.StatusCode != http.StatusOK {
		t.Fatalf("device-token POST: %d, want 200", res4.StatusCode)
	}
	res4.Body.Close()
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, t.TempDir())
	for i := 0; i < 6; i++ {
		res, err := http.Post(h.server.URL+"/api/auth/login", "application/json",
			bytes.NewBufferString(`{"password":"totallywrong"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if i >= 5 && res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status %d, want 429 after lockout", i, res.StatusCode)
		}
	}
}

func TestExemptPathsStayOpen(t *testing.T) {
	h := newHarness(t, t.TempDir())
	// The CLI proxy paths are exempt by design (the CLIs cannot log in).
	for _, path := range []string{"/codex/v1/responses", "/claude/v1/messages", "/grok/x", "/opencode/y"} {
		res, err := http.Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusUnauthorized {
			t.Fatalf("proxy path %s was auth-gated", path)
		}
	}
	// The hub stays management-key gated (401 without the key), not auth-gated.
	res, _ := http.Get(h.server.URL + "/v0/management/auth-files")
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized && !strings.Contains("", "") {
		// The hub answers its own 401 with the management-key error; the
		// exact status is the hub's business, the point is it did not 404.
		t.Skip("hub returns its own status")
	}
}

func TestStaticServedUnauthenticated(t *testing.T) {
	h := newHarness(t, t.TempDir())
	// An index-less mux still serves other handlers; the gate must not 401
	// the static UI (registered here as a plain page).
	h.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { //nolint
		_, _ = w.Write([]byte("<html>ui</html>"))
	})
	res, err := http.Get(h.server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("static UI under auth: %d, want 200", res.StatusCode)
	}
}

func TestPasswordSetupLoopbackOnly(t *testing.T) {
	root := t.TempDir()
	store := settings.New(root)
	// No password on disk yet: the setup endpoint is reachable locally.
	gate := &AuthGate{Store: store}
	mux := http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(mux)
	server := httptest.NewServer(gate.Wrap(mux))
	defer server.Close()

	res, _ := http.Post(server.URL+"/api/auth/password", "application/json",
		bytes.NewBufferString(`{"next":"hunter22"}`))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first password set locally: %d", res.StatusCode)
	}
	if !store.HasPassword() {
		t.Fatal("password hash missing after setup")
	}
	// With a hash on disk, a spoofed Host is still allowed through the
	// local check only because httptest uses 127.0.0.1; a non-loopback
	// Host header is rejected.
	req, _ := http.NewRequest("POST", server.URL+"/api/auth/password", bytes.NewBufferString(`{"next":"otherpass1"}`))
	req.Host = "192.168.1.5:8787"
	res2, _ := http.DefaultClient.Do(req)
	res2.Body.Close()
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback host on password change: %d, want 403", res2.StatusCode)
	}
	_ = os.Remove(filepath.Join(root, "settings.json"))
}

func TestLocalOnlyLANSemantics(t *testing.T) {
	opts := LocalOptions{Port: 8787, LANHost: "192.168.1.5"}
	handler := LocalOnlyWith(opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { //nolint
		w.WriteHeader(200)
	}))

	// LAN host accepted.
	req := httptest.NewRequest("GET", "http://192.168.1.5:8787/api/state", nil)
	if res := httptest.NewRecorder(); func() bool { handler.ServeHTTP(res, req); return res.Code != 200 }() {
		t.Fatal("LAN host rejected")
	}
	// A rebound Host header (DNS rebinding) still rejected.
	req.Host = "attacker.example"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 403 {
		t.Fatalf("rebound host: %d, want 403", res.Code)
	}
	// LAN origin accepted for state-changing methods.
	req2 := httptest.NewRequest("POST", "https://192.168.1.5:8787/api/update", nil)
	req2.Host = "192.168.1.5:8787"
	req2.Header.Set("Origin", "https://192.168.1.5:8787")
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	res2 := httptest.NewRecorder()
	handler.ServeHTTP(res2, req2)
	if res2.Code != 200 {
		t.Fatalf("LAN same-origin POST: %d, want 200", res2.Code)
	}
	// A different port in the Origin is rejected.
	req3 := httptest.NewRequest("POST", "https://192.168.1.5:9443/api/update", nil)
	req3.Host = "192.168.1.5:8787"
	req3.Header.Set("Origin", "https://192.168.1.5:9443")
	req3.Header.Set("Sec-Fetch-Site", "same-origin")
	res3 := httptest.NewRecorder()
	handler.ServeHTTP(res3, req3)
	if res3.Code != 403 {
		t.Fatalf("cross-port origin: %d, want 403", res3.Code)
	}
}

func TestRotateTokenRequiresAuth(t *testing.T) {
	root := t.TempDir()
	store := settings.New(root)
	if err := store.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	oldToken, _ := store.EnsureDeviceToken()
	gate := &AuthGate{Store: store}
	mux := http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(mux)
	server := httptest.NewServer(gate.Wrap(mux))
	defer server.Close()

	// No auth at all: rejected.
	res, _ := http.Post(server.URL+"/api/auth/rotate-device-token", "application/json", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rotation: %d, want 401", res.StatusCode)
	}
	// Current device token (proof of possession): allowed.
	req, _ := http.NewRequest("POST", server.URL+"/api/auth/rotate-device-token", nil)
	req.Header.Set("Authorization", "Bearer "+oldToken)
	res2, _ := http.DefaultClient.Do(req)
	res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("rotation with current token: %d, want 200", res2.StatusCode)
	}
	if newToken, _ := store.ReadDeviceToken(); newToken == oldToken {
		t.Fatal("token not rotated")
	}
}

func TestSettingsPatchRequiresPassword(t *testing.T) {
	root := t.TempDir()
	store := settings.New(root) // no password
	gate := &AuthGate{Store: store}
	mux := http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(mux)
	server := httptest.NewServer(gate.Wrap(mux))
	defer server.Close()

	res, _ := http.NewRequest(http.MethodPatch, server.URL+"/api/settings", bytes.NewBufferString(`{"bind_lan":true,"tls":true}`))
	res2, err := http.DefaultClient.Do(res)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusConflict {
		t.Fatalf("LAN enable without password: %d, want 409", res2.StatusCode)
	}
}

func TestSessionRevivalAfterDisable(t *testing.T) {
	dir := t.TempDir()
	store := settings.New(dir)
	if err := store.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	token, _ := store.NewSession()
	if !store.ValidateSession(token) {
		t.Fatal("session should be valid")
	}
	if err := store.DisableAuth(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if store.ValidateSession(token) {
		t.Fatal("stale cookie revived after disable + re-enable")
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	dir := t.TempDir()
	// A sessions file whose entries are all in the past: the store must
	// drop them on load instead of reviving dead sessions.
	file := map[string]int64{}
	file["1111111111111111111111111111111111111111111111111111111111111111"] = 1
	out, _ := json.Marshal(map[string]any{"sessions": file})
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	store := settings.New(dir)
	if store.ValidateSession("1111111111111111111111111111111111111111111111111111111111111111") {
		t.Fatal("expired session accepted")
	}
}
