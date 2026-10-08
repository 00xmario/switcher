package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// fakeCreditProvider layers banked-reset behavior on the forwarding fake.
type fakeCreditProvider struct {
	*fakeProvider
	mu        sync.Mutex
	credits   []provider.ResetCredit
	listCalls int
	consumed  []string
	outcome   string
	listErr   error
}

func (f *fakeCreditProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	f.mu.Lock()
	spent := len(f.consumed)
	f.mu.Unlock()
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "Session", ResetsAt: int64(100000 + spent)}}}, nil
}

func (f *fakeCreditProvider) ListResetCredits(context.Context, store.Account) ([]provider.ResetCredit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]provider.ResetCredit(nil), f.credits...), nil
}

func (f *fakeCreditProvider) ConsumeResetCredit(_ context.Context, _ store.Account, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.consumed = append(f.consumed, id)
	return f.outcome, nil
}

func (f *fakeCreditProvider) spends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.consumed)
}

// grant gives the account one spendable banked reset.
func (f *fakeCreditProvider) grant(accountID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credits = creditFor(accountID)
}

func creditFor(accountID string) []provider.ResetCredit {
	return []provider.ResetCredit{{ID: accountID + "-credit", ExpiresAt: time.Now().Add(time.Hour).Unix()}}
}

// newCreditManager wires a provider that can bank and spend resets. on
// resolves the auto-use policy exactly like the server does.
func newCreditManager(t *testing.T, upstream *httptest.Server, on func(store.Account) bool, accounts ...store.Account) (*Manager, *fakeCreditProvider) {
	t.Helper()
	st := store.New(t.TempDir())
	for _, a := range accounts {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	prov := &fakeCreditProvider{
		fakeProvider: &fakeProvider{id: "fake", upstream: upstream, refreshToken: "refreshed-token"},
		outcome:      "reset",
	}
	m, err := New(st, map[string]provider.Provider{prov.ID(): prov}, []string{prov.ID()})
	if err != nil {
		t.Fatal(err)
	}
	m.SetAutoUseResetPolicy(on)
	return m, prov
}

// limitUntil renders the 429 body that parks an account.
func limitUntil(until time.Time) string {
	return usageLimit(until.Unix())
}

func TestAutoUseResetIsLastResortAfterFailover(t *testing.T) {
	var mu sync.Mutex
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Fake-Account")
		mu.Lock()
		served = append(served, id)
		mu.Unlock()
		if id == "a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, func(store.Account) bool { return true },
		account("a"), account("b"))
	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 after failover: %s", rec.Code, rec.Body.String())
	}
	if prov.spends() != 0 {
		t.Fatalf("failover worked but spent %d banked reset(s); credits must be kept for when they are needed", prov.spends())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(served) < 2 || served[0] != "a" || served[1] != "b" {
		t.Fatalf("served %v, want failover from a to b", served)
	}
}

func TestAutoUseResetSpendsOneWhenThereIsNowhereToFailOver(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, func(store.Account) bool { return true }, account("a"))
	prov.grant("a")
	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 after auto-using a banked reset: %s", rec.Code, rec.Body.String())
	}
	if got := prov.spends(); got != 1 {
		t.Fatalf("spent %d banked reset(s), want exactly 1", got)
	}
	if _, stillParked := m.Exhausted("a"); stillParked {
		t.Fatal("account stayed exhausted after the banked reset was redeemed")
	}
}

func TestAutoUseResetDoesNothingWhenDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, nil, account("a"))
	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 when the feature is off", rec.Code)
	}
	if prov.spends() != 0 || prov.listCalls != 0 {
		t.Fatalf("feature off but listed %d and spent %d", prov.listCalls, prov.spends())
	}
	if _, parked := m.Exhausted("a"); !parked {
		t.Fatal("account should stay parked so it is not retried every request")
	}
}

func TestAutoUseResetPerAccountOverrideBeatsGlobal(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name       string
		override   *bool
		global     bool
		wantSpends int
	}{
		{"forced on beats a global off", &yes, false, 1},
		{"forced off beats a global on", &no, true, 0},
		{"no override follows global on", nil, true, 1},
		{"no override follows global off", nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
			}))
			defer upstream.Close()

			a := account("a")
			a.AutoUseReset = tc.override
			m, prov := newCreditManager(t, upstream,
				// Mirrors main.go: the per-account override wins over the
				// global preference.
				func(acc store.Account) bool { return acc.AutoUseResetEnabled(tc.global) }, a)
			if tc.wantSpends > 0 {
				prov.grant("a")
			}
			rec := doRequest(t, m, "/v1/responses")
			if got := prov.spends(); got != tc.wantSpends {
				t.Fatalf("spent %d banked reset(s), want %d (status %d)", got, tc.wantSpends, rec.Code)
			}
		})
	}
}

func TestAutoUseResetSpendsAtMostOnePerRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, func(store.Account) bool { return true }, account("a"))
	prov.grant("a")
	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 when the reset does not clear the limit", rec.Code)
	}
	if got := prov.spends(); got != 1 {
		t.Fatalf("spent %d banked reset(s) in one request, want exactly 1", got)
	}
}

func TestAutoUseResetSkipsProvidersWithoutBankedResets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, func(store.Account) bool { return true }, account("a"))
	// Replace the credit-capable provider with a plain one mid-test: the
	// policy is on but the provider cannot bank resets.
	plain := &fakeProvider{id: "fake", upstream: upstream, refreshToken: "refreshed-token"}
	m.mu.Lock()
	m.providers["fake"] = plain
	m.mu.Unlock()

	rec := doRequest(t, m, "/v1/responses")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 for a provider without banked resets", rec.Code)
	}
	if prov.spends() != 0 || prov.listCalls != 0 {
		t.Fatalf("non-banking provider listed %d and spent %d", prov.listCalls, prov.spends())
	}
}

// TestAccountPreferencesSurviveCredentialReplacement pins the bug where a
// relogin or a re-added account rebuilt the record from scratch and silently
// reset the per-account override to "follow global".
func TestAccountPreferencesSurviveCredentialReplacement(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	yes := true
	a := account("a")
	a.AutoUseReset = &yes
	m, _ := newCreditManager(t, upstream, func(store.Account) bool { return false }, a)

	keep := func(step string) {
		t.Helper()
		saved, err := m.store.Get("a")
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if saved.AutoUseReset == nil || !*saved.AutoUseReset {
			t.Fatalf("%s dropped the per-account auto-use override", step)
		}
	}

	fresh := account("a")
	fresh.Token.AccessToken = "relogin-token"
	if err := m.ReplaceReloginAccount(&fresh, "a"); err != nil {
		t.Fatal(err)
	}
	keep("relogin")
	if got, _ := m.store.Get("a"); got.Token.AccessToken != "relogin-token" {
		t.Fatal("relogin did not replace credentials")
	}

	rebuilt := account("a")
	rebuilt.Token.AccessToken = "readded-token"
	if err := m.ReplaceAccount(rebuilt); err != nil {
		t.Fatal(err)
	}
	keep("re-adding an account")
	if got, _ := m.store.Get("a"); got.Token.AccessToken != "readded-token" {
		t.Fatal("ReplaceAccount did not replace credentials")
	}
}

func TestUpdateAccountChangesOnlyThePreference(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	m, _ := newCreditManager(t, upstream, func(store.Account) bool { return false }, account("a"))
	before, err := m.store.Get("a")
	if err != nil {
		t.Fatal(err)
	}

	no := false
	if err := m.UpdateAccount("a", func(acc *store.Account) error {
		acc.AutoUseReset = &no
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := m.store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if after.AutoUseReset == nil || *after.AutoUseReset {
		t.Fatal("UpdateAccount did not persist the override")
	}
	if after.Token.AccessToken != before.Token.AccessToken || after.Email != before.Email {
		t.Fatal("UpdateAccount must only change the preference, not credentials")
	}

	if err := m.UpdateAccount("missing-id", func(*store.Account) error { return nil }); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateAccount on a missing account: %v, want ErrNotFound", err)
	}
}

func TestAutoUseResetCooldownStopsCreditDrain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
	}))
	defer upstream.Close()

	m, prov := newCreditManager(t, upstream, func(store.Account) bool { return true }, account("a"))
	prov.grant("a")
	for i := 0; i < 3; i++ {
		if rec := doRequest(t, m, "/v1/responses"); rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status %d, want 429", i, rec.Code)
		}
	}
	if got := prov.spends(); got != 1 {
		t.Fatalf("spent %d banked reset(s) across repeated 429s, want 1 while the cooldown is active", got)
	}
}

// planned returns an account on a plan, as usage polling records it.
func planned(id, plan string) store.Account {
	a := account(id)
	a.Plan = plan
	return a
}

// TestBankedResetComesBeforeAFreeAccount covers where a request goes when
// the active paid account runs out: another paid account first, then a
// banked reset on the account that ran out, and only then a Free account.
func TestBankedResetComesBeforeAFreeAccount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		accounts   []store.Account
		autoReset  bool
		credit     bool
		wantServed []string
		wantSpends int
		wantActive string
	}{
		{"a banked reset beats a Free account", []store.Account{planned("a", "plus"), planned("b", "free")}, true, true, []string{"a", "a"}, 1, "a"},
		{"a Free account when no reset is left", []store.Account{planned("a", "plus"), planned("b", "free")}, true, false, []string{"a", "b"}, 0, "b"},
		{"a Free account when auto-use is off", []store.Account{planned("a", "plus"), planned("b", "free")}, false, true, []string{"a", "b"}, 0, "b"},
		{"another paid account beats a Free one", []store.Account{planned("a", "plus"), planned("b", "free"), planned("c", "pro")}, true, true, []string{"a", "c"}, 0, "c"},
		{"an unknown plan counts as paid", []store.Account{planned("a", "plus"), planned("b", "free"), account("c")}, true, true, []string{"a", "c"}, 0, "c"},
		{"a Free account moves to another Free one", []store.Account{planned("a", "free"), planned("b", "free")}, true, true, []string{"a", "b"}, 0, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var served []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id := r.Header.Get("X-Fake-Account")
				mu.Lock()
				served = append(served, id)
				first := len(served) == 1
				mu.Unlock()
				if id == "a" && first {
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(limitUntil(time.Now().Add(time.Hour))))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			m, prov := newCreditManager(t, upstream, func(store.Account) bool { return tc.autoReset }, tc.accounts...)
			if err := m.Activate("a"); err != nil {
				t.Fatal(err)
			}
			if tc.credit {
				prov.grant("a")
			}
			rec := doRequest(t, m, "/v1/responses")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
			}
			mu.Lock()
			got := append([]string(nil), served...)
			mu.Unlock()
			if strings.Join(got, ",") != strings.Join(tc.wantServed, ",") {
				t.Fatalf("served %v, want %v", got, tc.wantServed)
			}
			if spends := prov.spends(); spends != tc.wantSpends {
				t.Fatalf("spent %d banked reset(s), want %d", spends, tc.wantSpends)
			}
			if active := m.ActiveID("fake"); active != tc.wantActive {
				t.Fatalf("active %q, want %q", active, tc.wantActive)
			}
		})
	}
}

func TestFirstPickPrefersAPaidAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	m, _ := newCreditManager(t, upstream, nil, planned("a", "free"), planned("b", "plus"))
	if rec := doRequest(t, m, "/v1/responses"); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if active := m.ActiveID("fake"); active != "b" {
		t.Fatalf("active %q, want the paid account", active)
	}
}
