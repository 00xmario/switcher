package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func TestManualResetRetryIsPinnedAfterQuotaWorkerCompletes(t *testing.T) {
	m, p := newCreditManager(t, nil, nil, account("a"))
	p.credits = []provider.ResetCredit{{ID: "first"}, {ID: "second"}}
	if outcome, err := m.UseBankedReset(context.Background(), "a", "first"); err != nil || outcome != "reset" {
		t.Fatalf("first: %s %v", outcome, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for m.LastReset("a").Pending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.LastReset("a").Pending {
		t.Fatal("fixture quota worker did not finish")
	}
	if outcome, err := m.UseBankedReset(context.Background(), "a", "first"); err != nil || outcome != "already_redeemed" {
		t.Fatalf("retry: %s %v", outcome, err)
	}
	if p.spends() != 1 {
		t.Fatal("retry spent a second credit")
	}
	if outcome, err := m.UseBankedReset(context.Background(), "a", "second"); err != nil || outcome != "reset" {
		t.Fatalf("explicit next credit: %s %v", outcome, err)
	}
	if p.spends() != 2 {
		t.Fatal("explicit next credit was not spent")
	}
	if got := m.AvailableResetCredits("a", p.credits); len(got) != 0 {
		t.Fatal("lagging provider list resurrected spent credits")
	}
}

func TestPreReset429RetriesWithoutReparkingRestoredAccount(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
			w.WriteHeader(429)
			_, _ = w.Write([]byte(usageLimit(time.Now().Add(time.Hour).Unix())))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	m, p := newCreditManager(t, upstream, nil, account("a"))
	p.grant("a")
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- doRequest(t, m, "/v1/responses") }()
	<-started
	if _, err := m.UseBankedReset(context.Background(), "a", "a-credit"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	response := <-result
	if response.Code != 200 {
		t.Fatalf("stale pre-reset 429 was not retried: %d", response.Code)
	}
	if _, parked := m.Exhausted("a"); parked {
		t.Fatal("stale response re-parked the restored account")
	}
	if p.spends() != 1 {
		t.Fatal("stale response caused another credit spend")
	}
}

func TestConcurrentManualClicksSpendPinnedCreditOnce(t *testing.T) {
	m, p := newCreditManager(t, nil, nil, account("a"))
	p.grant("a")
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := m.UseBankedReset(context.Background(), "a", "a-credit"); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if p.spends() != 1 {
		t.Fatalf("concurrent clicks spent %d credits", p.spends())
	}
}

func TestAutomaticFollowerRetriesAlreadyRestoredAccount(t *testing.T) {
	m, p := newCreditManager(t, nil, func(store.Account) bool { return true }, account("a"))
	p.grant("a")
	m.markExhausted("a", time.Now().Add(time.Hour))
	if !m.autoUseBankedReset(context.Background(), p, account("a")) {
		t.Fatal("first reset failed")
	}
	if !m.autoUseBankedReset(context.Background(), p, account("a")) {
		t.Fatal("follower rejected an account whose park was already lifted")
	}
	if p.spends() != 1 {
		t.Fatal("follower spent another credit")
	}
}

func TestParkCannotFollowAConcurrentRedemption(t *testing.T) {
	m, p := newCreditManager(t, nil, nil, account("a"))
	p.grant("a")
	expected := m.resetID("a")
	if _, err := m.UseBankedReset(context.Background(), "a", "a-credit"); err != nil {
		t.Fatal(err)
	}
	if m.markExhaustedIfResetUnchanged("a", time.Now().Add(time.Hour), expected) {
		t.Fatal("stale reset epoch was permitted to park account")
	}
	if _, parked := m.Exhausted("a"); parked {
		t.Fatal("redemption was undone by a late 429")
	}
}

func TestQuotaRemovalAdvancesPublishedRevision(t *testing.T) {
	m := newManager(t, nil, account("a"))
	stale := provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "Session", UsedPercent: 100, ResetsAt: 1}}}
	unrolled := provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "Session", UsedPercent: 100, ResetsAt: time.Now().Add(time.Hour).Unix()}}}
	m.recordUsage("a", 0, unrolled, true, nil)
	_, _, before, _ := m.QuotaSnapshot("a")
	m.mu.Lock()
	m.lastUsage["a"] = stale // fixture simulates the provider-reported rollover
	m.mu.Unlock()
	m.recordUsage("a", 0, provider.Usage{}, false, provider.ErrUsageUnavailable)
	usage, _, after, _ := m.QuotaSnapshot("a")
	if usage != nil || after <= before {
		t.Fatal("removing an expired snapshot did not advance revision")
	}
	m.recordUsage("a", 0, unrolled, true, nil)
	_, _, before, _ = m.QuotaSnapshot("a")
	m.mu.Lock()
	m.lastUsage["a"] = stale
	m.healthLocked("a").retryAt = time.Now().Add(time.Hour)
	m.mu.Unlock()
	m.refreshAccount(context.Background(), "a", 0, false)
	usage, _, after, _ = m.QuotaSnapshot("a")
	if usage != nil || after <= before {
		t.Fatal("cooldown stale-snapshot removal did not advance revision")
	}
}
