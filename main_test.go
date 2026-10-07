package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/config"
	"switcher/internal/desktoprelay"
	"switcher/internal/login"
	"switcher/internal/server"
	"switcher/internal/sessionmeta"
	"switcher/internal/settings"
)

type mainDesktopBlockedTransport struct{}

func (mainDesktopBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fixture blocks all HTTP egress")
}

func TestDesktopRelayStartupResumesOnlyPersistedOptIn(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = mainDesktopBlockedTransport{}
	t.Cleanup(func() { http.DefaultTransport = previous })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Transport: mainDesktopBlockedTransport{},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture blocks tunnel egress")
		}}
	ctx := context.Background()
	m, err := resumeDesktopRelay(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(ctx) })
	if m.Status().Enabled || m.Status().Listening {
		t.Fatal("startup enabled the relay")
	}
	if _, err := os.Stat(cfg.DataRoot); !os.IsNotExist(err) {
		t.Fatalf("disabled startup touched data: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	resumed, err := resumeDesktopRelay(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(ctx) })
	if !resumed.Status().Listening {
		t.Fatal("explicit saved opt-in was not resumed")
	}
	if err := resumed.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	disabled, err := resumeDesktopRelay(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disabled.Close(ctx) })
	if disabled.Status().Listening || disabled.Status().Enabled {
		t.Fatal("explicit stop was not retained across startup")
	}
}

func TestDesktopRelayStartupReturnsManagerOnUnavailableStore(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = mainDesktopBlockedTransport{}
	t.Cleanup(func() { http.DefaultTransport = previous })
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	badRoot := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(badRoot, []byte("fixture-private-store"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := resumeDesktopRelay(context.Background(), desktoprelay.Config{DataRoot: badRoot, Transport: mainDesktopBlockedTransport{}})
	if err == nil || m == nil || m.Status().Listening {
		t.Fatal("unavailable relay was not retained for management status")
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
}

func TestDesktopRelayPortValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ui, relay int
		valid     bool
	}{
		{"defaults", config.DefaultPort, 8789, true},
		{"distinct", 9123, 9124, true},
		{"collision", 9123, 9123, false},
		{"zero relay", 9123, 0, false},
		{"negative relay", 9123, -1, false},
		{"large relay", 9123, 65536, false},
		{"zero UI", 0, 8789, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateServerPorts(tc.ui, tc.relay); (err == nil) != tc.valid {
				t.Fatalf("port validation: %v", err)
			}
		})
	}
}

func TestDesktopSettingsPathResolution(t *testing.T) {
	home := t.TempDir()
	override := t.TempDir()
	for _, tc := range []struct {
		name, home, override, want string
	}{
		{"default HOME fixture", home, "", filepath.Join(home, ".claude", "settings.json")},
		{"absolute override", home, override, filepath.Join(override, "settings.json")},
		{"override without HOME", "", override, filepath.Join(override, "settings.json")},
		{"relative override", home, "relative-claude", ""},
		{"dot override", home, ".", ""},
		{"tilde override", home, "~/.claude", ""},
		{"newline override", home, override + "\n", ""},
		{"relative HOME", "relative-home", "", ""},
		{"missing HOME", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", tc.home)
			t.Setenv("CLAUDE_CONFIG_DIR", tc.override)
			if got := desktopSettingsPath(); got != tc.want {
				t.Fatalf("settings path = %q, want %q", got, tc.want)
			}
		})
	}
	for _, dir := range []string{home, override} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("path resolution wrote fixture files: %v", err)
		}
	}
}

func TestSessionMetadataRootResolution(t *testing.T) {
	home, override := t.TempDir(), t.TempDir()
	desktop := filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions")
	projects := filepath.Join(home, ".claude", "projects")
	for _, tc := range []struct {
		name, goos, home, override string
		want                       sessionmeta.Config
	}{
		{"mac defaults", "darwin", home, "", sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}},
		{"mac config override", "darwin", home, override, sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: filepath.Join(override, "projects")}},
		{"linux projects only", "linux", home, "", sessionmeta.Config{ProjectsRoot: projects}},
		{"linux override", "linux", home, override, sessionmeta.Config{ProjectsRoot: filepath.Join(override, "projects")}},
		{"other platform explicit projects", "windows", home, override, sessionmeta.Config{ProjectsRoot: filepath.Join(override, "projects")}},
		{"override without HOME", "darwin", "", override, sessionmeta.Config{ProjectsRoot: filepath.Join(override, "projects")}},
		{"relative override", "darwin", home, "relative", sessionmeta.Config{DesktopRoot: desktop}},
		{"tilde override", "darwin", home, "~/.claude", sessionmeta.Config{DesktopRoot: desktop}},
		{"newline override", "darwin", home, override + "\n", sessionmeta.Config{DesktopRoot: desktop}},
		{"NUL override", "darwin", home, override + "\x00", sessionmeta.Config{DesktopRoot: desktop}},
		{"carriage return override", "darwin", home, override + "\r", sessionmeta.Config{DesktopRoot: desktop}},
		{"unsafe HOME with explicit override", "darwin", home + "\n", override, sessionmeta.Config{ProjectsRoot: filepath.Join(override, "projects")}},
		{"relative HOME", "darwin", "relative", "", sessionmeta.Config{}},
		{"missing HOME", "darwin", "", "", sessionmeta.Config{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionMetadataRoots(tc.goos, tc.home, tc.override); got != tc.want {
				t.Fatalf("metadata roots = %#v, want %#v", got, tc.want)
			}
		})
	}
	for _, path := range []string{home, override} {
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("root resolution performed filesystem writes: %v", err)
		}
	}
}

func TestDesktopRelayStartupSharesLazyExplicitMetadataIndex(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	const conversation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const session = "11111111-1111-4111-8111-111111111111"
	roots := sessionMetadataRoots("darwin", root, filepath.Join(root, ".claude"))
	path := filepath.Join(roots.DesktopRoot, "account", "workspace", "local_"+conversation+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	record := func(title string) []byte {
		return []byte(fmt.Sprintf(`{"sessionId":"local_%s","cliSessionId":%q,"createdAt":1788432998079,"cwd":"/fixture/work/switcher","title":%q}`, conversation, session, title))
	}
	if err := os.WriteFile(path, record("Before startup"), 0600); err != nil {
		t.Fatal(err)
	}
	index := sessionmeta.New(roots)
	cfg := desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Conversations: index,
		Transport: mainDesktopBlockedTransport{}, DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture forbids tunnel egress")
		}}
	manager, err := resumeDesktopRelay(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	api := &server.API{DesktopRelay: manager, DesktopSessionMetadata: index}
	if manager.Status().Enabled || manager.Status().Listening {
		t.Fatal("metadata integration enabled the relay")
	}
	if _, err := os.Stat(cfg.DataRoot); !os.IsNotExist(err) {
		t.Fatalf("metadata integration touched the disabled relay store: %v", err)
	}
	if err := os.WriteFile(path, record("After startup"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := cfg.Conversations.Resolve(context.Background(), []string{session})
	if err != nil || resolved[session] != conversation {
		t.Fatalf("explicit index resolver = %#v, %v", resolved, err)
	}
	info := api.DesktopSessionMetadata.Lookup(context.Background(), []string{session})[session]
	if info.ConversationID != conversation || info.Title != "After startup" {
		t.Fatalf("shared lazy metadata view = %#v", info)
	}
}

func webTestServer(t *testing.T, preferences *settings.Store) *httptest.Server {
	t.Helper()
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&server.API{Settings: preferences}).Register(mux)
	registerWebRoutes(mux, static, preferences)
	srv := httptest.NewServer((&server.AuthGate{Store: preferences}).Wrap(hardenedHeaders(mux)))
	t.Cleanup(srv.Close)
	return srv
}

func TestBrowserGetsOnlyStandaloneLoginUntilAuthenticated(t *testing.T) {
	preferences := settings.New(t.TempDir())
	if err := preferences.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	srv := webTestServer(t, preferences)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string, cookie *http.Cookie) *http.Response {
		t.Helper()
		r, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	root := get("/", nil)
	if root.StatusCode != http.StatusSeeOther || root.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated root: %d", root.StatusCode)
	}
	root.Body.Close()
	login := get("/login", nil)
	page, _ := io.ReadAll(login.Body)
	login.Body.Close()
	if login.StatusCode != http.StatusOK || !bytes.Contains(page, []byte(`src="/login.js"`)) ||
		bytes.Contains(page, []byte(`id="providers"`)) || bytes.Contains(page, []byte(`src="app.js"`)) ||
		login.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("login page exposed app shell or was cacheable: %d", login.StatusCode)
	}
	for _, path := range []string{"/login.js", "/login.css"} {
		resp := get(path, nil)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("login asset %s: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	for _, path := range []string{"/app.js", "/style.css", "/index.html"} {
		resp := get(path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("private asset %s: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	credentials, _ := json.Marshal(map[string]string{"password": "hunter22"})
	response, err := client.Post(srv.URL+"/api/auth/login", "application/json", bytes.NewReader(credentials))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(response.Body).Decode(&auth); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(response.Cookies()) != 1 || auth.CSRF == "" {
		t.Fatalf("login did not issue a session: %d", response.StatusCode)
	}
	cookie := response.Cookies()[0]
	if !cookie.HttpOnly {
		t.Fatal("session cookie is readable by JavaScript")
	}
	for _, path := range []string{"/api/auth/unknown", "/v0/management/unknown"} {
		resp := get(path, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unknown path %s bypassed auth: %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	deleteSessions := func(cookie *http.Cookie, csrf string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/auth/sessions", nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-Switcher-CSRF", csrf)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if status := deleteSessions(nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated session deletion: %d", status)
	}
	if status := deleteSessions(cookie, ""); status != http.StatusForbidden {
		t.Fatalf("session deletion without CSRF: %d", status)
	}
	app := get("/", cookie)
	body, _ := io.ReadAll(app.Body)
	app.Body.Close()
	if app.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`id="providers"`)) || app.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated app shell unavailable: %d", app.StatusCode)
	}
	script := get("/app.js", cookie)
	if script.StatusCode != http.StatusOK || script.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("authenticated app.js: %d", script.StatusCode)
	}
	script.Body.Close()
	logout, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/auth/logout", nil)
	logout.AddCookie(cookie)
	logout.Header.Set("X-Switcher-CSRF", auth.CSRF)
	logoutResponse, err := client.Do(logout)
	if err != nil {
		t.Fatal(err)
	}
	logoutResponse.Body.Close()
	if logoutResponse.StatusCode != http.StatusOK {
		t.Fatalf("logout: %d", logoutResponse.StatusCode)
	}
	if again := get("/", cookie); again.StatusCode != http.StatusSeeOther {
		again.Body.Close()
		t.Fatalf("old session still loads app: %d", again.StatusCode)
	} else {
		again.Body.Close()
	}
	second, err := preferences.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if status := deleteSessions(&http.Cookie{Name: "switcher_session", Value: second}, auth.CSRF); status != http.StatusOK {
		t.Fatalf("session deletion with cookie and CSRF: %d", status)
	}
	if afterAll := get("/app.js", &http.Cookie{Name: "switcher_session", Value: second}); afterAll.StatusCode != http.StatusUnauthorized {
		afterAll.Body.Close()
		t.Fatalf("logout everywhere left app available: %d", afterAll.StatusCode)
	} else {
		afterAll.Body.Close()
	}
}

func TestDefaultWithoutPasswordStillServesApp(t *testing.T) {
	preferences := settings.New(t.TempDir())
	srv := webTestServer(t, preferences)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`id="providers"`)) {
		t.Fatalf("default no-password install lost its app: %d", resp.StatusCode)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	login, err := client.Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != http.StatusSeeOther || login.Header.Get("Location") != "/" {
		t.Fatalf("login page when password is off: %d", login.StatusCode)
	}
	set, err := client.Post(srv.URL+"/api/auth/password", "application/json", bytes.NewBufferString(`{"next":"hunter22"}`))
	if err != nil {
		t.Fatal(err)
	}
	set.Body.Close()
	if set.StatusCode != http.StatusOK {
		t.Fatalf("first password setup: %d", set.StatusCode)
	}
	locked, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	locked.Body.Close()
	if locked.StatusCode != http.StatusSeeOther || locked.Header.Get("Location") != "/login" {
		t.Fatalf("app was still visible after enabling password: %d", locked.StatusCode)
	}
}

func TestCallbackServerBoundsPartialHeaders(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	srv.Config = callbackServer(srv.Config.Handler, "")
	if srv.Config.ReadHeaderTimeout <= 0 || srv.Config.ReadTimeout <= 0 || srv.Config.WriteTimeout <= 0 || srv.Config.IdleTimeout <= 0 {
		t.Fatal("callback listener has unbounded HTTP phases")
	}
	srv.Config.ReadHeaderTimeout = 30 * time.Millisecond
	srv.Start()
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /callback HTTP/1.1\r\nHost: %s\r\nX-Partial:", srv.Listener.Addr()); err != nil {
		t.Fatal(err)
	}
	_, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		t.Fatal("unfinished callback headers received a successful response")
	}
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("callback connection outlived the server header deadline")
	}
	if called.Load() {
		t.Fatal("partial headers reached callback handler")
	}
}

func TestIgnoreOwnRelayProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://scope:secret@127.0.0.1:8789")
	t.Setenv("HTTP_PROXY", "http://corp-proxy.example:8789")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:3128")
	ignoreOwnRelayProxy(8789)
	if os.Getenv("HTTPS_PROXY") != "" {
		t.Fatal("own relay proxy was kept")
	}
	if os.Getenv("HTTP_PROXY") == "" || os.Getenv("ALL_PROXY") == "" {
		t.Fatal("unrelated proxies were removed")
	}
}

// `switcher login finish` reads the callback's outcome as JSON; browsers keep
// getting the HTML page.
func TestCallbackAnswersJSONToTheCommandLine(t *testing.T) {
	handler := callbackHandler(login.New(nil))
	r := httptest.NewRequest("GET", "/callback?code=abc&state=unknown", nil)
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	handler(w, r)
	var answer struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || answer.OK || answer.State != "unknown" || answer.Error == "" {
		t.Fatalf("%v %+v", err, answer)
	}
	w = httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", "/callback?code=abc&state=unknown", nil))
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal("browsers should get the HTML page")
	}
}
