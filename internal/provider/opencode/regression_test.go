package opencode

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

func TestRateLimitRequiresConfirmedDepletedUsage(t *testing.T) {
	reset := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	depleted := `{"usage":{"weekly":{"percent":100,"resetsAt":"` + reset.Format(time.RFC3339) + `"}}}`
	for _, tc := range []struct {
		name, body string
		status     int
		want       bool
	}{
		{"burst", `{"usage":{"weekly":{"percent":50,"resetsAt":"` + reset.Format(time.RFC3339) + `"}}}`, 200, false},
		{"empty", `{}`, 200, false},
		{"outage", `{"private":"do-not-log"}`, 503, false},
		{"malformed", `{"usage":`, 200, false},
		{"oversized", depleted + strings.Repeat(" ", (1<<20)+1), 200, false},
		{"confirmed", depleted, 200, true},
		{"depleted without reset", `{"usage":{"rolling":{"percent":100}}}`, 200, true},
		{"depleted with stale reset", `{"usage":{"rolling":{"percent":100,"resetsAt":"2000-01-01T00:00:00Z"}}}`, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != upstreamBase+"/usage" || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Fatal("unexpected usage request")
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = old })
			until, exhausted := New().ParseRateLimit(context.Background(), store.Account{Token: store.Token{AccessToken: "fixture-key"}}, 429, []byte(`{"error":"rate limit exceeded"}`))
			if exhausted != tc.want || (!exhausted && !until.IsZero()) || (exhausted && !until.After(time.Now())) {
				t.Fatalf("classification = %v, %v", until, exhausted)
			}
			if tc.name == "confirmed" && !until.Equal(reset) {
				t.Fatalf("reset = %v, want %v", until, reset)
			}
		})
	}
}

func TestRateLimitCancellationAndBoundedError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	if _, exhausted := New().ParseRateLimit(ctx, store.Account{}, 429, nil); exhausted {
		t.Fatal("cancelled usage lookup caused exhaustion")
	}
	if _, err := New().Usage(ctx, store.Account{}); !errors.Is(err, context.Canceled) || !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("usage cancellation was lost: %v", err)
	}
	http.DefaultClient.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("non-429 or oversized error queried usage")
		return nil, errors.New("unexpected usage request")
	})
	for _, status := range []int{400, 429} {
		body := []byte(strings.Repeat("x", (8<<10)+1))
		if _, exhausted := New().ParseRateLimit(context.Background(), store.Account{}, status, body); exhausted {
			t.Fatal("unusable upstream response caused exhaustion")
		}
	}
}

func TestApplyAuthRemovesGoogleClientKey(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", nil)
	r.Header.Set("X-Goog-Api-Key", "old-google-key")
	if err := New().ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-key"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Authorization") != "Bearer chosen-key" {
		t.Fatal("client Google key survived selected-key authentication")
	}
}
