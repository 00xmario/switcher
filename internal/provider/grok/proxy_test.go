package grok

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func TestGrokProxyPathsAndChosenSessionHeaders(t *testing.T) {
	p := New()
	for _, tc := range []struct{ path, want string }{
		{"/chat/completions", "https://cli-chat-proxy.grok.com/v1/chat/completions"},
		{"/v1/chat/completions", "https://cli-chat-proxy.grok.com/v1/chat/completions"},
		{"/v1/models", "https://cli-chat-proxy.grok.com/v1/models"},
		{"/v1/billing", "https://cli-chat-proxy.grok.com/v1/billing"},
		{"/v1x/chat/completions", "https://cli-chat-proxy.grok.com/v1/v1x/chat/completions"},
	} {
		if got := p.UpstreamURL(tc.path); got != tc.want {
			t.Fatalf("UpstreamURL(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer old-token")
	r.Header.Set("X-Api-Key", "old-key")
	r.Header.Set("X-XAI-Token-Auth", "old-client-mode")
	r.Header.Set("X-Grok-Model-Override", "grok-build-special")
	if err := p.ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-session"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer chosen-session" || r.Header.Get("X-Api-Key") != "" ||
		r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" || r.Header.Get("X-Grok-Model-Override") != "grok-build-special" {
		t.Fatal("Grok session auth or model override changed incorrectly")
	}
}

func TestGrokUsageWithoutCurrentPeriodDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing" || r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Errorf("unexpected billing request: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":150}}`))
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "cli-chat-proxy.grok.com" {
			return nil, errors.New("unexpected destination")
		}
		copy := req.Clone(req.Context())
		copy.URL.Scheme, copy.URL.Host = base.Scheme, base.Host
		return http.DefaultTransport.RoundTrip(copy)
	})}
	defer func() { provider.OAuthHTTPClient = old }()
	u, err := New().Usage(context.Background(), store.Account{Token: store.Token{AccessToken: "session-token"}})
	if err != nil || !u.Available || len(u.Windows) != 1 || u.Windows[0].UsedPercent != 100 || u.Windows[0].ResetsAt != 0 {
		t.Fatalf("missing period should retain only reported usage, not panic: %+v, %v", u, err)
	}
}

func TestGrokDoesNotInferExhaustionFromUnknownErrors(t *testing.T) {
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("rate-limit classification must not query billing")
		return nil, errors.New("unexpected quota request")
	})}
	defer func() { provider.OAuthHTTPClient = old }()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if _, exhausted := New().ParseRateLimit(context.Background(), store.Account{}, status, []byte(`{"error":"rate limit exceeded"}`)); exhausted {
			t.Fatalf("http %d falsely parked the Grok account", status)
		}
	}
}
