package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/login"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// stubEndpoints points every Copilot flow endpoint at a test server and
// restores them when the test ends.
func stubEndpoints(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldDevice, oldToken, oldUser := deviceCodeURL, tokenURL, userURL
	oldCT, oldCU := copilotTokenURL, copilotUserURL
	deviceCodeURL, tokenURL, userURL = srv.URL+"/device/code", srv.URL+"/access_token", srv.URL+"/user"
	copilotTokenURL, copilotUserURL = srv.URL+"/copilot_internal/v2/token", srv.URL+"/copilot_internal/user"
	t.Cleanup(func() {
		deviceCodeURL, tokenURL, userURL = oldDevice, oldToken, oldUser
		copilotTokenURL, copilotUserURL = oldCT, oldCU
		srv.Close()
	})
}

// fastPoll shrinks the poll cadence so tests finish in milliseconds.
func fastPoll(t *testing.T) {
	t.Helper()
	oldMin, oldPenalty := minPollInterval, slowDownPenalty
	minPollInterval, slowDownPenalty = 10*time.Millisecond, 250*time.Millisecond
	t.Cleanup(func() { minPollInterval, slowDownPenalty = oldMin, oldPenalty })
}

func TestDeviceStartAndPollStateMachine(t *testing.T) {
	fastPoll(t)
	var tokenPolls int
	start := time.Now()
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != clientID || r.PostForm.Get("scope") != "read:user" {
				t.Errorf("device code form = %v", r.PostForm)
			}
			_, _ = w.Write([]byte(`{"device_code":"dev-123","user_code":"ABCD-1234","verification_uri":"https://github.com/login/device"}`))
		case "/access_token":
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") != deviceGrant || r.PostForm.Get("device_code") != "dev-123" {
				t.Errorf("poll form = %v", r.PostForm)
			}
			tokenPolls++
			switch tokenPolls {
			case 1:
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			case 2:
				_, _ = w.Write([]byte(`{"error":"slow_down"}`))
			default:
				_, _ = w.Write([]byte(`{"access_token":"ghu_token"}`))
			}
		case "/user":
			if got := r.Header.Get("Authorization"); got != "Bearer ghu_token" {
				t.Errorf("user authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/copilot_internal/v2/token":
			t.Error("Copilot token must be minted on demand, not during login")
			w.WriteHeader(http.StatusInternalServerError)
		case "/copilot_internal/user":
			_, _ = w.Write([]byte(`{"copilot_plan":"pro_plus"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	p := New()
	info, poll, err := p.DeviceStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Kind != "device" || info.State != "dev-123" ||
		info.VerificationURL != "https://github.com/login/device" || info.UserCode != "ABCD-1234" {
		t.Fatalf("unexpected info: %+v", info)
	}
	acc, err := poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Three polls happened: pending, slow_down (adaptive +250ms), success.
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("poll resolved in %v; the slow_down penalty was not applied", elapsed)
	}
	if acc.Provider != "copilot" || acc.Email != "octocat@copilot" || acc.Token.AccountID != "octocat" {
		t.Fatalf("unexpected account: %+v", acc)
	}
	if acc.Token.AccessToken != "" || acc.Token.RefreshToken != "ghu_token" {
		t.Fatalf("token chain wrong: %+v", acc.Token)
	}
	if !strings.HasPrefix(acc.ID, "copilot-") || len(acc.ID) != len("copilot-")+8 {
		t.Fatalf("id = %q", acc.ID)
	}
	if !p.IsExpired(acc) {
		t.Fatal("new GitHub login must mint Copilot token before forwarding")
	}
}

// The GitHub device authorization can succeed while the subsequent Copilot
// token mint rejects the request. Keep the whole login chain in this test.
func TestDeviceLoginMintsCopilotTokenWithGitHubClientHeaders(t *testing.T) {
	fastPoll(t)
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"device","user_code":"ABCD","verification_uri":"https://github.com/login/device"}`))
		case "/access_token":
			_, _ = w.Write([]byte(`{"access_token":"github-oauth-token"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/copilot_internal/v2/token":
			if r.Header.Get("Authorization") != "token github-oauth-token" ||
				r.Header.Get("Accept") != "application/vnd.github+json" ||
				r.Header.Get("X-GitHub-Api-Version") != "2025-04-01" ||
				r.Header.Get("User-Agent") == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"token":"copilot-token","expires_at":1900000000}`))
		case "/copilot_internal/user":
			_, _ = w.Write([]byte(`{"copilot_plan":"pro"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_, poll, err := New().DeviceStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	account, err := poll(context.Background())
	if err != nil {
		t.Fatalf("GitHub authorization completed but Copilot account was not added: %v", err)
	}
	if account.Token.RefreshToken != "github-oauth-token" || account.Token.AccessToken != "" {
		t.Fatal("GitHub login should persist without requiring a Copilot token")
	}
	if err := New().Refresh(context.Background(), &account); err != nil {
		t.Fatalf("deferred Copilot token exchange: %v", err)
	}
	if account.Token.AccessToken != "copilot-token" {
		t.Fatal("Copilot token was not minted on first use")
	}
}

func TestDeviceLoginSurvivesCopilotMintForbidden(t *testing.T) {
	fastPoll(t)
	mintAvailable := false
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"device","user_code":"ABCD","verification_uri":"https://github.com/login/device"}`))
		case "/access_token":
			_, _ = w.Write([]byte(`{"access_token":"github-oauth-token"}`))
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/copilot_internal/v2/token":
			if !mintAvailable {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"token":"copilot-token","expires_at":1900000000}`))
		case "/copilot_internal/user":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	st := store.New(t.TempDir())
	manager := login.New(func(account *store.Account, _ string) error { return st.Save(*account) })
	handle, err := manager.StartDevice(context.Background(), New(), "")
	if err != nil {
		t.Fatal(err)
	}
	account, err, finished := manager.Outcome(handle.State, time.Second)
	if !finished || err != nil || account.ID == "" || account.Token.RefreshToken != "github-oauth-token" {
		t.Fatalf("authorized GitHub account disappeared after Copilot 403: finished=%v error=%v", finished, err)
	}
	if saved, err := st.Get(account.ID); err != nil || saved.Token.RefreshToken != "github-oauth-token" {
		t.Fatalf("authorized Copilot account not persisted: %v", err)
	}
	if err := New().Refresh(context.Background(), &account); err == nil {
		t.Fatal("first use must still report Copilot mint failure")
	}
	mintAvailable = true
	if err := New().Refresh(context.Background(), &account); err != nil || account.Token.AccessToken != "copilot-token" {
		t.Fatalf("stored GitHub login could not recover after mint 403: %v", err)
	}
}

func TestDeviceFlowRejectsAndExpires(t *testing.T) {
	fastPoll(t)
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			_, _ = w.Write([]byte(`{"device_code":"d","user_code":"U","verification_uri":"https://github.com/login/device"}`))
		case "/access_token":
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		}
	})
	p := New()
	_, poll, err := p.DeviceStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poll(context.Background()); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("err = %v, want access_denied failure", err)
	}
}

func TestRefreshOnlyClassifiesGitHubMintUnauthorized(t *testing.T) {
	status := http.StatusUnauthorized
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token github-token" {
			t.Errorf("authorization = %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"secret":"do-not-log"}`))
	})
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, false},
		{http.StatusTooManyRequests, false},
		{http.StatusBadGateway, false},
		{http.StatusOK, false},
	} {
		status = tc.status
		a := store.Account{Token: store.Token{RefreshToken: "github-token"}}
		err := New().Refresh(context.Background(), &a)
		if err == nil || errors.Is(err, provider.ErrReloginRequired) != tc.want || strings.Contains(err.Error(), "do-not-log") {
			t.Fatalf("status %d: err = %v, relogin = %t", status, err, tc.want)
		}
	}
	if err := New().Refresh(context.Background(), &store.Account{}); !errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("missing GitHub token: %v", err)
	}
}

func TestFetchCopilotTokenExpiryFormats(t *testing.T) {
	// Unix seconds and RFC3339 strings decode to the same unix value.
	for raw, want := range map[string]int64{
		`{"token":"t","expires_at":1900000000}`:             1900000000,
		`{"token":"t","expires_at":null}`:                   0,
		`{"token":"t","expires_at":"2030-03-15T04:26:40Z"}`: 1899779200,
	} {
		var ct copilotToken
		if err := json.Unmarshal([]byte(raw), &ct); err != nil {
			t.Fatal(err)
		}
		if int64(ct.ExpiresAt) != want {
			t.Fatalf("expires_at = %d for raw %s, want %d", ct.ExpiresAt, raw, want)
		}
	}
}

func TestUsage(t *testing.T) {
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_token" {
			t.Errorf("usage authorization = %q", got)
		}
		if r.Header.Get("X-GitHub-Api-Version") != apiVersion {
			t.Errorf("api version = %q", r.Header.Get("X-GitHub-Api-Version"))
		}
		_, _ = w.Write([]byte(`{"copilot_plan":"pro_plus","access_type_sku":"copilot_plus",
			"quota_reset_date":"2026-09-25",
			"quota_snapshots":{
				"chat":{"entitlement":300,"used":150},
				"completions":{"entitlement":1000,"entitled":250},
				"premium_interactions":{"entitlement":300,"unlimited":true}}}`))
	})

	p := New()
	acc := store.Account{Provider: "copilot", Token: store.Token{RefreshToken: "ghu_token"}}
	usage, err := p.Usage(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Available || len(usage.Windows) != 3 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	if usage.Windows[0].Label != "Copilot Chat" || usage.Windows[0].UsedPercent != 50 {
		t.Fatalf("chat window = %+v", usage.Windows[0])
	}
	if usage.Windows[1].Label != "Completions" || usage.Windows[1].UsedPercent != 25 {
		t.Fatalf("completions window = %+v", usage.Windows[1])
	}
	if usage.Windows[2].Label != "Premium Requests" || usage.Windows[2].UsedPercent != 0 {
		t.Fatalf("premium window = %+v", usage.Windows[2])
	}
	midnight, err := time.ParseInLocation("2006-01-02", "2026-09-25", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Windows[0].ResetsAt != midnight.Unix() {
		t.Fatalf("reset = %d, want local midnight %d", usage.Windows[0].ResetsAt, midnight.Unix())
	}
}

func TestUsageUnavailable(t *testing.T) {
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := New()
	acc := store.Account{Provider: "copilot", Token: store.Token{RefreshToken: "ghu_token"}}
	if _, err := p.Usage(context.Background(), acc); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("err = %v, want ErrUsageUnavailable", err)
	} else if errors.Is(err, provider.ErrReloginRequired) {
		t.Fatalf("quota endpoint error classified as relogin: %v", err)
	}
	if _, err := p.Usage(context.Background(), store.Account{}); !errors.Is(err, provider.ErrUsageUnavailable) {
		t.Fatalf("err = %v, want ErrUsageUnavailable without a GitHub token", err)
	}
}

func TestUsageAuthStatus(t *testing.T) {
	status := http.StatusUnauthorized
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" || r.Header.Get("Authorization") != "Bearer secret-access" {
			t.Errorf("unexpected usage request: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"private":"do-not-log"}`))
	})
	for _, s := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		status = s
		_, err := New().Usage(context.Background(), store.Account{Token: store.Token{RefreshToken: "secret-access"}})
		if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (s == http.StatusUnauthorized) || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
			t.Fatalf("status %d: unexpected usage error: %v", s, err)
		}
	}
}

func TestRefresh(t *testing.T) {
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "token ghu_token" {
			t.Errorf("refresh authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"token":"cctok2","expires_at":1900003600}`))
	})

	p := New()
	acc := store.Account{Provider: "copilot", Token: store.Token{
		AccessToken: "cctok", RefreshToken: "ghu_token"}}
	if err := p.Refresh(context.Background(), &acc); err != nil {
		t.Fatal(err)
	}
	if acc.Token.AccessToken != "cctok2" || acc.Token.ExpiresAt != 1900003600 {
		t.Fatalf("refresh did not update the token: %+v", acc.Token)
	}
	if copilotExpiry(acc) != 1900003600 {
		t.Fatalf("copilot expiry = %d", copilotExpiry(acc))
	}

	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	if err := p.Refresh(context.Background(), &acc); err == nil {
		t.Fatal("a 401 on the token endpoint must surface as an error")
	}
}

func TestIsExpired(t *testing.T) {
	p := New()
	if !p.IsExpired(store.Account{}) {
		t.Fatal("missing Copilot token must require a mint")
	}
	soon := store.Account{Token: store.Token{AccessToken: "copilot-token",
		Extra: map[string]any{extraCopilotExpires: float64(time.Now().Add(time.Minute).Unix())}}}
	if !p.IsExpired(soon) {
		t.Fatal("a token inside the 5 minute lead must count as expired")
	}
	later := store.Account{Token: store.Token{AccessToken: "copilot-token",
		Extra: map[string]any{extraCopilotExpires: float64(time.Now().Add(time.Hour).Unix())}}}
	if p.IsExpired(later) {
		t.Fatal("a token beyond the 5 minute lead is fresh")
	}
	// After a Save/Load round trip the Extra value decodes as float64.
	raw, _ := json.Marshal(store.Account{Token: store.Token{
		Extra: map[string]any{extraCopilotExpires: int64(1900000000)}}})
	var loaded store.Account
	_ = json.Unmarshal(raw, &loaded)
	if expiry := copilotExpiry(loaded); expiry != 1900000000 {
		t.Fatalf("expiry after round trip = %d", expiry)
	}
}

func TestParseRateLimit(t *testing.T) {
	tomorrow := time.Now().Add(24 * time.Hour).Format("2006-01-02")
	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"copilot_plan":"pro","quota_reset_date":%q,
			"quota_snapshots":{"premium_interactions":{"entitlement":300,"used":300}}}`, tomorrow)
	})

	p := New()
	acc := store.Account{Provider: "copilot", Token: store.Token{RefreshToken: "ghu_token"}}

	until, exhausted := p.ParseRateLimit(context.Background(), acc,
		http.StatusTooManyRequests, []byte(`{"error":"premium_request_exceeded"}`))
	if !exhausted {
		t.Fatal("a premium 429 must mark the account exhausted")
	}
	midnight, _ := time.ParseInLocation("2006-01-02", tomorrow, time.Local)
	if !until.Equal(midnight) {
		t.Fatalf("parked until %v, want %v", until, midnight)
	}
	if _, exhausted := p.ParseRateLimit(context.Background(), acc, http.StatusBadRequest, nil); exhausted {
		t.Fatal("other statuses must never exhaust the account")
	}
	if _, exhausted := p.ParseRateLimit(context.Background(), acc,
		http.StatusTooManyRequests, []byte(`{"error":"other"}`)); exhausted {
		t.Fatal("a 429 without a quota body must not exhaust the account")
	}
}

func TestImportFromKeychain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apps.json")
	if err := os.WriteFile(path, []byte(`{"github.com":{"user":"octocat","oauth_token":"ghu_stored"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldPath := appsJSONPath
	appsJSONPath = path
	t.Cleanup(func() { appsJSONPath = oldPath })

	stubEndpoints(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/copilot_internal/v2/token":
			_, _ = w.Write([]byte(`{"token":"cctok","expires_at":1900000000}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	p := New()
	acc, err := p.ImportFromKeychain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acc.Provider != "copilot" || acc.Email != "octocat@copilot" ||
		acc.Token.RefreshToken != "ghu_stored" || acc.Token.AccessToken != "" || !p.IsExpired(acc) {
		t.Fatalf("unexpected imported account: %+v", acc)
	}

	appsJSONPath = filepath.Join(dir, "missing.json")
	if _, err := p.ImportFromKeychain(context.Background()); err == nil {
		t.Fatal("import without stored credentials must fail")
	}
}

func TestUnsupportedFlows(t *testing.T) {
	p := New()
	if _, err := p.LoginStart(context.Background()); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("LoginStart err = %v", err)
	}
	if _, err := p.LoginExchange(context.Background(), "s", "c"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("LoginExchange err = %v", err)
	}
	if _, err := p.AddByKey(context.Background(), "k"); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("AddByKey err = %v", err)
	}
}
