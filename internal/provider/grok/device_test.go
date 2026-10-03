package grok

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func TestDevicePollSlowDownIncreasesEverySubsequentInterval(t *testing.T) {
	old := provider.OAuthHTTPClient
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","sub":"fixture-subject"}`)) + ".signature"
	bodies := []string{
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down"}`,
		`{"error":"authorization_pending"}`,
		`{"error":"slow_down"}`,
		`{"access_token":"fixture-access","refresh_token":"fixture-refresh","id_token":"` + idToken + `","expires_in":3600}`,
	}
	calls := 0
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://example.test/token" || r.Method != http.MethodPost {
			t.Fatal("unexpected device poll request")
		}
		if err := r.ParseForm(); err != nil || r.PostForm.Get("device_code") != "fixture-device" || r.PostForm.Get("grant_type") != deviceGrantType {
			t.Fatal("wrong device poll form")
		}
		if calls >= len(bodies) {
			t.Fatal("device flow polled after success")
		}
		body := bodies[calls]
		calls++
		status := http.StatusBadRequest
		if calls == len(bodies) {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	var waits []time.Duration
	p := New()
	p.wait = func(ctx context.Context, d time.Duration) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("poll timer did not receive the flow deadline")
		}
		waits = append(waits, d)
		return ctx.Err()
	}
	a, err := p.pollForToken(context.Background(), "https://example.test/token", "fixture-device", 1, time.Now().Add(time.Minute))
	if err != nil || a.Token.AccessToken != "fixture-access" || a.Token.AccountID != "fixture-subject" {
		t.Fatalf("device login failed: %v", err)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 10 * time.Second, 15 * time.Second}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("poll waits = %v, want %v", waits, want)
	}
}

func TestDeviceStartCarriesServerExpiryToInjectedTimer(t *testing.T) {
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.String() {
		case discoveryURL:
			body = `{"device_authorization_endpoint":"https://example.test/device","token_endpoint":"https://example.test/token"}`
		case "https://example.test/device":
			body = `{"device_code":"fixture-device","user_code":"ABCD","verification_uri":"https://example.test/verify","interval":7,"expires_in":12}`
		default:
			t.Fatal("device expiry test reached the token endpoint")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	p := New()
	p.wait = func(ctx context.Context, d time.Duration) error {
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 12*time.Second || d != 7*time.Second {
			t.Fatal("device expiry or advertised poll interval was lost")
		}
		return context.Canceled
	}
	_, poll, err := p.DeviceStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poll(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("injected timer cancellation was lost: %v", err)
	}
}

func TestDevicePollCancellationAndExpiryNeverReachTokenEndpoint(t *testing.T) {
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("cancelled or expired device flow contacted the token endpoint")
		return nil, errors.New("unexpected token poll")
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().pollForToken(ctx, "https://example.test/token", "device", 5, time.Now().Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation: %v", err)
	}
	if _, err := New().pollForToken(context.Background(), "https://example.test/token", "device", 5, time.Now().Add(-time.Second)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired device code: %v", err)
	}
	if err := waitForPoll(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("real timer cancellation: %v", err)
	}
}

func TestGrokApplyAuthRemovesGoogleClientKey(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/chat/completions", nil)
	r.Header.Set("X-Goog-Api-Key", "old-google-key")
	if err := New().ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-token"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Authorization") != "Bearer chosen-token" {
		t.Fatal("Google client key survived Grok authentication")
	}
}

func TestGrokControlResponseLimitsAndUsageCancellation(t *testing.T) {
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			body = `{"device_authorization_endpoint":"https://example.test/device","token_endpoint":"https://example.test/token"}`
		case "/v1/billing":
			body = `{"config":{"creditUsagePercent":100}}`
		case "/token", "/device":
			body = `{"access_token":"do-not-log"}`
		default:
			t.Fatal("unexpected Grok response-limit request")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body + strings.Repeat(" ", (1<<20)+1)))}, nil
	})}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
	if _, err := discover(context.Background()); err == nil {
		t.Fatal("oversized discovery response was accepted")
	}
	if _, _, err := postFormLenient(context.Background(), "https://example.test/token", nil); err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("oversized token response: %v", err)
	}
	if _, err := postForm(context.Background(), "https://example.test/device", nil); err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("oversized device response: %v", err)
	}
	if _, err := New().Usage(context.Background(), store.Account{}); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("oversized billing response: %v", err)
	}
	provider.OAuthHTTPClient.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	if _, err := New().Usage(context.Background(), store.Account{}); !errors.Is(err, context.Canceled) ||
		!errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("usage cancellation classification: %v", err)
	}
}
