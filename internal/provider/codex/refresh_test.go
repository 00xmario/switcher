package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshCredentialRejection(t *testing.T) {
	status, body := http.StatusOK, `{"access_token":"new","expires_in":3600}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	old := oauthHTTPClient
	base, _ := url.Parse(srv.URL)
	oauthHTTPClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { oauthHTTPClient = old }()

	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnauthorized, ``, true},
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusForbidden, ``, false},
		{http.StatusBadGateway, ``, false},
		{http.StatusOK, `not json`, false},
	} {
		status, body = tc.status, tc.body
		a := store.Account{Token: store.Token{RefreshToken: "secret"}}
		err := New().Refresh(context.Background(), &a)
		if err == nil || errors.Is(err, provider.ErrReloginRequired) != tc.want {
			t.Fatalf("status %d body %q: err = %v, relogin = %t", status, body, err, tc.want)
		}
	}
	if err := New().Refresh(context.Background(), &store.Account{}); !errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("missing token: %v", err)
	}
	status, body = http.StatusBadRequest, `{"error":"invalid_grant","secret":"do-not-log"}`
	_, err := tokenRequest(context.Background(), url.Values{"grant_type": {"authorization_code"}})
	if errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("login exchange classified or leaked response: %v", err)
	}
	status = http.StatusUnauthorized
	_, err = tokenRequest(context.Background(), url.Values{"grant_type": {"authorization_code"}})
	if errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("login exchange 401 classified as refresh failure: %v", err)
	}
	oauthHTTPClient.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	a := store.Account{Token: store.Token{RefreshToken: "secret"}}
	if err := New().Refresh(context.Background(), &a); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("token transport error: %v", err)
	}
}

func TestUsageAuthStatus(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/usage" || r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	old := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { oauthHTTPClient = old }()
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		status = s
		_, err := New().Usage(context.Background(), store.Account{Token: store.Token{AccessToken: "secret-access"}})
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (s == http.StatusUnauthorized) || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("status %d: unexpected usage error: %v", s, err)
		}
	}
}
