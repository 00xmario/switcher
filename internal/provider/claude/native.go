package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
	"time"
)

func (p *Provider) ConfigureNative(st *store.Store, dataRoot string) error {
	paths, err := claudecode.ResolvePaths()
	if err != nil {
		p.nativeError = err
		return err
	}
	p.Native = claudecode.New(paths, dataRoot, st, claudecode.SystemKeychain{}, p.nativeProfile, p.refreshGrant)
	p.nativeError = nil
	p.Native.SetReceiptReader(func(receipt string) (bool, error) {
		state, err := st.LoadState()
		return err == nil && state.NativeClaudeCommit == receipt, err
	})
	return nil
}
func (p *Provider) nativeProfile(ctx context.Context, token string) (claudecode.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	profile, err := p.fetchProfile(ctx, token)
	if err != nil {
		return claudecode.Identity{}, err
	}
	return claudecode.Identity{UUID: profile.Account.UUID, Email: profile.Account.Email, OrganizationUUID: profile.Organization.UUID, OrganizationName: profile.Organization.Name, Plan: profile.plan()}, nil
}
func (p *Provider) NativeEnabled() bool { return p.Native != nil || p.nativeError != nil }

// Conservative usage-API cadence from claude-swap; native credential
// observation still runs on the server's ordinary state refresh tick.
func (p *Provider) UsagePollInterval() time.Duration { return 3 * time.Minute }
func (p *Provider) BackupNativeAccount(a store.Account) error {
	if p.Native == nil {
		return nil
	}
	return p.Native.BackupAccount(a)
}
func (p *Provider) NativeStatus() claudecode.Status {
	if p.nativeError != nil {
		return claudecode.Status{Condition: "unavailable", Message: p.nativeError.Error()}
	}
	if p.Native == nil {
		return claudecode.Status{Condition: "not_configured"}
	}
	return p.Native.Status()
}
func (p *Provider) SwitchNative(ctx context.Context, id string, commit func(string) error) (claudecode.SwitchResult, error) {
	if p.nativeError != nil {
		return claudecode.SwitchResult{}, p.nativeError
	}
	return p.Native.SwitchWithReceipt(ctx, id, commit)
}
func (p *Provider) SyncNative(ctx context.Context, a *store.Account) (bool, error) {
	if p.nativeError != nil {
		return false, fmt.Errorf("%w: %v", provider.ErrNativeCredentialBusy, p.nativeError)
	}
	if p.Native == nil {
		return false, nil
	}
	changed, err := p.Native.Synchronize(ctx, a)
	if err != nil {
		return false, fmt.Errorf("%w: %v", provider.ErrNativeCredentialBusy, err)
	}
	return changed, nil
}

func (p *Provider) NativeReplacement(a store.Account) error {
	if p.Native == nil {
		return p.nativeError
	}
	return p.Native.RetireSuccessor(a)
}

func (p *Provider) importNative(ctx context.Context) (store.Account, error) {
	a, err := p.Native.Capture(ctx)
	if err != nil {
		return store.Account{}, err
	}
	p.assignNativeID(&a)
	return a, nil
}

func (p *Provider) assignNativeID(a *store.Account) {
	if a.ID != "" {
		return
	}
	var identity claudecode.Identity
	_ = json.Unmarshal(a.ClaudeCode.OAuthAccount, &identity)
	if id := p.Native.ExistingID(identity); id != "" {
		a.ID = id
		return
	}
	sum := sha256.Sum256([]byte(a.Provider + "|" + a.Email + "|" + a.Token.AccountID + "|" + identity.OrganizationUUID))
	a.ID = fmt.Sprintf("claude-%x", sum[:4])
}

func (p *Provider) CaptureAndPersist(ctx context.Context, save func(*store.Account) error) (store.Account, error) {
	if p.nativeError != nil {
		return store.Account{}, p.nativeError
	}
	if p.Native == nil {
		return store.Account{}, provider.ErrUnsupported
	}
	return p.Native.CaptureAndStore(ctx, func(a *store.Account) error { p.assignNativeID(a); return save(a) })
}
