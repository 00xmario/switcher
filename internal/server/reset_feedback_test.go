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

type feedbackProvider struct {
	*recheckProvider
	usageCalls   atomic.Int32
	listCalls    atomic.Int32
	releaseUsage chan struct{}
	releaseList  chan struct{}
	listStarted  chan struct{}
}

func (p *feedbackProvider) ListResetCredits(ctx context.Context, a store.Account) ([]provider.ResetCredit, error) {
	p.listCalls.Add(1)
	if p.listStarted != nil {
		p.listStarted <- struct{}{}
	}
	if p.releaseList != nil {
		select {
		case <-p.releaseList:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Simulate the provider's list briefly lagging behind a redemption.
	return []provider.ResetCredit{{ID: "credit-one", ExpiresAt: time.Now().Add(time.Hour).Unix()}}, nil
}
func (*feedbackProvider) ConsumeResetCredit(context.Context, store.Account, string) (string, error) {
	return "reset", nil
}
func (p *feedbackProvider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	used := 100
	if p.usageCalls.Add(1) > 1 {
		select {
		case <-p.releaseUsage:
		case <-ctx.Done():
			return provider.Usage{}, ctx.Err()
		}
		used = 5
	}
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "Session", UsedPercent: used}}}, nil
}

func TestResetRepliesBeforeQuotaFetchAndRemovesCreditImmediately(t *testing.T) {
	st := store.New(t.TempDir())
	account := store.Account{ID: "fake-a", Provider: "fake", Email: "fixture@example.test"}
	if err := st.Save(account); err != nil {
		t.Fatal(err)
	}
	p := &feedbackProvider{recheckProvider: &recheckProvider{}, releaseUsage: make(chan struct{})}
	defer func() {
		select {
		case <-p.releaseUsage:
		default:
			close(p.releaseUsage)
		}
	}()
	manager, err := proxy.New(st, map[string]provider.Provider{"fake": p}, []string{"fake"})
	if err != nil {
		t.Fatal(err)
	}
	manager.RefreshUsage(context.Background(), account)
	a := &API{Store: st, Proxy: manager, Providers: map[string]provider.Provider{"fake": p}}
	mux := http.NewServeMux()
	a.Register(mux)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/accounts/fake-a/use-reset", strings.NewReader(`{"credit_id":"credit-one"}`)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("redemption response waited for blocked quota endpoint")
	}
	if response.Code != 200 {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
	var body struct {
		Outcome string      `json:"outcome"`
		Account accountView `json:"account"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Outcome != "reset" || body.Account.LastReset == nil || !body.Account.LastReset.Pending {
		t.Fatal("missing immediate reset acknowledgement")
	}
	if body.Account.ResetCredits == nil || body.Account.ResetCredits.Count != 0 {
		t.Fatal("spent credit remained visible")
	}
	if body.Account.Usage == nil || body.Account.Usage.Windows[0].UsedPercent != 100 {
		t.Fatal("quota was fabricated before provider confirmed it")
	}
	close(p.releaseUsage)
	deadline := time.Now().Add(5 * time.Second)
	for manager.LastReset(account.ID).Pending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	view := a.viewOf(account)
	if view.LastReset.Pending || view.Usage == nil || view.Usage.Windows[0].UsedPercent != 5 {
		t.Fatal("fresh quota did not reach account state")
	}
	if view.ResetCredits.Count != 0 {
		t.Fatal("stale provider credit response resurrected spent credit")
	}
}

func TestCreditCacheCoalescesConcurrentMisses(t *testing.T) {
	p := &feedbackProvider{recheckProvider: &recheckProvider{}, releaseList: make(chan struct{}), listStarted: make(chan struct{}, 20)}
	defer close(p.releaseList)
	a := &API{}
	account := store.Account{ID: "fixture"}
	a.cachedResetCredits(p, account)
	<-p.listStarted
	for i := 0; i < 9; i++ {
		a.cachedResetCredits(p, account)
	}
	select {
	case <-p.listStarted:
		t.Fatal("a duplicate provider request started while refresh was in flight")
	case <-time.After(30 * time.Millisecond):
	}
	if p.listCalls.Load() != 1 {
		t.Fatalf("%d provider fetches, want 1", p.listCalls.Load())
	}
}
