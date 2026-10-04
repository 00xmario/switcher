package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"switcher/internal/claudecode"
	"switcher/internal/desktoprelay"
	"switcher/internal/provider"
	"switcher/internal/store"
)

type desktopCredentialBlockedTransport struct{}

func (desktopCredentialBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fixture blocks default HTTP egress")
}

type desktopNativeCredentialFixture struct {
	*fakeProvider
	synced, refreshed, rejected []string
	err                         error
	activations                 int
}

func (*desktopNativeCredentialFixture) NativeEnabled() bool { return true }
func (*desktopNativeCredentialFixture) NativeStatus() claudecode.Status {
	return claudecode.Status{Available: true, ActiveID: "claude-a"}
}
func (p *desktopNativeCredentialFixture) SyncNative(_ context.Context, a *store.Account) (bool, error) {
	p.synced = append(p.synced, a.ID)
	if p.err != nil {
		return false, p.err
	}
	return false, nil
}
func (p *desktopNativeCredentialFixture) SwitchNative(context.Context, string, func(string) error) (claudecode.SwitchResult, error) {
	p.activations++
	return claudecode.SwitchResult{}, errors.New("fixture forbids native activation")
}
func (p *desktopNativeCredentialFixture) Refresh(_ context.Context, a *store.Account) error {
	p.refreshed = append(p.refreshed, a.ID)
	if p.err != nil {
		return p.err
	}
	a.Token.AccessToken = "fixture-successor-access"
	return nil
}
func (p *desktopNativeCredentialFixture) RefreshAfter401(ctx context.Context, a *store.Account, rejected string) error {
	p.rejected = append(p.rejected, rejected)
	return p.Refresh(ctx, a)
}

func newDesktopCredentialFixture(t *testing.T) (*Manager, *desktopNativeCredentialFixture) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = desktopCredentialBlockedTransport{}
	t.Cleanup(func() { http.DefaultTransport = previous })
	st := store.New(t.TempDir())
	for _, a := range []store.Account{
		{ID: "claude-a", Provider: "claude", Token: store.Token{AccessToken: "fixture-active-access", RefreshToken: "fixture-active-refresh"}},
		{ID: "claude-b", Provider: "claude", Token: store.Token{AccessToken: "fixture-target-access", RefreshToken: "fixture-target-refresh", Extra: map[string]any{"user_id": "original-metadata"}}},
		{ID: "codex-a", Provider: "codex", Token: store.Token{AccessToken: "fixture-codex-access"}},
	} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveState(store.State{Active: map[string]string{"claude": "claude-a"}}); err != nil {
		t.Fatal(err)
	}
	p := &desktopNativeCredentialFixture{fakeProvider: &fakeProvider{id: "claude"}}
	m, err := New(st, map[string]provider.Provider{"claude": p, "codex": &fakeProvider{id: "codex"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestDesktopCredentialSourcePreparesExplicitClaudeAccountWithoutActivation(t *testing.T) {
	m, p := newDesktopCredentialFixture(t)
	source := NewDesktopCredentialSource(m)
	credential, err := source.Prepare(context.Background(), "claude-b")
	if err != nil || credential.AccountID != "claude-b" || credential.AccessToken != "fixture-target-access" {
		t.Fatalf("explicit preparation failed: %v", err)
	}
	// A valid stored token is returned without native sync or refresh.
	if m.ActiveID("claude") != "claude-a" || p.activations != 0 || len(p.synced) != 0 {
		t.Fatal("preparation changed or followed the native/global active account")
	}
	if _, err := source.RefreshRejected(context.Background(), "claude-b", "fixture-target-access"); err != nil {
		t.Fatal(err)
	}
	credential, err = source.RefreshRejected(context.Background(), "claude-b", "fixture-target-access")
	if err != nil || credential.AccessToken != "fixture-successor-access" || strings.Join(p.refreshed, ",") != "claude-b" || strings.Join(p.rejected, ",") != "fixture-target-access" || p.activations != 0 || m.ActiveID("claude") != "claude-a" {
		t.Fatal("rejected-generation recovery did not preserve account and owner")
	}
	saved, err := m.store.Get("claude-b")
	if err != nil || saved.Token.Extra["user_id"] != "original-metadata" || saved.Token.RefreshToken != "fixture-target-refresh" {
		t.Fatal("access adapter rewrote user metadata or consumed a copied refresh snapshot")
	}
	raw, err := json.Marshal(credential)
	if err != nil || strings.Contains(string(raw), "access") || strings.Contains(string(raw), "refresh") {
		t.Fatal("relay credential serialized OAuth")
	}
}

func TestDesktopCredentialSourceRejectsOtherProvidersAndSanitizesOwnedErrors(t *testing.T) {
	m, p := newDesktopCredentialFixture(t)
	source := NewDesktopCredentialSource(m)
	for _, id := range []string{"codex-a", "missing", "../claude-a"} {
		if credential, err := source.Prepare(context.Background(), id); err == nil || credential.AccessToken != "" {
			t.Fatalf("accepted invalid target %q", id)
		}
		if credential, err := source.RefreshRejected(context.Background(), id, "fixture-rejected"); err == nil || credential.AccessToken != "" {
			t.Fatalf("refreshed invalid target %q", id)
		}
	}
	if len(p.synced) != 0 || len(p.refreshed) != 0 {
		t.Fatal("invalid target reached credential ownership hooks")
	}
	for _, tc := range []struct{ err, want error }{
		{fmt.Errorf("%w: fixture-provider-body-secret", provider.ErrNativeCredentialBusy), desktoprelay.ErrBusy},
		{fmt.Errorf("%w: fixture-provider-body-secret", claudecode.ErrNativeOwned), desktoprelay.ErrBusy},
		{errors.New("fixture-provider-body-secret"), desktoprelay.ErrUnavailable},
	} {
		p.err = tc.err
		credential, err := source.RefreshRejected(context.Background(), "claude-b", "fixture-target-access")
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "fixture-provider-body-secret") || credential.AccessToken != "" {
			t.Fatal("adapter lost error classification or disclosed provider error")
		}
	}
}
