package desktoprelay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestAccountRemovedDuringBindingPreparationPreservesNotFound(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var removed atomic.Bool
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		if id == "B" {
			close(entered)
			<-release
			if removed.Load() {
				return desktoprelay.Credential{}, fmt.Errorf("oauth-secret and native ownership details: %w", desktoprelay.ErrNotFound)
			}
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	var sends atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := m.Bind(context.Background(), s.ID, sessionA, "B", bound.Revision); result <- err }()
	await(t, entered)
	removed.Store(true)
	once.Do(func() { close(release) })
	err = <-result
	if !errors.Is(err, desktoprelay.ErrNotFound) {
		t.Fatalf("removed account lost typed classification: %v", err)
	}
	var classified *desktoprelay.CredentialError
	if !errors.As(err, &classified) || classified.Code != "account_not_found" || errors.Unwrap(classified) != desktoprelay.ErrNotFound {
		t.Fatalf("removed account has no safe stable code: %v", err)
	}
	if strings.Contains(err.Error(), "oauth-secret") || strings.Contains(err.Error(), "native ownership") {
		t.Fatal("credential source error leaked")
	}
	view := observed(t, m, s.ID, sessionA)
	if view.AccountID != "A" || view.Revision != bound.Revision || sends.Load() != 1 {
		t.Fatalf("removed target changed binding or sent inference: %+v sends=%d", view, sends.Load())
	}
}

func TestBindCredentialFailuresHaveSanitizedStableCodes(t *testing.T) {
	cases := []struct {
		name, code          string
		sourceErr, wantKind error
	}{
		{"missing", "account_not_found", fmt.Errorf("oauth-secret: %w", desktoprelay.ErrNotFound), desktoprelay.ErrNotFound},
		{"busy", "credential_busy", fmt.Errorf("oauth-secret: %w", desktoprelay.ErrBusy), desktoprelay.ErrCredentialBusy},
		{"unavailable", "credential_unavailable", errors.New("oauth-secret in native store"), desktoprelay.ErrUnavailable},
		{"invalid credential", "credential_unavailable", nil, desktoprelay.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
				if id == "B" {
					return desktoprelay.Credential{}, tc.sourceErr
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-A"}, nil
			}}
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
			if err != nil {
				t.Fatal(err)
			}
			_, err = m.Bind(context.Background(), s.ID, sessionA, "B", bound.Revision)
			var classified *desktoprelay.CredentialError
			if !errors.Is(err, tc.wantKind) || !errors.As(err, &classified) || classified.Code != tc.code || errors.Is(err, desktoprelay.ErrBusy) {
				t.Fatalf("source classification %v", err)
			}
			if tc.code == "credential_busy" && !errors.Is(err, desktoprelay.ErrUnavailable) {
				t.Fatal("credential busy must retain 503-compatible unavailable classification")
			}
			b, _ := json.Marshal(classified)
			if strings.Contains(string(b), "oauth-secret") || strings.Contains(err.Error(), "oauth-secret") {
				t.Fatal("source error leaked through JSON or message")
			}
			view := observed(t, m, s.ID, sessionA)
			if view.AccountID != "A" || view.Revision != bound.Revision {
				t.Fatalf("failed preparation changed binding %+v", view)
			}
		})
	}
}

func TestForwardCredentialFailuresAreTypedBeforeAnyUpstreamSend(t *testing.T) {
	cases := []struct {
		name, code string
		sourceErr  error
		status     int
	}{
		{"removed", "account_not_found", fmt.Errorf("oauth-secret: %w", desktoprelay.ErrNotFound), 404},
		{"busy", "credential_busy", fmt.Errorf("oauth-secret: %w", desktoprelay.ErrBusy), 503},
		{"unavailable", "credential_unavailable", errors.New("oauth-secret native-store failure"), 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var fail atomic.Bool
			var sends atomic.Int32
			cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
				if fail.Load() {
					return desktoprelay.Credential{}, tc.sourceErr
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
			bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
			if err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				r := send(t, c, br, path, `{}`, sessionA, nil)
				body := drain(t, r)
				assertCredentialReply(t, r.StatusCode, body, tc.status, tc.code)
			}
			if sends.Load() != 1 {
				t.Fatal("failed selected credential sent inference or caller fallback")
			}
			view := observed(t, m, s.ID, sessionA)
			if view.AccountID != "A" || view.Revision != bound.Revision {
				t.Fatalf("credential failure changed binding %+v", view)
			}
		})
	}
}

func TestRemovedAccountDuring401RefreshReturnsTypedFailureWithoutRetry(t *testing.T) {
	cfg := fixtureConfig(t)
	var selected atomic.Bool
	var sends atomic.Int32
	src := fixtureSource().(sourceFunc)
	src.refresh = func(ctx context.Context, id, token string) (desktoprelay.Credential, error) {
		return desktoprelay.Credential{}, fmt.Errorf("oauth-secret: %w", desktoprelay.ErrNotFound)
	}
	cfg.Source = src
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		status := 200
		if selected.Load() {
			status = 401
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("original rejection"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	selected.Store(true)
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	body := drain(t, r)
	assertCredentialReply(t, r.StatusCode, body, 404, "account_not_found")
	if sends.Load() != 2 {
		t.Fatal("removed credential retried")
	}
	view := observed(t, m, s.ID, sessionA)
	if view.AccountID != "A" || view.Revision != bound.Revision {
		t.Fatal("refresh failure mutated binding")
	}
}

func TestAccountRemovedDuringForwardPreparationSendsNothingUpstream(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var preparing, removed atomic.Bool
	var sends atomic.Int32
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		if preparing.Load() {
			close(entered)
			<-release
			if removed.Load() {
				return desktoprelay.Credential{}, fmt.Errorf("oauth-secret removed after account check: %w", desktoprelay.ErrNotFound)
			}
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
	bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	preparing.Store(true)
	result := make(chan struct{}, 1)
	go func() {
		defer func() { result <- struct{}{} }()
		r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
		assertCredentialReply(t, r.StatusCode, drain(t, r), 404, "account_not_found")
	}()
	await(t, entered)
	removed.Store(true)
	once.Do(func() { close(release) })
	<-result
	view := observed(t, m, s.ID, sessionA)
	if view.AccountID != "A" || view.Revision != bound.Revision || sends.Load() != 1 {
		t.Fatalf("removed account was sent or changed selection: %+v sends=%d", view, sends.Load())
	}
}

func assertCredentialReply(t *testing.T, status int, body string, wantStatus int, wantCode string) {
	t.Helper()
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Details struct {
				Code string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || payload.Type != "error" || payload.Error.Details.Code != wantCode || strings.Contains(body, "oauth-secret") {
		t.Fatalf("credential response %d %s, want %d %s", status, body, wantStatus, wantCode)
	}
	if wantStatus == 404 && payload.Error.Type != "not_found_error" {
		t.Fatal("removed credential lost native not-found error type")
	}
}
