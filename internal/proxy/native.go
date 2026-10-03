package proxy

import (
	"context"
	"sort"
	"sync"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// ActivateForClient is the explicit UI switch path. Automatic proxy failover
// continues to select only its route, not mutate a machine's native login.
func (m *Manager) ActivateForClient(ctx context.Context, id string) (*claudecode.SwitchResult, error) {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	account, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	native, ok := m.providers[account.Provider].(provider.NativeLoginProvider)
	if !ok || !native.NativeEnabled() {
		return nil, m.Activate(id)
	}
	accounts, err := m.store.List()
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, a := range accounts {
		if a.Provider == account.Provider {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	locks := make([]*sync.Mutex, 0, len(ids))
	for _, key := range ids {
		lock := m.refreshLock(key)
		lock.Lock()
		locks = append(locks, lock)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	result, err := native.SwitchNative(ctx, id, func(receipt string) error { return m.activateWithReceipt(id, receipt) })
	// Native switching may have adopted outgoing credentials even if a later
	// validation/write failed. Invalidate workers carrying older snapshots.
	m.mu.Lock()
	for _, key := range ids {
		m.generation[key]++
		m.accountRevision[key]++
		delete(m.health, key)
		delete(m.planChecked, key)
	}
	m.mu.Unlock()
	return &result, err
}

func (m *Manager) syncNative(ctx context.Context, prov provider.Provider, a *store.Account) error {
	native, ok := prov.(provider.NativeLoginProvider)
	if !ok || !native.NativeEnabled() {
		return nil
	}
	beforeAccess, beforeRefresh := a.Token.AccessToken, a.Token.RefreshToken
	changed, err := native.SyncNative(ctx, a)
	if err != nil {
		return err
	}
	if changed {
		if err := m.store.Save(*a); err != nil {
			return err
		}
		m.mu.Lock()
		m.accountRevision[a.ID]++
		m.quotaRevision[a.ID]++
		if beforeAccess != a.Token.AccessToken || beforeRefresh != a.Token.RefreshToken {
			h := m.healthLocked(a.ID)
			h.relogin = false
			h.lastChecked, h.lastSuccess, h.retryAt = time.Time{}, time.Time{}, time.Time{}
			delete(m.lastUsage, a.ID)
			delete(m.planChecked, a.ID)
		}
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) nativeForRequest(ctx context.Context, prov provider.Provider, picked store.Account) (store.Account, error) {
	if native, ok := prov.(provider.NativeLoginProvider); !ok || !native.NativeEnabled() {
		return picked, nil
	}
	lock := m.refreshLock(picked.ID)
	lock.Lock()
	defer lock.Unlock()
	a, err := m.store.Get(picked.ID)
	if err != nil {
		return picked, err
	}
	err = m.syncNative(ctx, prov, &a)
	return a, err
}

// Import keeps capture and persistence in the same activation/account/native
// lock span. A captured predecessor cannot be saved after an intervening
// switch and inactive refresh advanced that account's saved generation.
func (m *Manager) ImportNative(ctx context.Context, prov provider.Provider, target string) (store.Account, error) {
	m.nativeActivation.Lock()
	defer m.nativeActivation.Unlock()
	capture, ok := prov.(interface {
		CaptureAndPersist(context.Context, func(*store.Account) error) (store.Account, error)
	})
	if !ok {
		return store.Account{}, provider.ErrUnsupported
	}
	accounts, err := m.store.List()
	if err != nil {
		return store.Account{}, err
	}
	ids := []string{}
	for _, a := range accounts {
		if a.Provider == prov.ID() {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	locks := []*sync.Mutex{}
	for _, id := range ids {
		lock := m.refreshLock(id)
		lock.Lock()
		locks = append(locks, lock)
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	return capture.CaptureAndPersist(ctx, func(a *store.Account) error {
		if target != "" {
			return m.replaceReloginRecord(a, target, false)
		}
		return m.replaceAccountRecord(*a, false)
	})
}
