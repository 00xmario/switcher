package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/claudesync"
	"switcher/internal/desktoprelay"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
)

type setupForbiddenSource struct{ prepare, refresh atomic.Int64 }

func (s *setupForbiddenSource) Prepare(context.Context, string) (desktoprelay.Credential, error) {
	s.prepare.Add(1)
	return desktoprelay.Credential{}, errors.New("fixture-provider-secret")
}

func (s *setupForbiddenSource) RefreshRejected(context.Context, string, string) (desktoprelay.Credential, error) {
	s.refresh.Add(1)
	return desktoprelay.Credential{}, errors.New("fixture-provider-secret")
}

type desktopSetupFixture struct {
	api     *API
	mux     *http.ServeMux
	source  *setupForbiddenSource
	root    string
	restart atomic.Int64
	sync    atomic.Int64
}

func newDesktopSetupFixture(t *testing.T) *desktopSetupFixture {
	t.Helper()
	blockDesktopControlEgress(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "unused-env-config"))
	f := &desktopSetupFixture{root: root, source: &setupForbiddenSource{}, mux: http.NewServeMux()}
	m, err := desktoprelay.New(desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Port: 0,
		Source: f.source, Transport: desktopControlBlockedTransport{},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture forbids tunnel egress")
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	accounts := store.New(filepath.Join(root, "accounts"))
	pm, err := proxy.New(accounts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.api = &API{DesktopRelay: m, DesktopSettingsPath: filepath.Join(root, "settings.json"),
		ManagementKey: "fixture-management-secret", Store: accounts, Proxy: pm,
		Settings:              settings.New(filepath.Join(root, "switcher-settings")),
		CodexConfigPath:       filepath.Join(root, "codex.toml"),
		RestartDesktopForTest: func(context.Context) error { f.restart.Add(1); return nil },
		syncClaudeForTest: func(context.Context, map[string]string) (claudesync.Result, error) {
			f.sync.Add(1)
			return claudesync.Result{}, nil
		}}
	f.api.Register(f.mux)
	t.Cleanup(func() {
		if f.source.prepare.Load() != 0 || f.source.refresh.Load() != 0 {
			t.Error("setup or restart invoked credential preparation or provider refresh")
		}
	})
	return f
}

func (f *desktopSetupFixture) request(t *testing.T, method, suffix, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := desktopControlRequest(t, f.mux, method, "/api/desktop-relay"+suffix, body, f.api.ManagementKey)
	if w.Code != want || !json.Valid(w.Body.Bytes()) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: %d %s", method, suffix, w.Code, w.Body.String())
	}
	assertDesktopSetupPublic(t, w.Body.String())
	return w
}

func assertDesktopSetupPublic(t *testing.T, raw string, secrets ...string) {
	t.Helper()
	for _, value := range append([]string{"fixture-management-secret", "fixture-provider-secret", "fixture-OAuth-secret",
		"fixture-settings-auth-secret", `"env"`, `"proxy_url"`, `"scope_token"`, `"access_token"`, `"refresh_token"`}, secrets...) {
		if value != "" && strings.Contains(raw, value) {
			t.Fatalf("public setup response disclosed %q", value)
		}
	}
}

func decodeDesktopSetupReply(t *testing.T, w *httptest.ResponseRecorder) desktoprelay.SetupStatus {
	t.Helper()
	var reply struct {
		Setup  desktoprelay.SetupStatus `json:"setup"`
		Status desktoprelay.Status      `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Setup.Condition == "" || reply.Status.Condition == "" {
		t.Fatalf("setup/status contract missing: %s, %v", w.Body.String(), err)
	}
	return reply.Setup
}

func TestDesktopSetupConfigureStatusRestoreWithoutOtherEffects(t *testing.T) {
	f := newDesktopSetupFixture(t)
	original := `{"env":{"KEEP":"fixture-settings-auth-secret","HTTPS_PROXY":"https://prior.fixture.test"},"permissions":{"allow":["Read"]},"oauthAccount":{"token":"fixture-OAuth-secret"},"theme":"dark"}`
	if err := os.WriteFile(f.api.DesktopSettingsPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(f.root, "codex.toml")
	other := []byte("model_provider = 'fixture-other'\n")
	if err := os.WriteFile(otherPath, other, 0600); err != nil {
		t.Fatal(err)
	}
	before := decodeDesktopSetupReply(t, f.request(t, "GET", "", "", 200))
	if before.Configured || before.Condition != "not_configured" || len(f.api.DesktopRelay.Scopes()) != 0 || f.api.DesktopRelay.Status().Listening {
		t.Fatal("GET configured or started the relay")
	}
	setup := decodeDesktopSetupReply(t, f.request(t, "POST", "/configure", `{}`, 200))
	if !setup.Configured || setup.Condition != "configured" || !setup.RestartRequired || setup.SettingsPath != f.api.DesktopSettingsPath || setup.ScopeID == "" || setup.BackupPath == "" || !f.api.DesktopRelay.Status().Listening {
		t.Fatalf("automatic setup incomplete: %+v", setup)
	}
	if scopes := f.api.DesktopRelay.Scopes(); len(scopes) != 1 || scopes[0].ID != setup.ScopeID {
		t.Fatal("configure did not create exactly one managed default scope")
	}
	configured, err := os.ReadFile(f.api.DesktopSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(configured, &document); err != nil {
		t.Fatal(err)
	}
	env := document["env"].(map[string]any)
	proxyURL, _ := env["HTTPS_PROXY"].(string)
	if env["KEEP"] != "fixture-settings-auth-secret" || env["NODE_EXTRA_CA_CERTS"] == "" || proxyURL == "" || proxyURL == "https://prior.fixture.test" {
		t.Fatal("configure did not merge only the relay environment fields")
	}
	again := decodeDesktopSetupReply(t, f.request(t, "POST", "/configure", "", 200))
	if again.ScopeID != setup.ScopeID || again.BackupPath != setup.BackupPath {
		t.Fatal("repeated configure replaced default ownership or backup")
	}
	for _, w := range []*httptest.ResponseRecorder{
		f.request(t, "GET", "", "", 200),
		f.request(t, "POST", "/configure", `{}`, 200),
	} {
		assertDesktopSetupPublic(t, w.Body.String(), proxyURL)
	}
	restored := decodeDesktopSetupReply(t, f.request(t, "POST", "/restore", `{}`, 200))
	if restored.Configured || restored.Condition != "not_configured" {
		t.Fatalf("restore status: %+v", restored)
	}
	raw, err := os.ReadFile(f.api.DesktopSettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if json.Unmarshal([]byte(original), &want) != nil || json.Unmarshal(raw, &got) != nil || !reflect.DeepEqual(want, got) {
		t.Fatal("restore changed native authentication or unrelated settings")
	}
	if raw, err := os.ReadFile(otherPath); err != nil || string(raw) != string(other) {
		t.Fatal("setup/restore wrote non-Claude config")
	}
	if f.restart.Load() != 0 || f.sync.Load() != 0 {
		t.Fatal("configure, status or restore restarted Desktop or synced indexes")
	}
}

func TestDesktopSetupEmptyAPIPathNeverUsesHOMEOrEnvironment(t *testing.T) {
	f := newDesktopSetupFixture(t)
	f.api.DesktopSettingsPath = ""
	for _, dir := range []string{filepath.Join(f.root, ".claude"), filepath.Join(f.root, "unused-env-config")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("fixture-settings-auth-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	view := decodeDesktopSetupReply(t, f.request(t, "GET", "", "", 200))
	if view.Condition != "unavailable" || view.Configured || view.SettingsPath != "" {
		t.Fatalf("empty path status: %+v", view)
	}
	f.request(t, "POST", "/configure", `{}`, 503)
	f.request(t, "POST", "/restore", `{}`, 503)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	if f.api.DesktopRelay.Status().Listening || len(f.api.DesktopRelay.Scopes()) != 0 || f.restart.Load() != 0 {
		t.Fatal("empty API path enabled a relay or restarted Desktop")
	}
	if _, err := os.Stat(filepath.Join(f.root, "relay")); !os.IsNotExist(err) {
		t.Fatalf("empty path created manager storage: %v", err)
	}
	for _, dir := range []string{filepath.Join(f.root, ".claude"), filepath.Join(f.root, "unused-env-config")} {
		raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
		if err != nil || string(raw) != "fixture-settings-auth-secret" {
			t.Fatal("empty API path fell back to HOME/environment settings")
		}
	}
}

func TestDesktopSetupErrorsAreSanitized(t *testing.T) {
	for _, err := range []error{errDesktopSetupConflict, desktoprelay.ErrBusy, desktoprelay.ErrUnavailable,
		fmt.Errorf("fixture-settings-auth-secret: %w", errDesktopSetupConflict), errors.New("fixture-settings-auth-secret")} {
		w := httptest.NewRecorder()
		writeDesktopRelayError(w, err)
		want := 503
		if errors.Is(err, errDesktopSetupConflict) || errors.Is(err, desktoprelay.ErrBusy) {
			want = 409
		}
		if w.Code != want {
			t.Fatalf("error mapping: %d, want %d", w.Code, want)
		}
		assertDesktopSetupPublic(t, w.Body.String())
	}
	busy := httptest.NewRecorder()
	writeDesktopRelayError(busy, fmt.Errorf("fixture-provider-secret: %w: %w", desktoprelay.ErrBusy, &desktoprelay.SetupError{Code: "setup_busy"}))
	var reply struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(busy.Body.Bytes(), &reply); err != nil || busy.Code != 409 || reply.ErrorCode != "setup_busy" {
		t.Fatalf("setup busy classification: %d %s", busy.Code, busy.Body.String())
	}
	assertDesktopSetupPublic(t, busy.Body.String())
	f := newDesktopSetupFixture(t)
	if err := os.WriteFile(f.api.DesktopSettingsPath, []byte("fixture-settings-auth-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	w := f.request(t, "POST", "/configure", `{}`, 503)
	if strings.Contains(w.Body.String(), f.api.DesktopSettingsPath) {
		t.Fatal("actor error disclosed settings path outside authorized status")
	}
}

func TestDesktopSetupNewRoutesRetainAuthAndLocalGuards(t *testing.T) {
	f := newDesktopSetupFixture(t)
	if err := f.api.Settings.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	cookie, err := f.api.Settings.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := f.api.Settings.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	gate := &AuthGate{Store: f.api.Settings, DesktopManagementKey: f.api.ManagementKey}
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, gate.Wrap(f.mux))
	for _, suffix := range []string{"/configure", "/restore", "/restart-desktop"} {
		for _, tc := range []struct {
			name, host, remote, cookie, csrf, bearer, key string
			want                                          int
		}{
			{"anonymous", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "", "", 401},
			{"provider Bearer", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "Bearer fixture-OAuth-secret", "", 401},
			{"cookie missing CSRF", "127.0.0.1:9123", "127.0.0.1:1234", cookie, "", "", "", 403},
			{"cookie wrong CSRF", "127.0.0.1:9123", "127.0.0.1:1234", cookie, "wrong", "", "", 403},
			{"key keeps CSRF", "127.0.0.1:9123", "127.0.0.1:1234", cookie, "", "", f.api.ManagementKey, 403},
			{"cookie with CSRF", "127.0.0.1:9123", "127.0.0.1:1234", cookie, f.api.Settings.CSRFToken(), "", "", 400},
			{"device", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "Bearer " + device, "", 400},
			{"independent key", "localhost:9123", "127.0.0.1:1234", "", "", "", f.api.ManagementKey, 400},
			{"IPv6 independent key", "[::1]:9123", "[::1]:1234", "", "", "", f.api.ManagementKey, 400},
			{"LAN socket spoof", "127.0.0.1:9123", "192.0.2.6:1234", "", "", "Bearer " + device, f.api.ManagementKey, 403},
			{"LAN Host", "192.0.2.5:9123", "127.0.0.1:1234", cookie, f.api.Settings.CSRFToken(), "", f.api.ManagementKey, 403},
			{"DNS Host", "attacker.test:9123", "127.0.0.1:1234", "", "", "", f.api.ManagementKey, 403},
			{"userinfo Host", "attacker.test@localhost:9123", "127.0.0.1:1234", "", "", "", f.api.ManagementKey, 403},
		} {
			t.Run(suffix+"/"+tc.name, func(t *testing.T) {
				// A malformed body proves authorized requests reach validation
				// without mutating setup while unauthorized ones stop at the gate.
				r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/desktop-relay"+suffix, strings.NewReader(`{`))
				r.Host, r.RemoteAddr = tc.host, tc.remote
				if tc.cookie != "" {
					r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
				}
				r.Header.Set(csrfHeader, tc.csrf)
				r.Header.Set("Authorization", tc.bearer)
				r.Header.Set(desktopControlHeader, tc.key)
				r.Header.Set("X-Forwarded-For", "127.0.0.1")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Fatalf("auth guard: %d %s", w.Code, w.Body.String())
				}
				assertDesktopSetupPublic(t, w.Body.String())
			})
		}
	}
	if f.restart.Load() != 0 || f.sync.Load() != 0 || f.api.DesktopRelay.Status().Listening {
		t.Fatal("auth guard allowed a mutation")
	}
	for _, path := range []string{"/api/state", "/api/auth/status", "/api/desktop-relay"} {
		w := desktopControlRequest(t, handler, "GET", path, "", "")
		if path != "/api/auth/status" && w.Code != 401 {
			t.Fatalf("preauth %s: %d", path, w.Code)
		}
		for _, private := range []string{f.api.DesktopSettingsPath, f.root, `"setup"`, `"scope_id"`, `"backup_path"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("preauth response disclosed setup metadata")
			}
		}
	}
}
