package server

import (
	"context"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

type usagePlanFixture struct{ *recheckProvider }

func (*usagePlanFixture) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{Available: true, Plan: "free", Windows: []provider.UsageWindow{{Label: "Monthly", UsedPercent: 0}}}, nil
}

func TestPublicPlanMatchesQuotaSnapshotDespiteEarlierAccountRead(t *testing.T) {
	st := store.New(t.TempDir())
	old := store.Account{ID: "fake-a", Provider: "fake", Plan: "pro", Token: store.Token{AccessToken: "fixture-access"}}
	if err := st.Save(old); err != nil {
		t.Fatal(err)
	}
	p := &usagePlanFixture{&recheckProvider{}}
	providers := map[string]provider.Provider{"fake": p}
	m, err := proxy.New(st, providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.RefreshUsage(context.Background(), old)
	api := &API{Store: st, Proxy: m, Providers: providers}
	view := viewOfBase(api, old)
	if view.Plan != "free" || view.Usage == nil || view.Usage.Plan != "free" {
		t.Fatalf("old account read paired paid plan with free quota: plan=%s usage=%+v", view.Plan, view.Usage)
	}
}
