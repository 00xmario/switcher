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
