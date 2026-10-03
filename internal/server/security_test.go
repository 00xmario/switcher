package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
	"switcher/internal/update"
)

func TestLANStaysClosedDuringAndAfterDisabledAuthRestart(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&API{Settings: prefs}).registerAuthRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("private state and management key"))
	})
	lan := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).WrapLAN(mux))
	request := func(path, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://192.0.2.5:9123"+path, nil)
		r.RemoteAddr = "192.0.2.6:1234"
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		w := httptest.NewRecorder()
		lan.ServeHTTP(w, r)
		return w
	}
	if w := request("/api/state", session); w.Code != 200 {
		t.Fatalf("authenticated LAN: %d", w.Code)
	}
	restarted := make(chan struct{}, 1)
	original := restartForTest
	restartForTest = func() { restarted <- struct{}{} } // failed/no-op restart, never exec the test binary
	defer func() { restartForTest = original }()
	r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/auth/disable", strings.NewReader(`{"password":"fixture-password"}`))
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	assertClosed := func() {
		for _, path := range []string{"/", "/api/state", "/api/auth/status", "/codex/v1/responses", "/v0/management/auth-files"} {
			for _, cookie := range []string{"", session} {
				w := request(path, cookie)
				if w.Code != 503 || strings.Contains(w.Body.String(), "management key") {
					t.Fatalf("LAN reopened at %s: %d", path, w.Code)
				}
			}
		}
	}
	assertClosed()
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("restart hook was not called")
	}
	assertClosed()
	local := (&AuthGate{Store: prefs}).Wrap(mux)
	w = httptest.NewRecorder()
	local.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:9123/", nil))
	if w.Code != 200 {
		t.Fatal("local no-auth install did not recover after disable")
	}
}

func TestDamagedSettingsNeverOpenDashboardOrLAN(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Update(func(st *settings.Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("private")) })
	gate := &AuthGate{Store: prefs}
	if err := os.WriteFile(prefs.Path(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, handler := range []http.Handler{gate.Wrap(next), gate.WrapLAN(next)} {
		for _, path := range []string{"/api/state", "/app.js", "/api/auth/login"} {
			r := httptest.NewRequest("GET", path, nil)
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
				t.Fatalf("damaged settings exposed %s: %d", path, w.Code)
			}
		}
	}
	w := httptest.NewRecorder()
	gate.WrapLAN(next).ServeHTTP(w, httptest.NewRequest("GET", "/codex/v1/responses", nil))
	if w.Code != 503 {
		t.Fatal("damaged settings left LAN native traffic open")
	}
	w = httptest.NewRecorder()
	gate.Wrap(next).ServeHTTP(w, httptest.NewRequest("GET", "/codex/v1/responses", nil))
	if w.Code != 200 {
		t.Fatal("local independently authenticated CLI route was browser-gated")
	}
}

func TestPreauthStatusDoesNotTrustHostOrRevealNativeMetadata(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	native := &nativeClaudeFixture{statusOverride: &claudecode.Status{Email: "private@example.test", ActiveID: "claude-private", ConfigPath: "/fixture/private-home/.claude.json"}}
	api := &API{Settings: prefs, Providers: map[string]provider.Provider{"claude": native}}
	mux := http.NewServeMux()
	api.registerAuthRoutes(mux)
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).Wrap(mux))
	for _, tc := range []struct {
		name, remote, cookie, bearer string
		details, csrf                bool
	}{
		{"anonymous local", "127.0.0.1:1234", "", "", false, false},
		{"spoofed LAN", "192.0.2.6:1234", "", "", false, false},
		{"authenticated LAN", "192.0.2.6:1234", session, "", false, true},
		{"local cookie", "127.0.0.1:1234", session, "", true, true},
		{"local device", "127.0.0.1:1234", "", "Bearer " + device, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://127.0.0.1:9123/api/auth/status", nil)
			r.RemoteAddr = tc.remote
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			r.Header.Set("Authorization", tc.bearer)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
				t.Fatalf("status: %d %v", w.Code, err)
			}
			if _, ok := body["claude_code"]; ok != tc.details {
				t.Fatalf("wrong metadata visibility: %s", w.Body.String())
			}
			if _, ok := body["csrf"]; ok != tc.csrf {
				t.Fatalf("wrong CSRF visibility: %s", w.Body.String())
			}
			if !tc.details {
				want := 2
				if tc.csrf {
					want++
				}
				if len(body) != want || strings.Contains(w.Body.String(), "private") {
					t.Fatalf("preauth disclosure: %s", w.Body.String())
				}
			}
		})
	}
}

type machineImporter struct {
	*recheckProvider
	calls atomic.Int32
}

func (*machineImporter) ID() string { return "copilot" }
func (p *machineImporter) ImportFromKeychain(context.Context) (store.Account, error) {
	p.calls.Add(1)
	return store.Account{ID: "copilot-fixture", Provider: "copilot", Email: "fixture@example.test"}, nil
}

func TestEveryMachineImportRequiresLoopbackIncludingCopilot(t *testing.T) {
	p := &machineImporter{recheckProvider: &recheckProvider{}}
	st := store.New(t.TempDir())
	providers := map[string]provider.Provider{"copilot": p}
	manager, err := proxy.New(st, providers, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: st, Proxy: manager, Providers: providers, Settings: prefs}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, tc := range []struct {
		remote       string
		cookie, csrf bool
		status       int
	}{
		{"127.0.0.1:1234", false, false, 401},
		{"127.0.0.1:1234", true, false, 403},
		{"192.0.2.6:1234", true, true, 403},
		{"127.0.0.1:1234", true, true, 200},
	} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/login/import", strings.NewReader(`{"provider":"copilot"}`))
		r.RemoteAddr = tc.remote
		if tc.cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if tc.csrf {
			r.Header.Set(csrfHeader, prefs.CSRFToken())
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("import from %s: %d %s", tc.remote, w.Code, w.Body.String())
		}
		if tc.status != 200 && p.calls.Load() != 0 {
			t.Fatal("rejected import accessed machine credentials")
		}
	}
	if p.calls.Load() != 1 {
		t.Fatal("local fixture import was not called exactly once")
	}
}

type fakeUpdateChecker struct {
	refreshes, installs atomic.Int32
	checkErr            error
}

func (c *fakeUpdateChecker) Check(context.Context) (update.State, error) {
	c.refreshes.Add(1)
	return c.State(), c.checkErr
}
func (c *fakeUpdateChecker) State() update.State {
	return update.State{Latest: "v9.0.0", Available: c.refreshes.Load() > 0}
}
func (c *fakeUpdateChecker) InstallAndRestart() error {
	c.installs.Add(1)
	return errors.New("fake install blocked")
}

func TestUpdateCheckRefreshesOnlyMetadataAndKeepsAuthAndCSRF(t *testing.T) {
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeUpdateChecker{}
	api := &API{Settings: prefs, Updater: checker}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	call := func(method, path string, cookie, csrf bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(nil))
		if cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		}
		if csrf {
			r.Header.Set(csrfHeader, prefs.CSRFToken())
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call("POST", "/api/update/check", false, false); w.Code != 401 {
		t.Fatalf("anonymous check: %d", w.Code)
	}
	if w := call("POST", "/api/update/check", true, false); w.Code != 403 {
		t.Fatalf("CSRF-less check: %d", w.Code)
	}
	if checker.refreshes.Load() != 0 || checker.installs.Load() != 0 {
		t.Fatal("rejected request invoked updater")
	}
	w := call("POST", "/api/update/check", true, true)
	var body struct {
		Update update.State `json:"update"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 || !body.Update.Available || body.Update.Latest != "v9.0.0" || checker.refreshes.Load() != 1 || checker.installs.Load() != 0 {
		t.Fatalf("metadata check installed or failed to refresh: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/api/update", true, true); w.Code != 500 || checker.installs.Load() != 1 {
		t.Fatal("install route no longer uses the separate install operation")
	}
	checker.checkErr = errors.New("fixture metadata unavailable")
	if w := call("POST", "/api/update/check", true, true); w.Code != 503 || checker.installs.Load() != 1 {
		t.Fatal("failed metadata refresh was reported as a successful check or invoked installation")
	}
	api.Updater = nil
	if w := call("POST", "/api/update/check", true, true); w.Code != 503 {
		t.Fatal("missing updater did not return unavailable")
	}
}

func TestProviderDisplayRoutesReportPersistenceFailureAndPreserveState(t *testing.T) {
	for _, tc := range []struct {
		path, method, body string
		initiallyHidden    bool
	}{
		{"/api/providers/order", "PATCH", `{"order":["other","fake"]}`, false},
		{"/api/providers/fake/hide", "POST", "", false},
		{"/api/providers/fake/show", "POST", "", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			st := store.New(root)
			providers := map[string]provider.Provider{"fake": &recheckProvider{}, "other": &recheckProvider{}}
			manager, err := proxy.New(st, providers, []string{"fake", "other"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.initiallyHidden {
				if err := manager.HideProvider("fake"); err != nil {
					t.Fatal(err)
				}
			}
			order, hidden := manager.Providers()
			statePath := filepath.Join(root, "state.json")
			if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := os.Mkdir(statePath, 0700); err != nil {
				t.Fatal(err)
			}
			api := &API{Store: st, Proxy: manager, Providers: providers}
			mux := http.NewServeMux()
			api.Register(mux)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			nextOrder, nextHidden := manager.Providers()
			if w.Code != 500 || !reflect.DeepEqual(order, nextOrder) || !reflect.DeepEqual(hidden, nextHidden) {
				t.Fatalf("failed preference write reported success or changed state: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLogoutReportsPersistentFailureAndAllowsOnlyCSRFProtectedRetry(t *testing.T) {
	root := t.TempDir()
	prefs := settings.New(root)
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	csrf := prefs.CSRFToken()
	mux := http.NewServeMux()
	(&API{Settings: prefs}).registerAuthRoutes(mux)
	privateCalls := 0
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		privateCalls++
	})
	handler := (&AuthGate{Store: prefs}).Wrap(mux)
	call := func(method, path, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		r.Header.Set(csrfHeader, csrf)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := filepath.Join(root, "sessions.json")
	backup := filepath.Join(root, "readable-sessions.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, path); err != nil {
		t.Fatal(err)
	}
	w := call("POST", "/api/auth/logout", csrf)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"status":"ok"`) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("blocked logout reported success or discarded the retry cookie: %d %s", w.Code, w.Body.String())
	}
	if prefs.ValidateSession(token) {
		t.Fatal("failed logout persistence restored in-memory authentication")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if !settings.New(root).ValidateSession(token) {
		t.Fatal("fixture did not retain the old disk session across a fresh store")
	}
	if w := call("GET", "/api/state", csrf); w.Code != http.StatusUnauthorized || privateCalls != 0 {
		t.Fatal("logout retry authentication exposed another protected route")
	}
	if w := call("POST", "/api/auth/logout", ""); w.Code != http.StatusForbidden {
		t.Fatalf("logout retry bypassed CSRF: %d", w.Code)
	}
	if w := call("POST", "/api/auth/logout", "wrong-csrf"); w.Code != http.StatusForbidden {
		t.Fatalf("logout retry accepted incorrect CSRF: %d", w.Code)
	}
	w = call("POST", "/api/auth/logout", csrf)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != sessionCookie || cookies[0].MaxAge >= 0 {
		t.Fatalf("repaired logout did not persist and expire its cookie: %d %s", w.Code, w.Body.String())
	}
	if settings.New(root).ValidateSession(token) {
		t.Fatal("successful logout retry left a valid persistent session")
	}
}
