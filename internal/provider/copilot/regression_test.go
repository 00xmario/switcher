package copilot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type regressionTransport func(*http.Request) (*http.Response, error)

func (f regressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRateLimitNeedsSpecificErrorOrConfirmedQuota(t *testing.T) {
	depleted := `{"quota_snapshots":{"premium_interactions":{"entitlement":300,"used":300}}}`
	available := `{"quota_snapshots":{"premium_interactions":{"entitlement":300,"used":150}}}`
	for _, tc := range []struct {
		name, errorBody, quota string
		status                 int
		want                   bool
	}{
		{"burst", `{"error":"rate limit exceeded"}`, available, 200, false},
		{"premium mention", `{"message":"premium rate limit exceeded"}`, available, 200, false},
		{"malformed error", `{"error":`, available, 200, false},
		{"missing quota", `{}`, `{}`, 200, false},
		{"entitled is not consumed quota", `{}`, `{"quota_snapshots":{"premium_interactions":{"entitlement":300,"entitled":300}}}`, 200, false},
		{"missing consumed count", `{}`, `{"quota_snapshots":{"premium_interactions":{"entitlement":300}}}`, 200, false},
		{"unlimited quota", `{}`, `{"quota_snapshots":{"premium_interactions":{"entitlement":300,"used":300,"unlimited":true}}}`, 200, false},
		{"quota outage", `{"error":"rate limit exceeded"}`, `{"private":"do-not-log"}`, 503, false},
		{"malformed quota", `{}`, `{"quota_snapshots":`, 200, false},
		{"oversized quota", `{}`, depleted + strings.Repeat(" ", (1<<20)+1), 200, false},
		{"confirmed quota", `{"error":"other"}`, depleted, 200, true},
		{"known error during outage", `{"error":"premium_request_exceeded"}`, `{}`, 503, true},
		{"nested known code", `{"error":{"code":"premium_request_exceeded"}}`, available, 200, true},
		{"nested known type", `{"error":{"type":"premium_request_exceeded"}}`, available, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := provider.OAuthHTTPClient
			provider.OAuthHTTPClient = &http.Client{Transport: regressionTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != copilotUserURL || r.Header.Get("Authorization") != "Bearer github-fixture" {
					t.Fatal("unexpected quota request")
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.quota))}, nil
			})}
			t.Cleanup(func() { provider.OAuthHTTPClient = old })
			until, exhausted := New().ParseRateLimit(context.Background(), store.Account{Token: store.Token{RefreshToken: "github-fixture"}}, 429, []byte(tc.errorBody))
			if exhausted != tc.want || (!exhausted && !until.IsZero()) || (exhausted && !until.After(time.Now())) {
				t.Fatalf("classification = %v, %v", until, exhausted)
			}
		})
	}
}

func TestRateLimitAndUsageCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: regressionTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	a := store.Account{Token: store.Token{RefreshToken: "github-fixture"}}
	if _, exhausted := New().ParseRateLimit(ctx, a, 429, []byte(`{"error":"premium_request_exceeded"}`)); exhausted {
		t.Fatal("cancelled quota lookup caused exhaustion")
	}
	if _, err := New().Usage(ctx, a); !errors.Is(err, context.Canceled) || !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("usage cancellation classification: %v", err)
	}
	provider.OAuthHTTPClient.Transport = regressionTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("non-429 or oversized error queried quota")
		return nil, errors.New("unexpected quota call")
	})
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"premium_request_exceeded"}`},
		{429, `{"error":"premium_request_exceeded"}` + strings.Repeat(" ", 8<<10)},
	} {
		if _, exhausted := New().ParseRateLimit(context.Background(), a, tc.status, []byte(tc.body)); exhausted {
			t.Fatal("unusable response caused exhaustion")
		}
	}
}

func TestApplyAuthRemovesAlternateClientKeys(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer old-token")
	r.Header.Set("X-Api-Key", "old-api-key")
	r.Header.Set("X-Goog-Api-Key", "old-google-key")
	r.Header.Set("X-Copilot-Feature", "preserve")
	if err := New().ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-token"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer chosen-token" || r.Header.Get("X-Api-Key") != "" ||
		r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("X-Copilot-Feature") != "preserve" {
		t.Fatal("outgoing authentication or feature headers are wrong")
	}
}

func TestCopilotControlResponsesRejectOversizedJSONPrefixes(t *testing.T) {
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: regressionTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case userURL:
			body = `{"login":"fixture"}`
		case copilotTokenURL:
			body = `{"token":"do-not-log","expires_at":1900000000}`
		case deviceCodeURL:
			body = `{"device_code":"do-not-log","verification_uri":"https://example.test/verify"}`
		default:
			t.Fatal("unexpected response-limit request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body + strings.Repeat(" ", (1<<20)+1)))}, nil
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	if _, err := New().accountFromGithubToken(context.Background(), "fixture-github"); err == nil {
		t.Fatal("oversized GitHub identity response was accepted")
	}
	a := store.Account{Token: store.Token{RefreshToken: "fixture-github"}}
	if err := New().Refresh(context.Background(), &a); err == nil || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("oversized mint response: %v", err)
	}
	if _, _, err := New().DeviceStart(context.Background()); err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("oversized device response: %v", err)
	}
}
