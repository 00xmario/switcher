package proxy

import (
	"context"
	"errors"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type planTestProvider struct {
	*fakeProvider
	calls int
	err   error
}

func (p *planTestProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{Available: true}, nil
}
func (p *planTestProvider) ResolvePlan(context.Context, store.Account) (string, error) {
	p.calls++
	return "claude_max_5x", p.err
}

func TestPlanBackfillPreservesCredentialsAndPreference(t *testing.T) {
	a := account("a")
	no := false
	a.AutoUseReset = &no
	m := newManager(t, nil, a)
	p := &planTestProvider{fakeProvider: &fakeProvider{id: "fake"}}
	m.providers["fake"] = p
	m.RefreshUsage(context.Background(), a)
	saved, err := m.store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Plan != "claude_max_5x" || saved.Token.AccessToken != a.Token.AccessToken || saved.AutoUseReset == nil || *saved.AutoUseReset {
		t.Fatalf("backfill lost account state: plan=%s", saved.Plan)
	}
	m.RefreshUsage(context.Background(), a)
	if p.calls != 1 {
		t.Fatal("plan fetched on every poll")
	}
	if err := m.ReplaceAccount(a); err != nil {
		t.Fatal(err)
	}
	m.RefreshUsage(context.Background(), a)
	if p.calls != 2 {
		t.Fatal("relogin did not invalidate plan metadata")
	}
}

func TestManualRecheckRetriesFailedPlanLookup(t *testing.T) {
	a := account("a")
	m := newManager(t, nil, a)
	p := &planTestProvider{fakeProvider: &fakeProvider{id: "fake"}, err: errors.New("offline")}
	m.providers["fake"] = p
	m.RefreshUsage(context.Background(), a)
	m.RefreshUsage(context.Background(), a)
	if p.calls != 1 {
		t.Fatal("failed plan lookup was not backed off")
	}
	p.err = nil
	m.refreshAccount(context.Background(), a.ID, m.generation[a.ID], true)
	saved, err := m.store.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.calls != 2 || saved.Plan != "claude_max_5x" {
		t.Fatal("manual check did not recover plan")
	}
}

type currentPlanProvider struct {
	*planTestProvider
	plan     string
	usageErr error
}

func (p *currentPlanProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{Available: p.usageErr == nil, Plan: p.plan, Windows: []provider.UsageWindow{{Label: "Monthly", UsedPercent: 0}}}, p.usageErr
}

func TestUsagePlanTracksDowngradeAndUpgradeWithoutRelogin(t *testing.T) {
	a := account("a")
	a.Plan = "pro"
	yes := true
	a.AutoUseReset = &yes
	m := newManager(t, nil, a)
	p := &currentPlanProvider{planTestProvider: &planTestProvider{fakeProvider: &fakeProvider{id: "fake"}}, plan: "free"}
	m.providers["fake"] = p
	if err := m.Activate(a.ID); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []string{"free", "prolite", "free"} {
		p.plan = plan
		m.RefreshUsage(context.Background(), a)
		saved, err := m.store.Get(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := m.LastUsage(a.ID)
		if saved.Plan != plan || !ok || u.Plan != plan || len(u.Windows) != 1 || u.Windows[0].UsedPercent != 0 {
			t.Fatalf("plan/quota not reconciled: saved=%s usage=%+v", saved.Plan, u)
		}
		if saved.Token.AccessToken != a.Token.AccessToken || saved.AutoUseReset == nil || !*saved.AutoUseReset || m.ActiveID("fake") != a.ID {
			t.Fatal("plan update changed credentials, preferences, or routing")
		}
	}
	if p.calls != 0 {
		t.Fatal("separate plan lookup replaced current response metadata")
	}
	p.plan, p.usageErr = "pro", errors.New("fixture quota unavailable")
	m.RefreshUsage(context.Background(), a)
	saved, _ := m.store.Get(a.ID)
	if saved.Plan != "free" {
		t.Fatal("failed usage response replaced verified free plan")
	}
}

func TestUsageWithoutPlanRetainsSavedPlan(t *testing.T) {
	a := account("a")
	a.Plan = "free"
	m := newManager(t, nil, a)
	p := &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil }}
	p.fakeProvider = &fakeProvider{id: "fake"}
	m.providers["fake"] = p
	m.RefreshUsage(context.Background(), a)
	saved, _ := m.store.Get(a.ID)
	if saved.Plan != "free" {
		t.Fatal("missing plan metadata erased saved plan")
	}
}
