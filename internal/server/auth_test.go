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
	"time"

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
	// The hub is registered by main.go (mgmtapi.Register), not by this
	// harness; its management-key gate is covered by the gate order tests
	// and unchanged by the auth work.
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

func TestMenuUsageBarsSettingDoesNotRestartOrChangeNetwork(t *testing.T) {
	store := settings.New(t.TempDir())
	st := store.Load()
	st.AuthEnabled = true
	st.PasswordHash = "existing-password-hash"
	st.BindLAN = true
	st.TLS = true
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	a := &API{Settings: store}
	restarted := false
	previous := restartForTest
	restartForTest = func() { restarted = true }
	defer func() { restartForTest = previous }()

	patch := httptest.NewRequest(http.MethodPatch, "/api/settings",
		bytes.NewBufferString(`{"menu_usage_bars":false}`))
	patch.RemoteAddr = "127.0.0.1:1234"
	patch.Host = "127.0.0.1:8787"
	w := httptest.NewRecorder()
	a.handleSettingsPatch(w, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("patch status %d: %s", w.Code, w.Body.String())
	}
	if store.MenuUsageBars() || !store.Load().BindLAN || !store.Load().TLS {
		t.Fatal("visual preference failed or modified network settings")
	}
	if restarted {
		t.Fatal("a visual preference restarted the server")
	}
	get := httptest.NewRecorder()
	a.handleSettingsGet(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var response struct {
		MenuUsageBars bool `json:"menu_usage_bars"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.MenuUsageBars {
		t.Fatal("Settings GET did not reflect the saved value")
	}
}

func TestResetNotificationsSettingDoesNotRestartOrOverwriteUsageBars(t *testing.T) {
	store := settings.New(t.TempDir())
	a := &API{Settings: store}
	previous := restartForTest
	restarted := false
	restartForTest = func() { restarted = true }
	defer func() { restartForTest = previous }()
	for _, payload := range []string{`{"menu_usage_bars":false}`, `{"reset_notifications":true}`} {
		request := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewBufferString(payload))
		request.RemoteAddr = "127.0.0.1:1234"
		request.Host = "127.0.0.1:8787"
		response := httptest.NewRecorder()
		a.handleSettingsPatch(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d %s", payload, response.Code, response.Body.String())
		}
	}
	if !store.Load().ResetNotifications || store.MenuUsageBars() || restarted {
		t.Fatal("notification setting lost the usage bar preference or restarted listeners")
	}
	response := httptest.NewRecorder()
	a.handleSettingsGet(response, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var body struct {
		ResetNotifications bool `json:"reset_notifications"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.ResetNotifications {
		t.Fatalf("GET settings did not report reset notifications: %v", err)
	}
}

func TestCLISetupChecksAndConnectsOnlyLocally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "config.toml")
	a := &API{CodexConfigPath: path, Port: 9123}
	mux := http.NewServeMux()
	a.Register(mux)
	request := func(method, remote string) *httptest.ResponseRecorder {
		t.Helper()
		url := "/api/cli-setup"
		if method == http.MethodPost {
			url += "/codex/install"
		}
		r := httptest.NewRequest(method, url, nil)
		r.Host, r.RemoteAddr = "127.0.0.1:9123", remote
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodPost, "192.168.1.20:1234"); w.Code != http.StatusForbidden {
		t.Fatalf("LAN setup = %d, want 403", w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("LAN request wrote config")
	}
	if w := request(http.MethodGet, "127.0.0.1:1234"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"condition":"missing"`) {
		t.Fatalf("initial check: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, "127.0.0.1:1234"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"condition":"ready"`) {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "127.0.0.1:9123/codex/v1") {
		t.Fatalf("custom port config: %v", err)
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

// loopbackHarness builds an API with a password on disk and no listeners:
// requests are driven directly against the mux so RemoteAddr can be set
// per request, exactly like the real listeners would.
func loopbackHarness(t *testing.T) (*settings.Store, *http.ServeMux) {
	t.Helper()
	store := settings.New(t.TempDir())
	if err := store.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(mux)
	return store, mux
}

func TestLoopbackOnlyRejectsSpoofedHost(t *testing.T) {
	_, mux := loopbackHarness(t)
	// httptest.NewRequest sets RemoteAddr to 192.0.2.1:1234 (a LAN-class
	// address): a spoofed Host: 127.0.0.1 must not buy loopback access.
	body := bytes.NewBufferString(`{"current":"hunter22","next":"newerpass99"}`)
	req := httptest.NewRequest("POST", "/api/auth/password", body)
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusForbidden {
		t.Fatalf("spoofed Host from a LAN socket: %d, want 403", res.Code)
	}
}

func TestLoopbackOnlyAcceptsLoopbackSocket(t *testing.T) {
	store, mux := loopbackHarness(t)
	body := bytes.NewBufferString(`{"current":"hunter22","next":"newerpass99"}`)
	req := httptest.NewRequest("POST", "/api/auth/password", body)
	req.RemoteAddr = "127.0.0.1:9999"
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("loopback socket password change: %d, want 200", res.Code)
	}
	if !store.VerifyPassword("newerpass99") {
		t.Fatal("password change did not take effect")
	}
}

func TestLoopbackOnlyAcceptsIPv6LoopbackSocket(t *testing.T) {
	_, mux := loopbackHarness(t)
	body := bytes.NewBufferString(`{"current":"hunter22","next":"newerpass99"}`)
	req := httptest.NewRequest("POST", "/api/auth/password", body)
	req.RemoteAddr = "[::1]:52341"
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("IPv6 loopback socket password change: %d, want 200", res.Code)
	}
}

func TestPasswordChangeRateLimited(t *testing.T) {
	_, mux := loopbackHarness(t)
	for i := 0; i < 5; i++ {
		body := bytes.NewBufferString(`{"current":"totallywrong","next":"newerpass99"}`)
		req := httptest.NewRequest("POST", "/api/auth/password", body)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = "127.0.0.1:8787"
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i, res.Code)
		}
	}
	// Five wrong currents trip the lockout: the next attempt is refused
	// with 429 before any verification work, even with the right password.
	body := bytes.NewBufferString(`{"current":"hunter22","next":"newerpass99"}`)
	req := httptest.NewRequest("POST", "/api/auth/password", body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("locked-out password change: %d, want 429", res.Code)
	}
}

func TestDisableAuthClearsSessionsAndRestarts(t *testing.T) {
	store, _ := loopbackHarness(t)
	token, err := store.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	restarted := make(chan struct{}, 1)
	original := restartForTest
	restartForTest = func() { restarted <- struct{}{} }
	defer func() { restartForTest = original }()

	mux := http.NewServeMux()
	api := &API{Settings: store}
	api.registerAuthRoutes(mux)
	body := bytes.NewBufferString(`{"password":"hunter22"}`)
	req := httptest.NewRequest("POST", "/api/auth/disable", body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("disable with the right password: %d, want 200", res.Code)
	}
	if store.Enabled() || store.HasPassword() {
		t.Fatal("auth still on after disable")
	}
	if store.ValidateSession(token) {
		t.Fatal("session survived disable")
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("disable did not schedule a restart of the listeners")
	}
}

func TestDisableAuthWrongPasswordRecordsFailure(t *testing.T) {
	store, mux := loopbackHarness(t)
	restartForTest = func() {}
	defer func() { restartForTest = nil }()
	for i := 0; i < 6; i++ {
		body := bytes.NewBufferString(`{"password":"totallywrong"}`)
		req := httptest.NewRequest("POST", "/api/auth/disable", body)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Host = "127.0.0.1:8787"
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if i < 5 && res.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i, res.Code)
		}
		if i == 5 && res.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status %d, want 429 after lockout", i, res.Code)
		}
	}
	if !store.Enabled() || !store.HasPassword() {
		t.Fatal("disable went through without the correct password")
	}
}

func TestLogoutEverywhereClearsAllSessions(t *testing.T) {
	store, mux := loopbackHarness(t)
	tokens := make([]string, 2)
	for i := range tokens {
		var err error
		tokens[i], err = store.NewSession()
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("DELETE", "/api/auth/sessions", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Host = "127.0.0.1:8787"
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("logout everywhere: %d, want 200", res.Code)
	}
	for _, token := range tokens {
		if store.ValidateSession(token) {
			t.Fatal("a session survived logout everywhere")
		}
	}
	if n := store.SessionCount(); n != 0 {
		t.Fatalf("sessions remain after logout everywhere: %d", n)
	}
}
