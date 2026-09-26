package main

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"switcher/internal/server"
	"switcher/internal/settings"
)

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
