package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
)

func TestCodexRouteTestStoresOnlyScopedProxyEvidence(t *testing.T) {
	st := store.New(t.TempDir())
	for _, id := range []string{"codex-one", "codex-two"} {
		if err := st.Save(store.Account{ID: id, Provider: "codex", Email: id + "@example.test",
			Token: store.Token{AccessToken: "private-token-" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := proxy.New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("codex-one"); err != nil {
		t.Fatal(err)
	}
	preferences := settings.New(t.TempDir())
	a := &API{Store: st, Proxy: m, Settings: preferences, Version: "test-build", Port: 9123,
		CodexConfigPath: filepath.Join(t.TempDir(), "config.toml")}
	var calls atomic.Int32
	a.probeCodexForTest = func(context.Context) proxy.ProbeCodexResult {
		calls.Add(1)
		fingerprint, id := m.ProbeFingerprint()
		return proxy.ProbeCodexResult{Outcome: "success", Method: "switcher_proxy", Model: proxy.CodexProbeModel,
			Route: proxy.CodexProbeRoute, AccountID: id, Fingerprint: fingerprint}
	}
	mux := http.NewServeMux()
	a.Register(mux)
	test := setupRequest(http.MethodPost, "/api/cli-setup/codex/test", "127.0.0.1:1234")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, test)
	if w.Code != http.StatusOK || calls.Load() != 1 || !strings.Contains(w.Body.String(), `"condition":"last_success"`) ||
		strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), `"fingerprint"`) {
		t.Fatalf("route test status or privacy: %d %s", w.Code, w.Body.String())
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(preferences.Path()), "cli-probe.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved verification not private: %v", err)
	}
	get := httptest.NewRecorder()
	mux.ServeHTTP(get, setupRequest(http.MethodGet, "/api/cli-setup", "127.0.0.1:1234"))
	var body struct {
		Clients []cliSetupClient `json:"clients"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &body); err != nil || body.Clients[0].Verification.Condition != "last_success" || calls.Load() != 1 {
		t.Fatalf("GET re-tested or lost evidence: %v %+v", err, body)
	}
	a.Port = 8787
	if status := a.codexProbeStatus(); status.Condition != "historical" {
		t.Fatalf("port drift retained current proof: %+v", status)
	}
	a.Port = 9123
	a.Version = "new-build"
	if status := a.codexProbeStatus(); status.Condition != "historical" {
		t.Fatalf("build drift retained current proof: %+v", status)
	}
	a.Version = "test-build"
	if err := m.Activate("codex-two"); err != nil {
		t.Fatal(err)
	}
	if status := a.codexProbeStatus(); status.Condition != "historical" {
		t.Fatalf("account change retained current proof: %+v", status)
	}
	newManager, err := proxy.New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Proxy = newManager
	if status := a.codexProbeStatus(); status.Condition != "historical" {
		t.Fatalf("restart retained current proof: %+v", status)
	}
	invalid := httptest.NewRequest(http.MethodPost, "/api/cli-setup/codex/test", strings.NewReader(`{"model":"arbitrary"}`))
	invalid.Host, invalid.RemoteAddr = "127.0.0.1:9123", "127.0.0.1:1234"
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, invalid)
	if w.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("caller-supplied model was accepted: %d", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, setupRequest(http.MethodPost, "/api/cli-setup/codex/test", "192.168.1.20:1234"))
	if w.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("LAN route test was accepted: %d", w.Code)
	}
}

func TestCodexRouteTestKeepsAuthAndCSRF(t *testing.T) {
	preferences := settings.New(t.TempDir())
	if err := preferences.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	session, err := preferences.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(t.TempDir())
	m, err := proxy.New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Settings: preferences, Store: st, Proxy: m, CodexConfigPath: filepath.Join(t.TempDir(), "config.toml")}
	mux := http.NewServeMux()
	a.Register(mux)
	secured := (&AuthGate{Store: preferences}).Wrap(mux)
	run := func(cookie, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := setupRequest(http.MethodPost, "/api/cli-setup/codex/test", "127.0.0.1:1234")
		if cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if csrf {
			r.Header.Set(csrfHeader, preferences.CSRFToken())
		}
		w := httptest.NewRecorder()
		secured.ServeHTTP(w, r)
		return w
	}
	if got := run(false, false).Code; got != http.StatusUnauthorized {
		t.Fatalf("no session: %d", got)
	}
	if got := run(true, false).Code; got != http.StatusForbidden {
		t.Fatalf("no csrf: %d", got)
	}
	if w := run(true, true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"no_active_account"`) {
		t.Fatalf("authorized test did not fail safely without an active account: %d %s", w.Code, w.Body.String())
	}
}
