package claude

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
		{http.StatusBadRequest, `{"error":{"type":"invalid_grant_error"}}`, true},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusForbidden, ``, false},
		{http.StatusInternalServerError, ``, false},
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
	_, err := tokenRequest(context.Background(), tokenURL, []byte(`{}`))
	if errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("login exchange classified or leaked response: %v", err)
	}
}

func TestUsageAuthStatus(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" || r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { http.DefaultClient = old }()
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		status = s
		_, err := New().Usage(context.Background(), store.Account{Token: store.Token{AccessToken: "secret-access"}})
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (s == http.StatusUnauthorized) || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("status %d: unexpected usage error: %v", s, err)
		}
	}
}
