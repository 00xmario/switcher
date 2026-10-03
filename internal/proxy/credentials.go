package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"switcher/internal/store"
)

// PrepareAccount obtains current credentials for an explicitly named account.
// It synchronizes native credentials and refreshes under the account lock,
// without choosing a different account or changing proxy routing.
func (m *Manager) PrepareAccount(ctx context.Context, id string) (store.Account, error) {
	a, _, err := m.prepareAccount(ctx, id, nil, false)
	return a, err
}

// RefreshAccountAfter401 retries credential preparation for a rejected access
// generation. A concurrently installed successor is used without another grant.
func (m *Manager) RefreshAccountAfter401(ctx context.Context, id, rejectedAccessToken string) (store.Account, error) {
	a, _, err := m.prepareAccount(ctx, id, &rejectedAccessToken, false)
	return a, err
}

func (m *Manager) prepareAccount(ctx context.Context, id string, rejected *string, forProxy bool) (store.Account, requestEpoch, error) {
	lock := m.refreshLock(id)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			return store.Account{}, requestEpoch{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer lock.Unlock()
	return m.prepareAccountLocked(ctx, id, rejected, forProxy)
}

// The caller holds this account's refresh lock through credential preparation
// and any provider action that must use the resulting generation.
func (m *Manager) prepareAccountLocked(ctx context.Context, id string, rejected *string, forProxy bool) (store.Account, requestEpoch, error) {
	if err := ctx.Err(); err != nil {
		return store.Account{}, requestEpoch{}, err
	}
	a, err := m.store.Get(id)
	if err != nil {
		return a, requestEpoch{}, err
	}
	prov, ok := m.providers[a.Provider]
	if !ok {
		return a, requestEpoch{}, fmt.Errorf("unknown provider %q", a.Provider)
	}
	if err := m.syncNative(ctx, prov, &a); err != nil {
		return a, requestEpoch{}, err
	}
	if rejected != nil && a.Token.AccessToken == *rejected {
		a, err = m.refreshForProxyLocked(ctx, prov, a, true, forProxy)
	} else if a.Token.AccessToken == "" || prov.IsExpired(a) {
		a, err = m.refreshForProxyLocked(ctx, prov, a, false, forProxy)
	}
	if err == nil && a.Token.AccessToken == "" {
		err = errors.New("provider did not issue an access credential")
	}
	m.mu.Lock()
	epoch := m.requestEpochLocked(a.Provider, id)
	m.mu.Unlock()
	return a, epoch, err
}
