package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func TestLoginExchangeRejectsExpiredVerifierWithoutAnotherLogin(t *testing.T) {
	p := New()
	p.verifier["abandoned"] = verifierEntry{value: "secret-verifier", startedAt: time.Now().Add(-verifierTTL - time.Minute)}
	old := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("expired login state reached the token endpoint")
		return nil, errors.New("unexpected token exchange")
	})}
	t.Cleanup(func() { oauthHTTPClient = old })
	if _, err := p.LoginExchange(context.Background(), "abandoned", "fresh-code"); err == nil {
		t.Fatal("expired verifier was accepted")
	}
	if _, exists := p.verifier["abandoned"]; exists {
		t.Fatal("expired verifier was retained")
	}
}

func TestApplyAuthRemovesAlternateKeysAndStaleAccountIdentity(t *testing.T) {
	for _, accountID := range []string{"chosen-account", ""} {
		r, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", nil)
		r.Header.Set("Authorization", "Bearer old-token")
		r.Header.Set("X-Api-Key", "old-api-key")
		r.Header.Set("X-Goog-Api-Key", "old-google-key")
		r.Header.Set("ChatGPT-Account-Id", "old-account")
		r.Header.Set("X-Codex-Feature", "preserve")
		if err := New().ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-token", AccountID: accountID}}); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Authorization") != "Bearer chosen-token" || r.Header.Get("ChatGPT-Account-Id") != accountID ||
			r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("X-Codex-Feature") != "preserve" {
			t.Fatal("outgoing authentication or feature headers are wrong")
		}
	}
}

func TestParseRateLimitRequiresBoundedKnownError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"known", `{"error":{"type":"usage_limit_reached","resets_at":1900000000}}`, true},
		{"burst", `{"error":{"type":"rate_limit_exceeded"}}`, false},
		{"text mention", `{"error":{"message":"usage_limit_reached"}}`, false},
		{"malformed", `{"error":{"type":"usage_limit_reached"}`, false},
		{"oversized", `{"error":{"type":"usage_limit_reached"}}` + strings.Repeat(" ", 8<<10), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			until, exhausted := New().ParseRateLimit(context.Background(), store.Account{}, http.StatusTooManyRequests, []byte(tc.body))
			if exhausted != tc.want || (!exhausted && !until.IsZero()) {
				t.Fatalf("classification = %v, %v", until, exhausted)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, exhausted := New().ParseRateLimit(ctx, store.Account{}, http.StatusTooManyRequests, []byte(`{"error":{"type":"usage_limit_reached"}}`)); exhausted {
		t.Fatal("cancelled request was classified as exhaustion")
	}
}

func TestControlResponsesRejectOversizedJSONPrefixes(t *testing.T) {
	old := oauthHTTPClient
	oldDefault := http.DefaultClient
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/oauth/token":
			body = `{"access_token":"do-not-log","expires_in":3600}`
		case "/backend-api/codex/usage":
			body = `{"rate_limit":{"primary_window":{"limit_window_seconds":3600,"used_percent":100}}}`
		case "/backend-api/wham/rate-limit-reset-credits":
			body = `{"credits":[]}`
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			body = `{"code":"reset"}`
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		body += strings.Repeat(" ", (1<<20)+1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	oauthHTTPClient, http.DefaultClient = client, client
	t.Cleanup(func() { oauthHTTPClient, http.DefaultClient = old, oldDefault })
	if _, err := tokenRequest(context.Background(), url.Values{"grant_type": {"refresh_token"}}); err == nil || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("oversized token response: %v", err)
	}
	if _, err := New().Usage(context.Background(), store.Account{}); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("oversized usage response: %v", err)
	}
	if _, err := New().ListResetCredits(context.Background(), store.Account{}); err == nil {
		t.Fatal("oversized reset-credit response was accepted")
	}
	if _, err := New().ConsumeResetCredit(context.Background(), store.Account{}, "fixture-credit"); err == nil {
		t.Fatal("oversized reset-redemption response was accepted")
	}
}

func TestUsageRetainsCancellationClassification(t *testing.T) {
	old := oauthHTTPClient
	oauthHTTPClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}
	t.Cleanup(func() { oauthHTTPClient = old })
	if _, err := New().Usage(context.Background(), store.Account{}); !errors.Is(err, context.Canceled) ||
		!errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("usage cancellation classification: %v", err)
	}
}
