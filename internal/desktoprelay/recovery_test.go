package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func Test401RecoveryUsesSameAccountAndRejectedGenerationBeforeOutput(t *testing.T) {
	cfg := fixtureConfig(t)
	var tokenMu sync.Mutex
	token := "old-A"
	var grants int
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		tokenMu.Lock()
		defer tokenMu.Unlock()
		return desktoprelay.Credential{AccountID: id, AccessToken: token}, nil
	}, refresh: func(_ context.Context, id, rejected string) (desktoprelay.Credential, error) {
		tokenMu.Lock()
		defer tokenMu.Unlock()
		if id != "A" || rejected != "old-A" {
			t.Errorf("refresh ownership %q %q", id, rejected)
		}
		if token == rejected {
			token = "new-A"
			grants++
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: token}, nil
	}}
	var oldCalls atomic.Int32
	both := make(chan struct{})
	var selected atomic.Bool
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"model":"same-model"}` {
			t.Errorf("replay changed body: %s", b)
		}
		status := 200
		reply := "caller"
		if selected.Load() {
			switch r.Header.Get("Authorization") {
			case "Bearer old-A":
				if oldCalls.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				status = 401
				reply = "do not expose first rejection"
			case "Bearer new-A":
				reply = "event: message_stop\ndata: unchanged\n\n"
			default:
				t.Errorf("fallback credential: %v", r.Header)
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{"model":"same-model"}`, sessionA, nil))
	task := observed(t, m, s.ID, sessionA)
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", task.Revision); err != nil {
		t.Fatal(err)
	}
	selected.Store(true)
	c2, br2 := tunnel(t, s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r := send(t, c, br, "/v1/messages", `{"model":"same-model"}`, sessionA, nil)
		b := drain(t, r)
		if r.StatusCode != 200 || b != "event: message_stop\ndata: unchanged\n\n" {
			t.Errorf("first response: %d %q", r.StatusCode, b)
		}
	}()
	go func() {
		defer wg.Done()
		r := send(t, c2, br2, "/v1/messages", `{"model":"same-model"}`, sessionA, nil)
		b := drain(t, r)
		if r.StatusCode != 200 || b != "event: message_stop\ndata: unchanged\n\n" {
			t.Errorf("second response: %d %q", r.StatusCode, b)
		}
	}()
	wg.Wait()
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if grants != 1 {
		t.Fatalf("refresh consumed %d generations", grants)
	}
}

func TestRecoveryNeverFallsBackAndIsLimitedToOneReal401(t *testing.T) {
	cases := []struct {
		name                               string
		status                             int
		networkError                       bool
		freshAccount, freshToken           string
		refreshError                       bool
		wantCalls, wantRefresh, wantStatus int
	}{
		{"forbidden", 403, false, "A", "new-A", false, 1, 0, 403},
		{"rate limit", 429, false, "A", "new-A", false, 1, 0, 429},
		{"server error", 503, false, "A", "new-A", false, 1, 0, 503},
		{"network error", 0, true, "A", "new-A", false, 1, 0, 502},
		{"SSE error after headers", 200, false, "A", "new-A", false, 1, 0, 200},
		{"retry still rejected", 401, false, "A", "new-A", false, 2, 1, 401},
		{"refresh failed", 401, false, "A", "new-A", true, 1, 1, 401},
		{"refresh unchanged", 401, false, "A", "token-A", false, 1, 1, 401},
		{"wrong refresh owner", 401, false, "B", "new-B", false, 1, 1, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var calls, refreshes atomic.Int32
			var selected atomic.Bool
			src := fixtureSource().(sourceFunc)
			src.refresh = func(_ context.Context, id, old string) (desktoprelay.Credential, error) {
				refreshes.Add(1)
				if id != "A" || old != "token-A" {
					t.Errorf("wrong rejected credential %q %q", id, old)
				}
				if tc.refreshError {
					return desktoprelay.Credential{}, errors.New("do not expose oauth-secret")
				}
				return desktoprelay.Credential{AccountID: tc.freshAccount, AccessToken: tc.freshToken}, nil
			}
			cfg.Source = src
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if !selected.Load() {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("observe"))}, nil
				}
				calls.Add(1)
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || r.Header.Get("Authorization") == "Bearer caller-token" || r.Header.Get("X-Api-Key") != "" {
					t.Errorf("fallback credentials %v", r.Header)
				}
				if tc.networkError {
					return nil, errors.New("network failed with oauth-secret")
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "Retry-After": []string{"17"}, "X-Request-Id": []string{"original"}}, Body: io.NopCloser(strings.NewReader("event: error\ndata: exact-upstream-error\n\n"))}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
				t.Fatal(err)
			}
			selected.Store(true)
			r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
			b := drain(t, r)
			if r.StatusCode != tc.wantStatus || int(calls.Load()) != tc.wantCalls || int(refreshes.Load()) != tc.wantRefresh || strings.Contains(b, "oauth-secret") {
				t.Fatalf("recovery %d %q calls %d refresh %d", r.StatusCode, b, calls.Load(), refreshes.Load())
			}
			if !tc.networkError && (b != "event: error\ndata: exact-upstream-error\n\n" || r.Header.Get("Retry-After") != "17" || r.Header.Get("X-Request-Id") != "original") {
				t.Fatal("upstream error changed")
			}
		})
	}
}

func TestSelectedPreparationFailureDoesNotSendCallerCredentialOrLeakSecrets(t *testing.T) {
	cfg := fixtureConfig(t)
	var fail atomic.Bool
	var sends atomic.Int32
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		if fail.Load() {
			return desktoprelay.Credential{}, errors.New("oauth-secret native-store-details")
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-A"}, nil
	}}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	one, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err = m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); !errors.Is(err, desktoprelay.ErrUnavailable) || strings.Contains(err.Error(), "oauth-secret") {
		t.Fatalf("credential source error leaked: %v", err)
	}
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	b := drain(t, r)
	if r.StatusCode != 503 || strings.Contains(b, "oauth-secret") || sends.Load() != 1 {
		t.Fatalf("fallback/send/leak %d %s sends %d", r.StatusCode, b, sends.Load())
	}
}

func TestPartialUpstreamStreamFailureAbortsWithoutReplacement(t *testing.T) {
	cfg := fixtureConfig(t)
	var sends atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&brokenStream{})}, nil
	})
	_, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	r := send(t, c, br, "/v1/messages", `{}`, "", nil)
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err == nil || string(b) != "event: content_block_delta\ndata: first\n\n" || sends.Load() != 1 {
		t.Fatalf("truncated stream falsely completed or retried %q %v sends %d", b, err, sends.Load())
	}
}

type brokenStream struct{ sent bool }

func (b *brokenStream) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("fixture stream failure")
	}
	b.sent = true
	return copy(p, "event: content_block_delta\ndata: first\n\n"), nil
}
