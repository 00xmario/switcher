package gemini

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

// stubCloudCode points the Gemini Cloud Code endpoints at a test server
// and restores them when the test ends.
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
		t.Fatal("gemini is a confidential client: no PKCE expected")
	}
	if len(info.State) != 32 {
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
		var body struct {
			Metadata struct {
				IdeType string `json:"ideType"`
			} `json:"metadata"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Metadata.IdeType != "IDE_UNSPECIFIED" {
			t.Errorf("ideType = %q", body.Metadata.IdeType)
		}
		_, _ = w.Write([]byte(`{"cloudaicompanionProject":"projects/my-proj"}`))
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
	if acc.Provider != "gemini" || acc.Email != "dev@example.com" {
		t.Fatalf("unexpected account: %+v", acc)
	}
	if ProjectID(acc) != "my-proj" {
		t.Fatalf("project id = %q", ProjectID(acc))
	}
	if !strings.HasPrefix(acc.ID, "gemini-") || len(acc.ID) != len("gemini-")+8 {
		t.Fatalf("id = %q", acc.ID)
	}
	if acc.Token.AccessToken != "at" || acc.Token.RefreshToken != "rt" || acc.Token.ExpiresAt == 0 {
		t.Fatalf("unexpected token: %+v", acc.Token)
	}
	if _, err := p.LoginExchange(context.Background(), info.State, "abc"); err == nil {
		t.Fatal("a state must be single use")
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
			if !strings.Contains(string(raw), `"tier_id":"gemini-free"`) {
				t.Errorf("onboard body = %s", raw)
			}
			if got := r.Header.Get("User-Agent"); got != onboardUserAgent {
				t.Errorf("user agent = %q", got)
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
	if acc.Provider != "gemini" || acc.Email != "dev@example.com" {
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

func TestRefresh(t *testing.T) {
	stubGoogle(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "rt" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at2","expires_in":7200}`))
	})

	p := New()
	acc := store.Account{Provider: "gemini", Email: "dev@example.com",
		Token: store.Token{RefreshToken: "rt", Extra: map[string]any{"project_id": "p"}}}
	if err := p.Refresh(context.Background(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Token.AccessToken != "at2" || acc.Token.ExpiresAt == 0 {
		t.Fatalf("refresh did not update the token: %+v", acc.Token)
	}
	if ProjectID(acc) != "p" {
		t.Fatal("refresh must keep Extra untouched")
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
		if !strings.Contains(string(raw), "proj-9") {
			t.Errorf("body = %s, want the project_id", raw)
		}
		fmt.Fprintf(w, `{"groups":[{"displayName":"Gemini 2.5 Pro","buckets":[
			{"remaining_fraction":0.5,"reset_at":%q}]}]}`, reset)
	})

	p := New()
	acc := store.Account{Provider: "gemini", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-9"}}}
	usage, err := p.Usage(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || len(usage.Windows) != 1 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if usage.Windows[0].Label != "Gemini 2.5 Pro" {
		t.Fatalf("label = %q", usage.Windows[0].Label)
	}
}

func TestUsageWithoutGroups(t *testing.T) {
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	p := New()
	acc := store.Account{Provider: "gemini", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-9"}}}
	if _, err := p.Usage(context.Background(), acc); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("err = %v, want ErrUsageUnavailable", err)
	}

	noProject := store.Account{Provider: "gemini", Token: store.Token{AccessToken: "at"}}
	if _, err := p.Usage(context.Background(), noProject); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("err = %v, want ErrUsageUnavailable without a project", err)
	}
}

func TestParseRateLimit(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	stubCloudCode(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"groups":[{"displayName":"Pro","buckets":[
			{"remaining_fraction":0,"reset_at":%q}]}]}`, reset)
	})

	p := New()
	acc := store.Account{Provider: "gemini", Token: store.Token{
		AccessToken: "at", Extra: map[string]any{"project_id": "proj-9"}}}

	until, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusTooManyRequests, nil)
	if !exhausted {
		t.Fatal("429 must mark the account exhausted")
	}
	if want := time.Now().Add(time.Hour); until.Before(want.Add(-time.Minute)) || until.After(want.Add(time.Minute)) {
		t.Fatalf("parked until %v, want around %v", until, want)
	}
	if _, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusBadRequest, nil); exhausted {
		t.Fatal("other statuses must never exhaust the account")
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
