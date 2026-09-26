package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/provider/claude"
	"switcher/internal/store"
)

type claudeTransport func(*http.Request) (*http.Response, error)

func (f claudeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func claudeProxyFixture(t *testing.T, upstream http.HandlerFunc) *Manager {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	upstreamURL, _ := url.Parse(srv.URL)
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Timeout: 5 * time.Second, Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" {
			return nil, errors.New("unexpected upstream destination")
		}
		copy := r.Clone(r.Context())
		copy.URL.Scheme, copy.URL.Host = upstreamURL.Scheme, upstreamURL.Host
		copy.Host = upstreamURL.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	st := store.New(t.TempDir())
	a := store.Account{ID: "claude-test", Provider: "claude", Email: "test@example.com",
		Token: store.Token{AccessToken: "switcher-oauth", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, map[string]provider.Provider{"claude": claude.New()}, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate(a.ID); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestClaudeProxyMessagesAndTokenCountPreserveWireShape(t *testing.T) {
	paths := []string{"/v1/messages", "/v1/messages/count_tokens"}
	index := 0
	m := claudeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != paths[index] || r.URL.RawQuery != "sample=one" || r.Method != http.MethodPost {
			t.Errorf("wrong Claude upstream route: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"claude-test","messages":[]}` {
			t.Errorf("request body changed: %s", body)
		}
		if r.Header.Get("Authorization") != "Bearer switcher-oauth" || r.Header.Get("X-Api-Key") != "" ||
			r.Header.Get("Cookie") != "" || r.Header.Get("X-Switcher-CSRF") != "" {
			t.Error("client credentials leaked or active OAuth token not applied")
		}
		if r.Header.Get("Anthropic-Beta") != "interleaved-thinking-2025-05-14,oauth-2025-04-20" ||
			r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("X-Claude-Code-Feature") != "keep-this" {
			t.Errorf("Claude feature headers changed: %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	for i, path := range paths {
		index = i
		r := httptest.NewRequest(http.MethodPost, "/claude"+path+"?sample=one", strings.NewReader(`{"model":"claude-test","messages":[]}`))
		r.Header.Set("Authorization", "Bearer old-cli-token")
		r.Header.Set("X-Api-Key", "old-cli-key")
		r.Header.Set("Cookie", "switcher_session=local-secret")
		r.Header.Set("X-Switcher-CSRF", "local-csrf-secret")
		r.Header.Add("Anthropic-Beta", "interleaved-thinking-2025-05-14")
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("X-Claude-Code-Feature", "keep-this")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
			t.Fatalf("%s: status %d body %s", path, w.Code, w.Body.String())
		}
	}
}

func TestClaudeProxyBoundedErrorsPassThroughWithoutSwitching(t *testing.T) {
	status := http.StatusBadRequest
	m := claudeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
	})
	for _, code := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		status = code
		r := httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code != code || w.Body.String() != `{"error":"invalid_request"}` || w.Header().Get("Retry-After") != "12" || m.ActiveID("claude") != "claude-test" {
			t.Fatalf("http %d changed status/body/header or switched account: %d %s", code, w.Code, w.Body.String())
		}
	}
}

func TestClaudeProxyOversizedErrorDoesNotSendTruncatedContentLength(t *testing.T) {
	const size = 1<<20 + 1
	m := claudeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(strings.Repeat("x", size)))
	})
	downstream := httptest.NewServer(m)
	defer downstream.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(downstream.URL+"/claude/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusBadGateway || len(body) > 1024 {
		t.Fatalf("oversized error leaked a truncated upstream response: status=%d bytes=%d err=%v", resp.StatusCode, len(body), err)
	}
}

func TestClaudeProxySSEFlushAndCancellation(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{})
	var streams atomic.Int32
	m := claudeProxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: first\n\n"))
		w.(http.Flusher).Flush()
		if streams.Add(1) == 1 {
			select {
			case <-release:
				_, _ = w.Write([]byte("event: message\ndata: second\n\n"))
			case <-r.Context().Done():
				t.Error("first SSE request was cancelled before completion")
			}
		} else {
			<-r.Context().Done()
			close(cancelled)
		}
	})
	downstream := httptest.NewServer(m)
	defer downstream.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(downstream.URL+"/claude/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "event: message\n" {
		t.Fatalf("first SSE event did not flush: %q, %v", first, err)
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !strings.Contains(string(rest), "data: second") {
		t.Fatalf("second SSE event lost: %q, %v", rest, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, downstream.URL+"/claude/v1/messages", strings.NewReader(`{}`))
	resp, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("downstream cancellation did not reach upstream")
	}
}
