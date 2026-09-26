package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/provider/grok"
	"switcher/internal/store"
)

type grokTransport func(*http.Request) (*http.Response, error)

func (f grokTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func grokProxyFixture(t *testing.T, upstream http.HandlerFunc) *Manager {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	oldDefault, oldOAuth := http.DefaultClient, provider.OAuthHTTPClient
	http.DefaultClient = &http.Client{Timeout: 5 * time.Second, Transport: grokTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "cli-chat-proxy.grok.com" {
			return nil, errors.New("unexpected upstream")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		copy.Host = base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	provider.OAuthHTTPClient = &http.Client{Transport: grokTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected OAuth or billing request")
	})}
	t.Cleanup(func() { http.DefaultClient, provider.OAuthHTTPClient = oldDefault, oldOAuth })
	st := store.New(t.TempDir())
	a := store.Account{ID: "grok-test", Provider: "grok", Email: "test@example.com",
		Token: store.Token{AccessToken: "chosen-session", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, map[string]provider.Provider{"grok": grok.New()}, []string{"grok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(a.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGrokProxyHTTPRoutesAndSessionHeaders(t *testing.T) {
	cases := []struct{ local, upstream, method string }{
		{"/grok/chat/completions", "/v1/chat/completions", http.MethodPost},
		{"/grok/v1/chat/completions", "/v1/chat/completions", http.MethodPost},
		{"/grok/v1/models", "/v1/models", http.MethodGet},
		{"/grok/v1/billing", "/v1/billing", http.MethodGet},
	}
	want := ""
	m := grokProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want || r.URL.RawQuery != "trace=one" {
			t.Errorf("wrong session-service path: %s?%s, want %s", r.URL.Path, r.URL.RawQuery, want)
		}
		if r.Header.Get("Authorization") != "Bearer chosen-session" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" ||
			r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Switcher-CSRF") != "" ||
			r.Header.Get("X-Grok-Model-Override") != "grok-build-special" {
			t.Error("Grok auth or model override not forwarded correctly")
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"model":"grok-build-special","stream":true}` {
				t.Errorf("body changed: %s", body)
			}
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	for _, tc := range cases {
		want = tc.upstream
		r := httptest.NewRequest(tc.method, tc.local+"?trace=one", strings.NewReader(`{"model":"grok-build-special","stream":true}`))
		r.Header.Set("Authorization", "Bearer client-session")
		r.Header.Set("X-XAI-Token-Auth", "wrong-client-mode")
		r.Header.Set("X-Grok-Model-Override", "grok-build-special")
		r.Header.Set("X-Api-Key", "client-key")
		r.Header.Set("Cookie", "switcher_session=private")
		r.Header.Set("X-Switcher-CSRF", "private-csrf")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
			t.Fatalf("%s: %d %s", tc.local, w.Code, w.Body.String())
		}
	}
}

func TestGrokProxyUnknownErrorsDoNotParkOrSwitch(t *testing.T) {
	status := http.StatusBadRequest
	m := grokProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
	})
	for _, code := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status = code
		w := httptest.NewRecorder()
		m.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{}`)))
		_, parked := m.Exhausted("grok-test")
		if w.Code != code || w.Body.String() != `{"error":"rate limit exceeded"}` || w.Header().Get("Retry-After") != "30" || parked || m.ActiveID("grok") != "grok-test" {
			t.Fatalf("http %d changed error, parked or switched account: %d %s", code, w.Code, w.Body.String())
		}
	}
}

func TestGrokProxySSEFlushAndCancellation(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{})
	var streams atomic.Int32
	m := grokProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		if streams.Add(1) == 1 {
			select {
			case <-release:
				_, _ = w.Write([]byte("data: second\n\n"))
			case <-r.Context().Done():
				t.Error("first stream cancelled")
			}
		} else {
			<-r.Context().Done()
			close(cancelled)
		}
	})
	downstream := httptest.NewServer(m)
	defer downstream.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(downstream.URL+"/grok/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	first, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || first != "data: first\n" {
		t.Fatalf("first Grok SSE event not flushed: %q, %v", first, err)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.Contains(string(rest), "data: second") {
		t.Fatalf("Grok SSE completion lost: %q, %v", rest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/grok/v1/chat/completions", strings.NewReader(`{}`))
	resp, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("Grok cancellation did not reach upstream")
	}
}
