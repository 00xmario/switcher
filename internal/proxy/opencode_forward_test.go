package proxy

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/provider/opencode"
	"switcher/internal/store"
)

type openCodeTransport func(*http.Request) (*http.Response, error)

func (f openCodeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func openCodeProxyFixture(t *testing.T, upstream http.HandlerFunc) *Manager {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Timeout: 5 * time.Second, Transport: openCodeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "opencode.ai" {
			return nil, errors.New("unexpected upstream destination")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		copy.Host = base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	st := store.New(t.TempDir())
	a := store.Account{ID: "opencode-test", Provider: "opencode", Email: "test@example.com",
		Token: store.Token{AccessToken: "chosen-go-key"}}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, map[string]provider.Provider{"opencode": opencode.New()}, []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(a.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenCodeProxyVersionedAndLegacyRoutes(t *testing.T) {
	cases := []struct{ local, upstream string }{
		{"/opencode/responses", "/zen/go/v1/responses"},
		{"/opencode/v1/responses", "/zen/go/v1/responses"},
		{"/opencode/v1/chat/completions", "/zen/go/v1/chat/completions"},
		{"/opencode/v1/messages", "/zen/go/v1/messages"},
		{"/opencode/v1/models", "/zen/go/v1/models"},
	}
	want := ""
	m := openCodeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want || r.URL.RawQuery != "trace=one" {
			t.Errorf("wrong OpenCode Go route: %s?%s, want %s", r.URL.Path, r.URL.RawQuery, want)
		}
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(body) != `{"model":"go-test"}` {
			t.Errorf("request body changed: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer chosen-go-key" || r.Header.Get("X-Api-Key") != "" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("X-Switcher-CSRF") != "" || r.Header.Get("X-OpenCode-Feature") != "preserve" {
			t.Error("OpenCode client credentials leaked or chosen key/feature header lost")
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	for _, tc := range cases {
		want = tc.upstream
		method, body := http.MethodPost, strings.NewReader(`{"model":"go-test"}`)
		if strings.HasSuffix(tc.local, "/models") {
			method, body = http.MethodGet, strings.NewReader("")
		}
		r := httptest.NewRequest(method, tc.local+"?trace=one", body)
		r.Header.Set("Authorization", "Bearer native-go-key")
		r.Header.Set("X-Api-Key", "native-api-key")
		r.Header.Set("Cookie", "switcher_session=secret")
		r.Header.Set("X-Switcher-CSRF", "secret-csrf")
		r.Header.Set("X-OpenCode-Feature", "preserve")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
			t.Fatalf("%s: status %d body %s", tc.local, w.Code, w.Body.String())
		}
	}
}

func TestOpenCodeProxyBoundedErrorsDoNotSwitch(t *testing.T) {
	status := http.StatusBadRequest
	m := openCodeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"bad_request"}`))
	})
	for _, code := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		status = code
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/opencode/v1/responses", strings.NewReader(`{}`)))
		if w.Code != code || w.Body.String() != `{"error":"bad_request"}` || w.Header().Get("Retry-After") != "9" || m.ActiveID("opencode") != "opencode-test" {
			t.Fatalf("http %d changed error or active account: %d %s", code, w.Code, w.Body.String())
		}
	}
}

func TestOpenCodeProxySSEFlushesBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	m := openCodeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: second\n\n"))
	})
	downstream := httptest.NewServer(m)
	defer downstream.Close()
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Post(downstream.URL+"/opencode/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("first Go SSE event did not flush: %q, %v", first, err)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.Contains(string(rest), "data: second") {
		t.Fatalf("Go stream lost second event: %q, %v", rest, err)
	}
}
