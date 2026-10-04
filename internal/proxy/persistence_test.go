package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

func persistenceFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	st := store.New(root)
	if err := st.Save(account("a")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(store.State{Active: map[string]string{"fake": "a"}, Exhausted: map[string]int64{"a": time.Now().Add(time.Hour).Unix()}}); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, map[string]provider.Provider{"fake": &fakeProvider{id: "fake", upstream: mockUpstream()}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "state.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	return m, root
}

func TestFailedPreferenceWriteKeepsPublishedState(t *testing.T) {
	m, _ := persistenceFixture(t)
	if err := m.HideProvider("fake"); err == nil {
		t.Fatal("preference write failure was hidden")
	}
	if _, hidden := m.Providers(); len(hidden) != 0 {
		t.Fatalf("failed preference write remained published: %v", hidden)
	}
}

func TestFailedQuotaClearKeepsExhaustion(t *testing.T) {
	m, _ := persistenceFixture(t)
	if err := m.ClearExhausted("a"); err == nil {
		t.Fatal("quota-clear write failure was hidden")
	}
	if _, parked := m.Exhausted("a"); !parked {
		t.Fatal("failed quota clear removed the persisted park")
	}
}

func TestFailedDeletionKeepsAccountAndRouting(t *testing.T) {
	m, _ := persistenceFixture(t)
	if err := m.DeleteAccount("a"); err == nil {
		t.Fatal("deletion hid its state-write failure")
	}
	if _, err := m.store.Get("a"); err != nil || m.ActiveID("fake") != "a" {
		t.Fatalf("failed deletion removed account or selection: active=%s error=%v", m.ActiveID("fake"), err)
	}
}

func TestAcknowledgedResetRetainsCreditFenceWhenStateWriteFails(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	if err := st.Save(account("a")); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(store.State{Active: map[string]string{"fake": "a"}, Exhausted: map[string]int64{"a": time.Now().Add(time.Hour).Unix()}}); err != nil {
		t.Fatal(err)
	}
	p := &fakeCreditProvider{fakeProvider: &fakeProvider{id: "fake"}, outcome: "reset"}
	p.grant("a")
	m, err := New(st, map[string]provider.Provider{"fake": p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "state.json.tmp")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	outcome, err := m.UseBankedReset(context.Background(), "a", "a-credit")
	awaitResetWorker(t, m, "a")
	if outcome != "reset" || err == nil || p.spends() != 1 {
		t.Fatalf("redemption acknowledgement lost: outcome=%s error=%v spends=%d", outcome, err, p.spends())
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	outcome, err = m.UseBankedReset(context.Background(), "a", "a-credit")
	if outcome != "already_redeemed" || err != nil || p.spends() != 1 {
		t.Fatalf("pinned retry spent another credit: outcome=%s error=%v spends=%d", outcome, err, p.spends())
	}
	if _, parked := m.Exhausted("a"); parked {
		t.Fatal("pinned retry did not reconcile the acknowledged reset after storage recovered")
	}
}

func TestPinnedResetRetryDoesNotUndoLaterExhaustion(t *testing.T) {
	m, p := newCreditManager(t, nil, nil, account("a"))
	p.grant("a")
	if _, err := m.UseBankedReset(context.Background(), "a", "a-credit"); err != nil {
		t.Fatal(err)
	}
	awaitResetWorker(t, m, "a")
	if err := m.markExhausted("a", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if outcome, err := m.UseBankedReset(context.Background(), "a", "a-credit"); outcome != "already_redeemed" || err != nil {
		t.Fatalf("pinned retry: %s %v", outcome, err)
	}
	if _, parked := m.Exhausted("a"); !parked || p.spends() != 1 {
		t.Fatal("pinned retry undid a post-reset exhaustion response")
	}
}

func TestAutomaticResetDoesNotSpendWhenFailoverLookupFails(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	if err := st.Save(account("a")); err != nil {
		t.Fatal(err)
	}
	p := &fakeCreditProvider{fakeProvider: &fakeProvider{id: "fake", upstream: mockUpstream()}, outcome: "reset"}
	p.grant("a")
	blocked := &blockedCreditProvider{fakeCreditProvider: p, entered: make(chan struct{}), release: make(chan struct{})}
	m, err := New(st, map[string]provider.Provider{"fake": blocked}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetAutoUseResetPolicy(func(store.Account) bool { return true })
	mockForwarding(t, func(*http.Request) (*http.Response, error) {
		return regressionResponse(429, usageLimit(time.Now().Add(time.Hour).Unix())), nil
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doRequest(t, m, "/v1/responses") }()
	<-blocked.entered
	accounts, backup := filepath.Join(root, "accounts"), filepath.Join(root, "fixture-accounts")
	if err := os.Rename(accounts, backup); err != nil {
		close(blocked.release)
		<-done
		t.Fatal(err)
	}
	if err := os.WriteFile(accounts, []byte("fixture directory-read failure"), 0600); err != nil {
		close(blocked.release)
		<-done
		t.Fatal(err)
	}
	close(blocked.release)
	response := <-done
	if err := os.Remove(accounts); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, accounts); err != nil {
		t.Fatal(err)
	}
	awaitResetWorker(t, m, "a")
	if response.Code != 429 || p.spends() != 0 {
		t.Fatalf("spent without proving last-resort eligibility: status=%d spends=%d", response.Code, p.spends())
	}
}
