package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"switcher/internal/googleauth"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// stubGoogle points the shared Google endpoints at a test server and
// restores them when the test ends.
func stubGoogle(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldToken, oldUser := googleauth.TokenURL, googleauth.UserinfoURL
	googleauth.TokenURL, googleauth.UserinfoURL = srv.URL+"/token", srv.URL+"/userinfo"
	t.Cleanup(func() {
		googleauth.TokenURL, googleauth.UserinfoURL = oldToken, oldUser
		srv.Close()
	})
}

// stubCloudCode points the Antigravity Cloud Code endpoints at a test
// server and restores them when the test ends.
func stubCloudCode(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldBase, oldOnboard := apiBase, onboardBase
	apiBase, onboardBase = srv.URL, srv.URL
	t.Cleanup(func() {
		apiBase, onboardBase = oldBase, oldOnboard
		srv.Close()
	})
}

func TestAuthorizeURL(t *testing.T) {
	p := New()
	info, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "browser" || info.State == "" {
		t.Fatalf("unexpected info: %+v", info)
	}
	u, err := url.Parse(info.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Fatalf("unexpected authorize URL: %s", info.URL)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != ClientID ||
		q.Get("redirect_uri") != redirectURI || q.Get("scope") != Scopes ||
		q.Get("state") != info.State || q.Get("access_type") != "offline" ||
		q.Get("prompt") != "consent" {
		t.Fatalf("unexpected authorize params: %v", q)
	}
	if q.Get("code_challenge") != "" {
		t.Fatal("antigravity is a confidential client: no PKCE expected")
	}
	if len(info.State) != 32 { // 16 random bytes, hex encoded
		t.Fatalf("state = %q, want 16 bytes hex", info.State)
	}
}

func TestLoginExchange(t *testing.T) {
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("client_secret") != ClientSecret || r.PostForm.Get("code") != "abc" ||
				r.PostForm.Get("redirect_uri") != redirectURI ||
				r.PostForm.Get("grant_type") != "authorization_code" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"email":"dev@example.com"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:loadCodeAssist" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("X-Goog-Api-Client"); got != apiClient {
			t.Errorf("api client header = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("user agent = %q", got)
		}
		var body struct {
			Metadata struct {
				IdeType string `json:"ideType"`
			} `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Metadata.IdeType != "ANTIGRAVITY" {
			t.Errorf("ideType = %q", body.Metadata.IdeType)
		}
		_, _ = w.Write([]byte(`{"cloudaicompanionProject":"projects/my-proj"}`))
	})

	p := New()
	if _, err := p.LoginExchange(context.Background(), "missing", "abc"); err == nil {
		t.Fatal("exchange with an unknown state must fail")
	}
	info, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acc, err := p.LoginExchange(context.Background(), info.State, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Provider != "antigravity" || acc.Email != "dev@example.com" {
		t.Fatalf("unexpected account: %+v", acc)
	}
	if ProjectID(acc) != "my-proj" {
		t.Fatalf("project id = %q", ProjectID(acc))
	}
	if !strings.HasPrefix(acc.ID, "antigravity-") || len(acc.ID) != len("antigravity-")+8 {
		t.Fatalf("id = %q", acc.ID)
	}
	if acc.Token.AccessToken != "at" || acc.Token.RefreshToken != "rt" || acc.Token.ExpiresAt == 0 {
		t.Fatalf("unexpected token: %+v", acc.Token)
	}
}

func TestDiscoverProjectOnboarding(t *testing.T) {
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"email":"dev@example.com"}`))
		}
	})
	var onboardCalls int
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_, _ = w.Write([]byte(`{}`))
		case "/v1internal:onboardUser":
			onboardCalls++
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"tier_id":"antigravity-free"`) {
				t.Errorf("onboard body = %s", raw)
			}
			if onboardCalls < 2 {
				_, _ = w.Write([]byte(`{"done":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"done":true,"response":{"cloudaicompanionProject":"fresh-proj"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	p := New()
	info, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acc, err := p.LoginExchange(context.Background(), info.State, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if ProjectID(acc) != "fresh-proj" {
		t.Fatalf("project id = %q", ProjectID(acc))
	}
	if onboardCalls != 2 {
		t.Fatalf("onboard calls = %d, want 2 (poll until done)", onboardCalls)
	}
}

// TestLoginExchangeOnboardingFailure checks that a login whose project
// discovery fails keeps the minted tokens: the account comes back with no
// project id and Usage reports unavailable.
func TestLoginExchangeOnboardingFailure(t *testing.T) {
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"email":"dev@example.com"}`))
		}
	})
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	p := New()
	info, err := p.LoginStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acc, err := p.LoginExchange(context.Background(), info.State, "abc")
	if err != nil {
		t.Fatalf("onboarding failure must not discard the tokens: %v", err)
	}
	if acc.Provider != "antigravity" || acc.Email != "dev@example.com" {
		t.Fatalf("unexpected account: %+v", acc)
	}
	if ProjectID(acc) != "" {
		t.Fatalf("project id = %q, want empty", ProjectID(acc))
	}
	if acc.Token.AccessToken != "at" || acc.Token.RefreshToken != "rt" {
		t.Fatalf("tokens not kept: %+v", acc.Token)
	}
	if _, err := p.Usage(context.Background(), acc); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("usage err = %v, want ErrUsageUnavailable", err)
	}
}

func TestProjectIDOf(t *testing.T) {
	if got := projectIDOf("", "projects/abc/def", "other"); got != "def" {
		t.Fatalf("projectIDOf = %q", got)
	}
	if got := projectIDOf("plain"); got != "plain" {
		t.Fatalf("projectIDOf = %q", got)
	}
	if got := projectIDOf(""); got != "" {
		t.Fatalf("projectIDOf = %q", got)
	}
}

func TestRefresh(t *testing.T) {
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "rt" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at2","refresh_token":"rt2","expires_in":7200}`))
	})

	p := New()
	acc := store.Account{Provider: "antigravity", Email: "dev@example.com",
		Token: store.Token{RefreshToken: "rt"}}
	if err := p.Refresh(context.Background(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Token.AccessToken != "at2" || acc.Token.RefreshToken != "rt2" || acc.Token.ExpiresAt == 0 {
		t.Fatalf("refresh did not update the token: %+v", acc.Token)
	}
	if err := p.Refresh(context.Background(), &store.Account{}); err == nil {
		t.Fatal("refresh without a refresh token must fail")
	}
}

func TestRefreshReloginClassification(t *testing.T) {
	status, body := http.StatusBadRequest, `{"error":"invalid_grant"}`
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusUnauthorized, ``, true},
		{http.StatusForbidden, ``, false},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusServiceUnavailable, ``, false},
	} {
		status, body = tc.status, tc.body
		a := store.Account{Token: store.Token{RefreshToken: "rt"}}
		err := New().Refresh(context.Background(), &a)
		if err == nil || errors.Is(err, provider.ErrReloginRequired) != tc.want {
			t.Fatalf("status %d: err = %v, relogin = %t", status, err, tc.want)
		}
	}
	if err := New().Refresh(context.Background(), &store.Account{}); !errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("missing token: %v", err)
	}
}

func TestUsageQuotaSummary(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "proj-1") {
			t.Errorf("body = %s, want the project_id", raw)
		}
		fmt.Fprintf(w, `{"groups":[
			{"displayName":"Gemini 3 Pro","buckets":[
				{"remainingFraction":0.25,"resetTime":%q},
				{"remaining_fraction":0,"reset_at":%q}]},
			{"displayName":"NoFraction","buckets":[{"resetTime":%q}]}]}`, reset, reset, reset)
	})

	p := New()
	acc := store.Account{Provider: "antigravity", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-1"}}}
	usage, err := p.Usage(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || len(usage.Windows) != 2 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if usage.Windows[0].Label != "Gemini 3 Pro" || usage.Windows[0].UsedPercent != 75 {
		t.Fatalf("window 0 = %+v", usage.Windows[0])
	}
	if usage.Windows[1].UsedPercent != 100 {
		t.Fatalf("window 1 = %+v", usage.Windows[1])
	}
	if usage.Windows[0].ResetsAt == 0 {
		t.Fatal("reset time not parsed")
	}
}

func TestUsageModelsFallback(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:retrieveUserQuotaSummary":
			w.WriteHeader(http.StatusInternalServerError)
		case "/v1internal:fetchAvailableModels":
			fmt.Fprintf(w, `{"models":{"gemini-3-pro":{"quotaInfo":{"remaining_fraction":0.5,"resetTime":%q}}}}`, reset)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	p := New()
	acc := store.Account{Provider: "antigravity", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-1"}}}
	usage, err := p.Usage(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || len(usage.Windows) != 1 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if usage.Windows[0].Label != "gemini-3-pro" || usage.Windows[0].UsedPercent != 50 {
		t.Fatalf("window = %+v", usage.Windows[0])
	}
}

func TestUsageUnavailable(t *testing.T) {
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := New()
	acc := store.Account{Provider: "antigravity", Token: store.Token{AccessToken: "at"}}
	if _, err := p.Usage(context.Background(), acc); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("err = %v, want ErrUsageUnavailable", err)
	}
}

func TestUsageAuthStatusAcrossFallback(t *testing.T) {
	statusSummary, statusModels := http.StatusUnauthorized, http.StatusForbidden
	stubs := map[string]int{"/v1internal:retrieveUserQuotaSummary": statusSummary, "/v1internal:fetchAvailableModels": statusModels}
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("missing usage authorization")
		}
		w.WriteHeader(stubs[r.URL.Path])
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	})
	acc := store.Account{Token: store.Token{AccessToken: "secret-access", Extra: map[string]any{"project_id": "p"}}}
	for _, tc := range []struct {
		summary, models int
		auth            bool
	}{
		{http.StatusUnauthorized, http.StatusForbidden, true},
		{http.StatusForbidden, http.StatusUnauthorized, true},
		{http.StatusForbidden, http.StatusInternalServerError, false},
	} {
		stubs["/v1internal:retrieveUserQuotaSummary"], stubs["/v1internal:fetchAvailableModels"] = tc.summary, tc.models
		_, err := New().Usage(context.Background(), acc)
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != tc.auth || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("statuses %d/%d: unexpected usage error: %v", tc.summary, tc.models, err)
		}
	}
}

func TestParseRateLimit(t *testing.T) {
	far := time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)
	near := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"groups":[{"displayName":"Pro","buckets":[
			{"remainingFraction":1,"resetTime":%q},
			{"remaining_fraction":0,"reset_at":%q}]}]}`, near, far)
	})

	p := New()
	acc := store.Account{Provider: "antigravity", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-1"}}}

	until, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusTooManyRequests, nil)
	if !exhausted {
		t.Fatal("429 must mark the account exhausted")
	}
	if want := time.Now().Add(90 * time.Minute); until.Before(want.Add(-time.Minute)) || until.After(want.Add(time.Minute)) {
		t.Fatalf("parked until %v, want the latest exhausted reset around %v", until, want)
	}

	if _, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusInternalServerError, []byte(`{"error":"RESOURCE_EXHAUSTED"}`)); !exhausted {
		t.Fatal("RESOURCE_EXHAUSTED body must mark the account exhausted")
	}
	if _, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusBadRequest, nil); exhausted {
		t.Fatal("other statuses must never exhaust the account")
	}
}

func TestIsExpired(t *testing.T) {
	p := New()
	if p.IsExpired(store.Account{}) {
		t.Fatal("zero expiry must not count as expired")
	}
	soon := store.Account{Token: store.Token{ExpiresAt: time.Now().Add(10 * time.Minute).Unix()}}
	if !p.IsExpired(soon) {
		t.Fatal("a token inside the 30 minute lead must count as expired")
	}
	later := store.Account{Token: store.Token{ExpiresAt: time.Now().Add(time.Hour).Unix()}}
	if p.IsExpired(later) {
		t.Fatal("a token beyond the 30 minute lead is fresh")
	}
}

func TestUnsupportedFlows(t *testing.T) {
	p := New()
	if _, _, err := p.DeviceStart(context.Background()); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("DeviceStart err = %v", err)
	}
	if _, err := p.AddByKey(context.Background(), "k"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("AddByKey err = %v", err)
	}
}
