package grok

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshCredentialRejection(t *testing.T) {
	status, body := http.StatusOK, `{"access_token":"new","expires_in":3600}`
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_, _ = fmt.Fprintf(w, `{"token_endpoint":%q,"device_authorization_endpoint":%q}`, srv.URL+"/token", srv.URL+"/device")
		case "/token":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	old := http.DefaultClient
	oldOAuth := provider.OAuthHTTPClient
	base, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	http.DefaultClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected use of default client")
	})}
	provider.OAuthHTTPClient = client
	defer func() { http.DefaultClient, provider.OAuthHTTPClient = old, oldOAuth }()

	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusUnauthorized, ``, true},
		{http.StatusUnauthorized, `{"error":"invalid_client"}`, false},
		{http.StatusBadRequest, `{"error":"expired_token"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusForbidden, ``, false},
		{http.StatusServiceUnavailable, ``, false},
		{http.StatusOK, `{not json`, false},
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
	status = http.StatusServiceUnavailable
	// Discovery failure is not evidence about the stored credential.
	oldTransport := provider.OAuthHTTPClient.Transport
	provider.OAuthHTTPClient.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})
	defer func() { provider.OAuthHTTPClient.Transport = oldTransport }()
	a := store.Account{Token: store.Token{RefreshToken: "secret"}}
	if err := New().Refresh(context.Background(), &a); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("discovery transport error: %v", err)
	}
}

func TestRefreshDiscoveryRespectsCallerDeadline(t *testing.T) {
	for _, blocked := range []string{"discovery", "token"} {
		t.Run(blocked, func(t *testing.T) {
			var srv *httptest.Server
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (blocked == "discovery" && r.URL.Path == "/.well-known/openid-configuration") ||
					(blocked == "token" && r.URL.Path == "/token") {
					select {
					case <-r.Context().Done():
					case <-time.After(500 * time.Millisecond):
					}
				}
				if r.URL.Path == "/.well-known/openid-configuration" {
					_, _ = fmt.Fprintf(w, `{"token_endpoint":%q,"device_authorization_endpoint":%q}`, srv.URL+"/token", srv.URL+"/device")
				}
			}))
			defer srv.Close()
			base, _ := url.Parse(srv.URL)
			client := &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
				copy := req.Clone(req.Context())
				copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
				return http.DefaultTransport.RoundTrip(copy)
			})}
			oldDefault, oldOAuth := http.DefaultClient, provider.OAuthHTTPClient
			http.DefaultClient, provider.OAuthHTTPClient = client, client
			defer func() { http.DefaultClient, provider.OAuthHTTPClient = oldDefault, oldOAuth }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			a := store.Account{Token: store.Token{RefreshToken: "secret"}}
			err := New().Refresh(ctx, &a)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("refresh error = %v, want deadline exceeded", err)
			}
			if elapsed := time.Since(start); elapsed > 350*time.Millisecond {
				t.Fatalf("%s held refresh for %v after caller deadline", blocked, elapsed)
			}
		})
	}
}

func TestUsageAuthStatus(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing" || r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { provider.OAuthHTTPClient = old }()
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		status = s
		_, err := New().Usage(context.Background(), store.Account{Token: store.Token{AccessToken: "secret-access"}})
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (s == http.StatusUnauthorized) || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("status %d: unexpected usage error: %v", s, err)
		}
	}
}
