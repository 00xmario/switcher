package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
)

type nativeProviderFixture struct {
	*fakeProvider
	refreshCalls int
}

type captureProviderFixture struct {
	*nativeProviderFixture
	store           *store.Store
	captured        chan struct{}
	continueCapture chan struct{}
	switched        chan struct{}
}

func (p *captureProviderFixture) CaptureAndPersist(ctx context.Context, save func(*store.Account) error) (store.Account, error) {
	a, err := p.store.Get("a")
	if err != nil {
		return a, err
	}
	close(p.captured)
	<-p.continueCapture
	return a, save(&a)
}
func (p *captureProviderFixture) SwitchNative(ctx context.Context, id string, commit func(string) error) (claudecode.SwitchResult, error) {
	close(p.switched)
	a, err := p.store.Get("a")
	if err != nil {
		return claudecode.SwitchResult{}, err
	}
	a.Token.RefreshToken = "advanced-after-native-switch"
	if err := p.store.Save(a); err != nil {
		return claudecode.SwitchResult{}, err
	}
	return claudecode.SwitchResult{}, commit("capture-fixture")
}

func TestCapturePersistenceCannotLandAfterNativeSwitchAndRefresh(t *testing.T) {
	m := newManager(t, nil, account("a"), account("b"))
	p := &captureProviderFixture{nativeProviderFixture: &nativeProviderFixture{fakeProvider: &fakeProvider{id: "fake"}}, store: m.store,
		captured: make(chan struct{}), continueCapture: make(chan struct{}), switched: make(chan struct{})}
	m.providers["fake"] = p
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		if _, err := m.ImportNative(context.Background(), p, ""); err != nil {
			t.Error(err)
		}
	}()
	<-p.captured
	group.Add(1)
	go func() {
		defer group.Done()
		if _, err := m.ActivateForClient(context.Background(), "b"); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-p.switched:
		t.Fatal("native switch ran between capture and persistence")
	case <-time.After(30 * time.Millisecond):
	}
	close(p.continueCapture)
	group.Wait()
	saved, _ := m.store.Get("a")
	if saved.Token.RefreshToken != "advanced-after-native-switch" {
		t.Fatal("captured predecessor overwrote later native generation")
	}
}

func TestActivationPersistenceFailureRestoresRouting(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	for _, id := range []string{"a", "b"} {
		if err := st.Save(account(id)); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(st, map[string]provider.Provider{"fake": &fakeProvider{id: "fake"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	m.markExhausted("b", until)
	if err := os.Remove(filepath.Join(root, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.activateWithReceipt("b", "fixture-native-receipt"); err == nil {
		t.Fatal("persistence failure was hidden")
	}
	if m.ActiveID("fake") != "a" || m.nativeClaudeCommit != "" {
		t.Fatal("failed activation remained published in memory")
	}
	if _, parked := m.Exhausted("b"); !parked {
		t.Fatal("failed activation cleared old exhaustion state")
	}
}

func TestClaudeReloginCannotReplaceAnotherOrganization(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	a := account("a")
	a.Provider = "claude"
	a.Token.AccountID = "same-user"
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: json.RawMessage(`{}`), OAuthAccount: json.RawMessage(`{"accountUuid":"same-user","emailAddress":"a@example.com","organizationUuid":"org-a"}`)}
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	m, err := New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.Token.AccessToken = "foreign-organization"
	b.ClaudeCode = &store.ClaudeCodeLogin{Credentials: json.RawMessage(`{}`), OAuthAccount: json.RawMessage(`{"accountUuid":"same-user","emailAddress":"a@example.com","organizationUuid":"org-b"}`)}
	if err := m.ReplaceReloginAccount(&b, "a"); !errors.Is(err, ErrReloginIdentityMismatch) {
		t.Fatalf("cross-organization replacement: %v", err)
	}
	stored, _ := st.Get("a")
	if stored.Token.AccessToken != a.Token.AccessToken {
		t.Fatal("foreign organization overwrote original slot")
	}
}

func (*nativeProviderFixture) NativeEnabled() bool { return true }
func (*nativeProviderFixture) NativeStatus() claudecode.Status {
	return claudecode.Status{Available: true}
}
func (*nativeProviderFixture) SwitchNative(ctx context.Context, id string, commit func(string) error) (claudecode.SwitchResult, error) {
	return claudecode.SwitchResult{}, commit("fixture-receipt")
}
func (*nativeProviderFixture) SyncNative(ctx context.Context, a *store.Account) (bool, error) {
	if a.Token.AccessToken == "native-new" {
		return false, nil
	}
	a.Token.AccessToken = "native-new"
	return true, nil
}
func (p *nativeProviderFixture) Refresh(context.Context, *store.Account) error {
	p.refreshCalls++
	return provider.ErrNativeCredentialBusy
}

func TestNativeForwardUsesSynchronizedGeneration(t *testing.T) {
	m := newManager(t, nil, account("a"))
	p := &nativeProviderFixture{fakeProvider: &fakeProvider{id: "fake"}}
	a, err := m.nativeForRequest(context.Background(), p, account("a"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Token.AccessToken != "native-new" {
		t.Fatal("forwarding retained pre-synchronization credential")
	}
	stored, _ := m.store.Get("a")
	if stored.Token.AccessToken != a.Token.AccessToken {
		t.Fatal("synchronized generation was not saved")
	}
}

func TestNativeOwnedRefreshIsNotClassifiedAsDeadAccount(t *testing.T) {
	m := newManager(t, nil, account("a"))
	if err := m.Activate("a"); err != nil {
		t.Fatal(err)
	}
	p := &nativeProviderFixture{fakeProvider: &fakeProvider{id: "fake"}}
	_, err := m.refreshForProxy(context.Background(), p, account("a"), true)
	if !errors.Is(err, provider.ErrNativeCredentialBusy) {
		t.Fatalf("expected native ownership deferral: %v", err)
	}
	if _, parked := m.Exhausted("a"); parked || m.ActiveID("fake") != "a" {
		t.Fatal("owned token was incorrectly parked or failed over")
	}
	if m.AccountHealth("a").Condition == "needs_relogin" {
		t.Fatal("ownership deferral reported as a revoked credential")
	}
}
