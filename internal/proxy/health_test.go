package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type healthProvider struct {
	*fakeProvider
	usage   func(context.Context, store.Account) (provider.Usage, error)
	refresh func(context.Context, *store.Account) error
	expired func(store.Account) bool
}

func (p *healthProvider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	return p.usage(ctx, a)
}
func (p *healthProvider) Refresh(ctx context.Context, a *store.Account) error {
	if p.refresh != nil {
		return p.refresh(ctx, a)
	}
	return p.fakeProvider.Refresh(ctx, a)
}
func (p *healthProvider) IsExpired(a store.Account) bool {
	return p.expired != nil && p.expired(a)
}

func healthFixture(t *testing.T, p *healthProvider) *Manager {
	t.Helper()
	st := store.New(t.TempDir())
	if err := st.Save(account("a")); err != nil {
		t.Fatal(err)
	}
	p.fakeProvider = &fakeProvider{id: "fake", refreshToken: "rotated"}
	m, err := New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func goodUsage() provider.Usage {
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "week", UsedPercent: 37, ResetsAt: time.Now().Add(time.Hour).Unix()}}}
}

func TestUsageRateLimitBacksOffBackgroundButAllowsManualRecheck(t *testing.T) {
	var calls atomic.Int32
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
		if calls.Add(1) == 1 {
			return provider.Usage{}, provider.UsageStatusError(http.StatusTooManyRequests)
		}
		return goodUsage(), nil
	}}
	m := healthFixture(t, p)
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "usage_unavailable")
	firstChecked := m.AccountHealth("a").LastChecked
	m.RefreshUsageAll(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("background retried a throttled usage endpoint: %d requests", got)
	}
	if m.AccountHealth("a").LastChecked != firstChecked {
		t.Fatal("skipping a throttled endpoint falsely reported a new check")
	}
	if err := m.QueueRecheck("a"); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, m, "usage_current")
	if got := calls.Load(); got != 2 {
		t.Fatalf("manual Recheck did not bypass the cooldown: %d requests", got)
	}
	m.RefreshUsageAll(context.Background())
	if got := calls.Load(); got != 3 {
		t.Fatalf("successful Recheck did not clear cooldown: %d requests", got)
	}
}

func TestUsageRateLimitCooldownDropsRolledSnapshot(t *testing.T) {
	var calls atomic.Int32
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
		calls.Add(1)
		return provider.Usage{}, provider.UsageStatusError(http.StatusTooManyRequests)
	}}
	m := healthFixture(t, p)
	m.mu.Lock()
	m.lastUsage["a"] = goodUsage()
	m.healthLocked("a").lastSuccess = time.Now().Add(-6 * time.Minute)
	m.mu.Unlock()
	m.RefreshUsageAll(context.Background())
	m.mu.Lock()
	m.lastUsage["a"] = provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "week", UsedPercent: 100, ResetsAt: time.Now().Add(-time.Minute).Unix()},
	}}
	m.mu.Unlock()
	m.RefreshUsageAll(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("cooldown sent %d quota requests, want one", calls.Load())
	}
	if _, ok := m.LastUsage("a"); ok {
		t.Fatal("rolled snapshot remained visible during quota cooldown")
	}
	assertCondition(t, m, "usage_unavailable")
}

func TestReloginReplacementCannotRecreateDeletedAccount(t *testing.T) {
	m := healthFixture(t, &healthProvider{})
	for i := 0; i < 20; i++ {
		if err := m.ReplaceAccount(account("a")); err != nil {
			t.Fatal(err)
		}
		incoming := account("a")
		incoming.ID = "import-id"
		start := make(chan struct{})
		var wg sync.WaitGroup
		var replaceErr, deleteErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			replaceErr = m.ReplaceReloginAccount(&incoming, "a")
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteErr = m.DeleteAccount("a")
		}()
		close(start)
		wg.Wait()
		if deleteErr != nil {
			t.Fatalf("delete: %v", deleteErr)
		}
		if replaceErr != nil && !errors.Is(replaceErr, ErrReloginTargetUnavailable) {
			t.Fatalf("replace: %v", replaceErr)
		}
		if _, err := m.store.Get("a"); err == nil {
			t.Fatal("relogin recreated deleted account")
		}
		if _, err := m.store.Get("import-id"); err == nil {
			t.Fatal("relogin saved under a different ID")
		}
	}
}

func assertCondition(t *testing.T, m *Manager, want string) {
	t.Helper()
	if got := m.AccountHealth("a").Condition; got != want {
		t.Fatalf("health condition = %q, want %q", got, want)
	}
}

func awaitCondition(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.AccountHealth("a").Condition == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	assertCondition(t, m, want)
}

func TestHealthCurrentStaleAndJSON(t *testing.T) {
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil }}
	m := healthFixture(t, p)
	assertCondition(t, m, "checking")
	if got := m.RefreshUsage(context.Background(), account("a")); !got.Available {
		t.Fatal("expected available usage")
	}
	assertCondition(t, m, "usage_current")
	h := m.AccountHealth("a")
	if h.LastChecked == 0 || h.LastUsageSuccess == 0 {
		t.Fatalf("missing timestamps: %+v", h)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "" || !json.Valid(b) {
		t.Fatalf("invalid health JSON: %s", b)
	}
	m.mu.Lock()
	m.health["a"].lastSuccess = time.Now().Add(-6 * time.Minute)
	m.mu.Unlock()
	assertCondition(t, m, "usage_stale")
}

func TestHealthFailureAndLastGoodUsage(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
		if fail.Load() {
			return provider.Usage{}, provider.ErrUsageUnavailable
		}
		return goodUsage(), nil
	}}
	m := healthFixture(t, p)
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "usage_unavailable")
	if _, ok := m.LastUsage("a"); ok {
		t.Fatal("failure stored an empty snapshot")
	}
	fail.Store(false)
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "usage_current")
	fail.Store(true)
	got := m.RefreshUsage(context.Background(), account("a"))
	if !got.Available {
		t.Fatal("manual refresh lost last good usage")
	}
	if cached, ok := m.LastUsage("a"); !ok || !cached.Available {
		t.Fatal("transient failure cleared cached usage")
	}
	assertCondition(t, m, "usage_current")
	if m.AccountHealth("a").LastChecked < m.AccountHealth("a").LastUsageSuccess {
		t.Fatal("last check predates last success")
	}
	// Once all quota windows have rolled over, the old snapshot is unusable.
	m.mu.Lock()
	m.lastUsage["a"] = provider.Usage{Available: true, Windows: []provider.UsageWindow{{ResetsAt: time.Now().Add(-time.Minute).Unix()}}}
	m.mu.Unlock()
	m.RefreshUsageAll(context.Background())
	if _, ok := m.LastUsage("a"); ok {
		t.Fatal("expired cached window survived failure")
	}
	assertCondition(t, m, "usage_unavailable")
}

func TestReloginStickyUntilRefreshAndQueueRecovery(t *testing.T) {
	var rejected atomic.Bool
	rejected.Store(true)
	p := &healthProvider{
		expired: func(store.Account) bool { return true },
		refresh: func(_ context.Context, a *store.Account) error {
			if rejected.Load() {
				return fmt.Errorf("credential: %w", provider.ErrReloginRequired)
			}
			a.Token.AccessToken = "new"
			return nil
		},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
	}
	m := healthFixture(t, p)
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "needs_relogin")
	if m.AccountHealth("a").LastChecked == 0 {
		t.Fatal("failed refresh did not record a check")
	}
	// Even a successful usage read alone cannot clear credential rejection.
	p.expired = func(store.Account) bool { return false }
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "needs_relogin")
	rejected.Store(false)
	p.expired = func(store.Account) bool { return true }
	if err := m.QueueRecheck("a"); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, m, "checking")
	awaitCondition(t, m, "usage_current")
	if _, err := json.Marshal(m.AccountHealth("a")); err != nil {
		t.Fatal(err)
	}
	rejected.Store(true)
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "needs_relogin")
	m.ResetAccount("a")
	assertCondition(t, m, "checking")
	if _, ok := m.LastUsage("a"); ok {
		t.Fatal("reset retained usage")
	}
}

func TestQueueRecheckCoalescesWhileBlocked(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return goodUsage(), nil
	}}
	m := healthFixture(t, p)
	if err := m.QueueRecheck("missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing account error = %v", err)
	}
	if err := m.QueueRecheck("a"); err != nil {
		t.Fatal(err)
	}
	<-entered
	assertCondition(t, m, "checking")
	for i := 0; i < 4; i++ {
		if err := m.QueueRecheck("a"); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	awaitCondition(t, m, "usage_current")
	if got := calls.Load(); got != 1 {
		t.Fatalf("usage calls = %d, want one", got)
	}
}

func TestDeleteDuringCheckAndResetRejectLateResults(t *testing.T) {
	for _, operation := range []string{"delete", "reset"} {
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			finished := make(chan struct{})
			p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
				close(entered)
				<-release
				return goodUsage(), nil
			}}
			m := healthFixture(t, p)
			go func() { m.RefreshUsageAll(context.Background()); close(finished) }()
			<-entered
			if operation == "delete" {
				deleted := make(chan error, 1)
				go func() { deleted <- m.DeleteAccount("a") }()
				close(release)
				if err := <-deleted; err != nil {
					t.Fatal(err)
				}
			} else {
				m.ResetAccount("a")
				close(release)
			}
			<-finished
			if _, ok := m.LastUsage("a"); ok {
				t.Fatal("late check restored cached usage")
			}
			assertCondition(t, m, "checking")
			if operation == "delete" {
				if _, err := m.store.Get("a"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("account resurrected: %v", err)
				}
			}
		})
	}
}

func TestReplaceAccountSerializesTokenRefresh(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	p := &healthProvider{
		expired: func(store.Account) bool { return true },
		refresh: func(_ context.Context, a *store.Account) error {
			close(entered)
			<-release
			a.Token.AccessToken = "old-rotated"
			return nil
		},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
	}
	m := healthFixture(t, p)
	finished := make(chan struct{})
	go func() { m.RefreshUsageAll(context.Background()); close(finished) }()
	<-entered
	replaced := make(chan error, 1)
	newAccount := account("a")
	newAccount.Token.AccessToken = "new-login"
	go func() { replaced <- m.ReplaceAccount(newAccount) }()
	close(release)
	<-finished
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
	saved, err := m.store.Get("a")
	if err != nil || saved.Token.AccessToken != "new-login" {
		t.Fatalf("replacement token = %q, err %v", saved.Token.AccessToken, err)
	}
	if _, ok := m.LastUsage("a"); ok {
		t.Fatal("replacement retained old account usage")
	}
}

func TestProxyRefreshFailureReturnsWithoutDeadlock(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer upstream.Close()
			p := &healthProvider{
				expired: func(store.Account) bool { return expired },
				refresh: func(context.Context, *store.Account) error {
					return fmt.Errorf("rejected: %w", provider.ErrReloginRequired)
				},
				usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
			}
			m := healthFixture(t, p)
			p.fakeProvider.upstream = upstream
			done := make(chan int, 1)
			go func() { done <- doRequest(t, m, "/v1/responses").Code }()
			select {
			case status := <-done:
				if status != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503", status)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("proxy refresh failure deadlocked")
			}
			assertCondition(t, m, "needs_relogin")
		})
	}
}

func TestProxy401RefreshSerializesWithReplacement(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer a-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p := &healthProvider{
		refresh: func(_ context.Context, a *store.Account) error {
			close(entered)
			<-release
			a.Token.AccessToken = "old-rotated"
			return nil
		},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
	}
	m := healthFixture(t, p)
	p.fakeProvider.upstream = upstream
	done := make(chan int, 1)
	go func() { done <- doRequest(t, m, "/v1/responses").Code }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not reach refresh")
	}
	a := account("a")
	a.Token.AccessToken = "new-login"
	replaced := make(chan error, 1)
	go func() { replaced <- m.ReplaceAccount(a) }()
	close(release)
	if err := <-replaced; err != nil {
		t.Fatal(err)
	}
	if status := <-done; status != http.StatusOK {
		t.Fatalf("proxy status = %d", status)
	}
	saved, err := m.store.Get("a")
	if err != nil || saved.Token.AccessToken != "new-login" {
		t.Fatalf("replacement token = %q, err %v", saved.Token.AccessToken, err)
	}
}

func TestRefreshSaveFailureKeepsReloginAndLastGoodUsage(t *testing.T) {
	var failSave atomic.Bool
	p := &healthProvider{
		expired: func(store.Account) bool { return true },
		refresh: func(_ context.Context, a *store.Account) error {
			a.Token.AccessToken = "rotated"
			if failSave.Load() {
				// A value JSON cannot encode makes Store.Save fail without changing the disk account.
				a.Token.Extra = map[string]any{"bad": make(chan int)}
			}
			return nil
		},
		usage: func(_ context.Context, a store.Account) (provider.Usage, error) {
			if a.Token.AccessToken == "rotated" {
				return goodUsage(), nil
			}
			return provider.Usage{}, provider.ErrUsageUnavailable
		},
	}
	m := healthFixture(t, p)
	m.mu.Lock()
	m.healthLocked("a").relogin = true
	m.lastUsage["a"] = goodUsage()
	m.health["a"].lastSuccess = time.Now()
	m.mu.Unlock()
	failSave.Store(true)
	if got := m.RefreshUsage(context.Background(), account("a")); !got.Available {
		t.Fatal("lost last good usage")
	}
	assertCondition(t, m, "needs_relogin")
	saved, err := m.store.Get("a")
	if err != nil || saved.Token.AccessToken != "a-token" {
		t.Fatalf("stored access token = %q, err %v", saved.Token.AccessToken, err)
	}
	failSave.Store(false)
	m.RefreshUsage(context.Background(), account("a"))
	assertCondition(t, m, "usage_current")
}

func TestProxySaveFailureFallsBackWithoutForwardingRotatedToken(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			var forwarded atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				forwarded.Add(1)
				if r.Header.Get("X-Fake-Account") == "a" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			var refreshed atomic.Int32
			p := &healthProvider{
				expired: func(a store.Account) bool { return expired && a.ID == "a" },
				refresh: func(_ context.Context, a *store.Account) error {
					refreshed.Add(1)
					a.Token.AccessToken = "rotated"
					a.Token.Extra = map[string]any{"bad": make(chan int)}
					return nil
				},
				usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
			}
			m := healthFixture(t, p)
			p.fakeProvider.upstream = upstream
			if err := m.store.Save(account("b")); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			m.healthLocked("a").relogin = true
			m.mu.Unlock()
			if status := doRequest(t, m, "/v1/responses").Code; status != http.StatusOK {
				t.Fatalf("fallback status = %d, want 200", status)
			}
			wantForwarded := int32(1)
			if !expired {
				wantForwarded = 2 // initial 401, then account b
			}
			if got := forwarded.Load(); got != wantForwarded {
				t.Fatalf("forwarded %d requests, want %d", got, wantForwarded)
			}
			if got := refreshed.Load(); got != 1 {
				t.Fatalf("refresh attempts = %d, want 1", got)
			}
			if got := m.ActiveID("fake"); got != "b" {
				t.Fatalf("active = %q, want b", got)
			}
			assertCondition(t, m, "needs_relogin")
			saved, err := m.store.Get("a")
			if err != nil || saved.Token.AccessToken != "a-token" {
				t.Fatalf("stored token = %q, err %v", saved.Token.AccessToken, err)
			}
		})
	}
}

func TestProxySaveFailureWithNoFallbackStopsAfterOneRefresh(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("forwarded a token whose save failed")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	var calls atomic.Int32
	p := &healthProvider{
		expired: func(store.Account) bool { return true },
		refresh: func(_ context.Context, a *store.Account) error {
			calls.Add(1)
			a.Token.AccessToken = "rotated"
			a.Token.Extra = map[string]any{"bad": make(chan int)}
			return nil
		},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
	}
	m := healthFixture(t, p)
	p.fakeProvider.upstream = upstream
	if status := doRequest(t, m, "/v1/responses").Code; status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh attempts = %d, want 1", got)
	}
	if _, parked := m.Exhausted("a"); !parked {
		t.Fatal("account was not parked after failed token save")
	}
}

func TestUsageRefreshOnlyOnAuthenticationFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		available   bool
		wantRefresh int32
	}{
		{"outage", provider.ErrUsageUnavailable, false, 0},
		{"unavailable snapshot", nil, false, 0},
		{"body mentions 401", fmt.Errorf("%w: upstream status 503: body says 401", provider.ErrUsageUnavailable), false, 0},
		{"typed usage 401", errors.Join(provider.ErrUsageUnavailable, provider.ErrUsageAuthRequired), false, 1},
		{"untyped 401 text", fmt.Errorf("%w: upstream status 401: rejected", provider.ErrUsageUnavailable), false, 0},
		{"token-refresh sentinel is not usage auth", fmt.Errorf("usage: %w", provider.ErrReloginRequired), false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			p := &healthProvider{
				refresh: func(_ context.Context, a *store.Account) error {
					calls.Add(1)
					a.Token.AccessToken = "rotated"
					return nil
				},
				usage: func(_ context.Context, a store.Account) (provider.Usage, error) {
					if a.Token.AccessToken == "rotated" {
						return goodUsage(), nil
					}
					return provider.Usage{Available: tc.available}, tc.err
				},
			}
			m := healthFixture(t, p)
			m.RefreshUsageAll(context.Background())
			if got := calls.Load(); got != tc.wantRefresh {
				t.Fatalf("refreshes = %d, want %d", got, tc.wantRefresh)
			}
			if tc.wantRefresh == 0 {
				assertCondition(t, m, "usage_unavailable")
			} else {
				assertCondition(t, m, "usage_current")
			}
		})
	}
}

func TestQueueRecheckForcesReloginWithUnexpiredToken(t *testing.T) {
	var calls atomic.Int32
	p := &healthProvider{
		refresh: func(_ context.Context, a *store.Account) error {
			calls.Add(1)
			a.Token.AccessToken = "rotated"
			return nil
		},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil },
	}
	m := healthFixture(t, p)
	m.mu.Lock()
	m.healthLocked("a").relogin = true
	m.mu.Unlock()
	m.RefreshUsageAll(context.Background())
	assertCondition(t, m, "needs_relogin")
	if got := calls.Load(); got != 0 {
		t.Fatalf("background refreshes = %d, want 0", got)
	}
	if err := m.QueueRecheck("a"); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, m, "usage_current")
	if got := calls.Load(); got != 1 {
		t.Fatalf("forced refreshes = %d, want 1", got)
	}
}
