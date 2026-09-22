package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// fakeProvider forwards to a local fake upstream. ApplyAuth stamps the
// account identity into a header so the upstream can report which
// credential served each request.
type fakeProvider struct {
	id       string
	upstream *httptest.Server
	// refreshToken is handed out by Refresh so tests can verify the
	// refresh path end to end.
	refreshToken string
}

func (f *fakeProvider) ID() string          { return f.id }
func (f *fakeProvider) DisplayName() string { return f.id }

func (f *fakeProvider) LoginStart(context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{}, nil
}

func (f *fakeProvider) LoginExchange(context.Context, string, string) (store.Account, error) {
	return store.Account{}, nil
}

func (f *fakeProvider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}

func (f *fakeProvider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// ParseRateLimit stamps exhaustion onto any 429 from the fake upstream,
// unless the test disabled switching (plain rate limit case).
func (f *fakeProvider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	var parsed struct {
		Error struct {
			Type     string `json:"type"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &parsed) != nil {
		return time.Time{}, false
	}
	if parsed.Error.Type != "usage_limit_reached" {
		return time.Time{}, false
	}
	if parsed.Error.ResetsAt > 0 {
		return time.Unix(parsed.Error.ResetsAt, 0), true
	}
	return time.Now().Add(time.Hour), true
}

func (f *fakeProvider) Refresh(_ context.Context, a *store.Account) error {
	a.Token.AccessToken = f.refreshToken
	a.Token.ExpiresAt = time.Now().Add(time.Hour).Unix()
	return nil
}

func (f *fakeProvider) IsExpired(store.Account) bool { return false }

func (f *fakeProvider) UpstreamURL(path string) string {
	return f.upstream.URL + strings.TrimPrefix(path, "/v1")
}

func (f *fakeProvider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("X-Fake-Account", a.ID)
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	return nil
}

func (f *fakeProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{}, provider.ErrUsageUnavailable
}

func account(id string) store.Account {
	return store.Account{
		ID:       id,
		Provider: "fake",
		Email:    id + "@example.com",
		Token:    store.Token{AccessToken: id + "-token"},
	}
}

func newManager(t *testing.T, upstream *httptest.Server, accounts ...store.Account) *Manager {
	t.Helper()
	st := store.New(t.TempDir())
	prov := &fakeProvider{id: "fake", upstream: upstream, refreshToken: "refreshed-token"}
	for _, a := range accounts {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(st, map[string]provider.Provider{prov.ID(): prov}, []string{prov.ID()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func doRequest(t *testing.T, m *Manager, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/fake"+path, strings.NewReader(`{"x":1}`))
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec
}

// usageLimit renders a 429 body the codex upstream produces when a
// subscription is out of usage.
func usageLimit(resetsAt int64) string {
	return fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, resetsAt)
}

func TestSwitchesAccountOnUsageLimitAndRetries(t *testing.T) {
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = append(served, r.Header.Get("X-Fake-Account"))
		if r.Header.Get("X-Fake-Account") == "a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(usageLimit(4102444800))) // far future: Jan 2100
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	m := newManager(t, upstream, account("a"), account("b"))

	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got, want := strings.Join(served, ","), "a,b"; got != want {
		t.Fatalf("requests served by %q, want %q", got, want)
	}
	if active := m.ActiveID("fake"); active != "b" {
		t.Fatalf("active account = %q, want b", active)
	}
	if until, ok := m.Exhausted("a"); !ok || until.Before(time.Now()) {
		t.Fatalf("account a should be exhausted into the future, got %v ok=%v", until, ok)
	}
}

func TestNoSwitchWhenEveryAccountIsExhausted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(usageLimit(4102444800)))
	}))
	defer upstream.Close()

	m := newManager(t, upstream, account("a"), account("b"))

	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type == "" {
		t.Fatalf("expected a usage-limit error body, got %s", rec.Body.String())
	}
	if until, ok := m.Exhausted("a"); !ok || until.Before(time.Now()) {
		t.Fatalf("account a should be marked exhausted, got %v ok=%v", until, ok)
	}
}

func TestPlainRateLimitDoesNotSwitch(t *testing.T) {
	// A 429 that is NOT a usage-limit response (e.g. a burst rate limit)
	// must not burn the account or trigger switching.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_exceeded","message":"slow down"}}`))
	}))
	defer upstream.Close()

	m := newManager(t, upstream, account("a"), account("b"))
	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 passthrough", rec.Code)
	}
	if active := m.ActiveID("fake"); active != "a" {
		t.Fatalf("active = %q, want unchanged \"a\"", active)
	}
}

func TestRefreshOnUnauthorized(t *testing.T) {
	var attempts int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	a := account("a")
	m := newManager(t, upstream, a)
	// Simulate an expired token so pick() triggers a refresh first.
	a.Token.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := m.store.Save(a); err != nil {
		t.Fatal(err)
	}

	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	saved, err := m.store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Token.AccessToken != "refreshed-token" {
		t.Fatalf("refreshed token not persisted, got %q", saved.Token.AccessToken)
	}
}

func TestFirstAccountBecomesActiveWithoutExplicitPick(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	m := newManager(t, upstream, account("b"), account("a"))
	if m.ActiveID("fake") != "" {
		t.Fatalf("expected no active account before first request, got %q", m.ActiveID("fake"))
	}
	_ = doRequest(t, m, "/v1/responses")
	// List() sorts accounts by email, so "a" is the first usable account.
	if m.ActiveID("fake") != "a" {
		t.Fatalf("active = %q, want first usable account \"a\"", m.ActiveID("fake"))
	}
}
