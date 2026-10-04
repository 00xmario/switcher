package proxy

import (
	"context"
	"errors"

	"switcher/internal/claudecode"
	"switcher/internal/desktoprelay"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// DesktopCredentialSource exposes only a selected account's access credential.
// The proxy remains the credential owner, including native synchronization,
// serialized refresh and rejected-generation adoption. This adapter never
// activates an account or keeps a separate refresh-token snapshot.
type DesktopCredentialSource struct {
	manager *Manager
}

func NewDesktopCredentialSource(manager *Manager) *DesktopCredentialSource {
	return &DesktopCredentialSource{manager: manager}
}

var _ desktoprelay.CredentialSource = (*DesktopCredentialSource)(nil)

func (s *DesktopCredentialSource) Prepare(ctx context.Context, id string) (desktoprelay.Credential, error) {
	return s.prepare(ctx, id, nil)
}

func (s *DesktopCredentialSource) RefreshRejected(ctx context.Context, id, rejected string) (desktoprelay.Credential, error) {
	return s.prepare(ctx, id, &rejected)
}

func (s *DesktopCredentialSource) prepare(ctx context.Context, id string, rejected *string) (desktoprelay.Credential, error) {
	if s == nil || s.manager == nil || s.manager.store == nil {
		return desktoprelay.Credential{}, desktoprelay.ErrUnavailable
	}
	a, err := s.manager.store.Get(id)
	if err != nil {
		return desktoprelay.Credential{}, desktopCredentialError(err)
	}
	if a.ID != id || a.Provider != "claude" {
		return desktoprelay.Credential{}, desktoprelay.ErrUnavailable
	}
	if rejected == nil {
		a, err = s.manager.PrepareAccount(ctx, id)
	} else {
		a, err = s.manager.RefreshAccountAfter401(ctx, id, *rejected)
	}
	if err != nil {
		return desktoprelay.Credential{}, desktopCredentialError(err)
	}
	if a.ID != id || a.Provider != "claude" || a.Token.AccessToken == "" {
		return desktoprelay.Credential{}, desktoprelay.ErrUnavailable
	}
	return desktoprelay.Credential{AccountID: id, AccessToken: a.Token.AccessToken, AccountUUID: a.Token.AccountID}, nil
}

// Preserve only local classifications. Provider bodies, native paths and OAuth
// material must not enter relay errors or logs through an error chain.
func desktopCredentialError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return desktoprelay.ErrNotFound
	case errors.Is(err, provider.ErrNativeCredentialBusy), errors.Is(err, claudecode.ErrNativeOwned), errors.Is(err, claudecode.ErrConflict):
		return desktoprelay.ErrBusy
	default:
		return desktoprelay.ErrUnavailable
	}
}
