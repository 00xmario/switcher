package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

type recheckProvider struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

type importTestProvider struct {
	*recheckProvider
	account store.Account
}

func (p *importTestProvider) ImportFromKeychain(context.Context) (store.Account, error) {
	return p.account, nil
}

func (*recheckProvider) ID() string          { return "fake" }
func (*recheckProvider) DisplayName() string { return "Fake" }
func (*recheckProvider) LoginStart(context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{}, provider.ErrUnsupported
}
func (*recheckProvider) LoginExchange(context.Context, string, string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}
func (*recheckProvider) DeviceStart(context.Context) (provider.LoginInfo, func(context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}
func (*recheckProvider) AddByKey(context.Context, string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}
func (*recheckProvider) Refresh(context.Context, *store.Account) error { return nil }
func (*recheckProvider) IsExpired(store.Account) bool                  { return false }
func (*recheckProvider) UpstreamURL(path string) string                { return path }
func (*recheckProvider) ApplyAuth(*http.Request, store.Account) error  { return nil }
func (*recheckProvider) ParseRateLimit(context.Context, store.Account, int, []byte) (time.Time, bool) {
	return time.Time{}, false
}
func (p *recheckProvider) Usage(ctx context.Context, _ store.Account) (provider.Usage, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
	}
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "Weekly", UsedPercent: 25, ResetsAt: time.Now().Add(time.Hour).Unix()},
	}}, nil
}

func TestAccountRecheckIsAsyncAndHealthIsCoarse(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.Save(store.Account{ID: "fake-a", Provider: "fake", Email: "a@example.com",
		Token: store.Token{AccessToken: "private-token"}}); err != nil {
		t.Fatal(err)
	}
	p := &recheckProvider{entered: make(chan struct{}), release: make(chan struct{})}
	m, err := proxy.New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, Proxy: m, Providers: map[string]provider.Provider{"fake": p}}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	started := time.Now()
	resp, err := http.Post(srv.URL+"/api/accounts/fake-a/recheck", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("recheck status %d after %s, want prompt 202", resp.StatusCode, time.Since(started))
	}
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("queued check did not start")
	}
	// A second click coalesces while the first check is blocked upstream.
	resp, err = http.Post(srv.URL+"/api/accounts/fake-a/recheck", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if p.calls.Load() != 1 {
		t.Fatalf("upstream checks = %d, want one", p.calls.Load())
	}

	getHealth := func() (string, []byte) {
		t.Helper()
		res, err := http.Get(srv.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var state struct {
			Accounts []struct {
				Health struct {
					Condition string `json:"condition"`
				} `json:"health"`
			} `json:"accounts"`
		}
		var raw json.RawMessage
		if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &state); err != nil || len(state.Accounts) != 1 {
			t.Fatalf("invalid account health response: %v", err)
		}
		return state.Accounts[0].Health.Condition, raw
	}
	if status, raw := getHealth(); status != "checking" || strings.Contains(string(raw), "private-token") {
		t.Fatalf("health while blocked = %q; response leaked token: %t", status,
			strings.Contains(string(raw), "private-token"))
	}
	close(p.release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if status, _ := getHealth(); status == "usage_current" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("usage did not become current after the check completed")
}

func TestReloginImportRejectsAnotherIdentityAndKeepsTargetID(t *testing.T) {
	st := store.New(t.TempDir())
	target := store.Account{ID: "fake-a", Provider: "fake", Email: "a@example.com",
		Token: store.Token{AccessToken: "old"}}
	if err := st.Save(target); err != nil {
		t.Fatal(err)
	}
	p := &importTestProvider{recheckProvider: &recheckProvider{},
		account: store.Account{ID: "fake-b", Provider: "fake", Email: "b@example.com",
			Token: store.Token{AccessToken: "new"}}}
	m, err := proxy.New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, Proxy: m, Providers: map[string]provider.Provider{"fake": p}}
	call := func() *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/login/import",
			strings.NewReader(`{"provider":"fake","relogin_of":"fake-a"}`))
		w := httptest.NewRecorder()
		a.handleLoginImport(w, r)
		return w
	}
	if response := call(); response.Code != http.StatusConflict {
		t.Fatalf("different account status = %d, want 409", response.Code)
	}
	if _, err := st.Get("fake-b"); err == nil {
		t.Fatal("different identity was imported before validation")
	}
	if existing, err := st.Get("fake-a"); err != nil || existing.Token.AccessToken != "old" {
		t.Fatalf("target changed after mismatch: %v", err)
	}
	p.account.Email = "a@example.com"
	p.account.ID = "fake-new-id"
	response := call()
	if response.Code != http.StatusOK {
		t.Fatalf("matching account status = %d: %s", response.Code, response.Body.String())
	}
	if existing, err := st.Get("fake-a"); err != nil || existing.Token.AccessToken != "new" {
		t.Fatalf("target was not replaced in place: %v", err)
	}
	if _, err := st.Get("fake-new-id"); err == nil {
		t.Fatal("matching import minted a duplicate account")
	}
}
