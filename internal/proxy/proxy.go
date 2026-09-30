// Package proxy implements Switcher's request forwarding and its switching
// rules. There is exactly one active account; traffic is forwarded to it
// verbatim. The active account changes only in two cases: the user picks a
// different one, or the active account reports an exhausted usage limit,
// then Switcher moves to another usable account and transparently retries
// the in-flight request once.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// maxBodyBytes caps how much of a request body is buffered so a retry can
// resend it. Codex request payloads are well under this.
const maxBodyBytes = 64 << 20

// Avoid repeating an upstream quota request each minute after it returns
// 429. A user-initiated Recheck may still try immediately.
const usageRateLimitCooldown = 5 * time.Minute

// autoUseResetCooldown is how long an account waits before Switcher may
// spend another banked reset for it automatically. It bounds credit burn
// when a redeemed reset does not actually lift the limit. The manual
// "Use reset" action is unaffected.
const autoUseResetCooldown = 5 * time.Minute

// errNoAccount is the message returned when nothing can serve a request.
var errNoAccount = errors.New("no usable account is active; add one in the Switcher UI")

// Relogin errors describe only the local account match, never provider tokens.
var (
	ErrReloginIdentityMismatch  = errors.New("signed in to a different account")
	ErrReloginTargetUnavailable = errors.New("relogin account no longer exists")
)

// Health is a coarse usage and credential status. Timestamps are Unix seconds.
type Health struct {
	Condition        string `json:"condition"`
	LastChecked      int64  `json:"last_checked,omitempty"`
	LastUsageSuccess int64  `json:"last_usage_success,omitempty"`
}

type healthState struct {
	lastChecked time.Time
	lastSuccess time.Time
	relogin     bool
	checking    int
	queued      bool
	retryAt     time.Time
}

// Upstream result of a request that hit an exhausted account.
type usageLimitBody struct {
	Error struct {
		Type     string `json:"type"`
		ResetsAt int64  `json:"resets_at"`
	} `json:"error"`
}

// Manager owns the routing state and the forwarding handler.
type Manager struct {
	mu                sync.Mutex
	store             *store.Store
	providers         map[string]provider.Provider
	active            map[string]string // provider -> active account id
	exhausted         map[string]time.Time
	lastUsage         map[string]provider.Usage
	health            map[string]*healthState
	generation        map[string]uint64 // retained across deletion and replacement
	accountRevision   map[string]uint64 // credential writes, independent of queued-check generations
	selectionRevision map[string]uint64 // manual and automatic active-account changes
	syncing           atomic.Bool
	probeBusy         atomic.Bool
	probeBootID       string
	probeClient       *http.Client // optional internal test seam; production uses a fresh direct client
	refreshing        map[string]*sync.Mutex
	order             []string // display order of provider sections
	hidden            []string // providers dismissed from the UI
	managementKey     string   // hub management key, kept in state.json
	// autoUseReset reports whether an exhausted account may spend a banked
	// reset to stay in rotation. Nil means the feature is off: the proxy
	// never spends credits unless the server wires the user's preference in.
	autoUseReset func(store.Account) bool
	// autoResetAt throttles automatic redemptions per account so a client
	// retry loop cannot drain every banked reset on a limit that a reset
	// does not actually clear.
	autoResetAt   map[string]time.Time
	planChecked   map[string]time.Time
	resetEvents   map[string]ResetEvent
	resetSerial   uint64
	quotaRevision map[string]uint64
	spentCredits  map[string]map[string]int64
}

// New loads persisted state and returns the proxy manager. registration is
// the provider registration order (nil tolerated): registered but
// never-ordered providers append in that order instead of map order, so
// the default display order is deterministic.
func New(st *store.Store, providers map[string]provider.Provider, registration []string) (*Manager, error) {
	var boot [16]byte
	if _, err := rand.Read(boot[:]); err != nil {
		return nil, fmt.Errorf("probe boot identity: %w", err)
	}
	state, err := st.LoadState()
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	exhausted := map[string]time.Time{}
	for id, until := range state.Exhausted {
		if until > 0 {
			exhausted[id] = time.Unix(until, 0)
		}
	}
	active := state.Active
	if active == nil {
		active = map[string]string{}
	}
	order := []string{}
	hidden := []string{}
	for _, id := range state.ProviderOrder {
		if _, known := providers[id]; known {
			order = append(order, id)
		}
	}
	// Providers registered but never ordered land at the end, stable: the
	// registration order first, then anything else in map order.
	for _, id := range registration {
		if _, known := providers[id]; known && !containsID(order, id) {
			order = append(order, id)
		}
	}
	for id := range providers {
		if !containsID(order, id) {
			order = append(order, id)
		}
	}
	for _, id := range state.HiddenProviders {
		if _, known := providers[id]; known && !containsID(hidden, id) {
			hidden = append(hidden, id)
		}
	}
	return &Manager{
		store:             st,
		providers:         providers,
		active:            active,
		exhausted:         exhausted,
		lastUsage:         map[string]provider.Usage{},
		health:            map[string]*healthState{},
		generation:        map[string]uint64{},
		accountRevision:   map[string]uint64{},
		autoResetAt:       map[string]time.Time{},
		planChecked:       map[string]time.Time{},
		resetEvents:       map[string]ResetEvent{},
		quotaRevision:     map[string]uint64{},
		spentCredits:      map[string]map[string]int64{},
		selectionRevision: map[string]uint64{},
		probeBootID:       hex.EncodeToString(boot[:]),
		order:             order,
		hidden:            hidden,
		managementKey:     state.ManagementKey,
	}, nil
}

func containsID(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// ActiveAll returns a copy of the per-provider active map.
func (m *Manager) ActiveAll() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.active))
	for k, v := range m.active {
		out[k] = v
	}
	return out
}

// Providers returns the display order and hidden provider ids.
func (m *Manager) Providers() (order []string, hidden []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...), append([]string(nil), m.hidden...)
}

// SetManagementKey stores the hub key (generated at startup when state
// has none) so every later persist keeps it in state.json.
func (m *Manager) SetManagementKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if key == "" || m.managementKey == key {
		return
	}
	m.managementKey = key
	_ = m.persistLocked()
}

// ReorderProviders sets the display order. Unknown ids are ignored;
// registered providers missing from the list are appended, stable.
func (m *Manager) ReorderProviders(order []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := make([]string, 0, len(m.providers))
	seen := map[string]bool{}
	for _, id := range order {
		if _, ok := m.providers[id]; ok && !seen[id] {
			next = append(next, id)
			seen[id] = true
		}
	}
	for id := range m.providers {
		if !seen[id] {
			next = append(next, id)
			seen[id] = true
		}
	}
	m.order = next
	_ = m.persistLocked()
}

// HideProvider removes a provider from the display. Its accounts keep
// serving traffic through its prefix; re-adding is a UI action.
func (m *Manager) HideProvider(providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.providers[providerID]; !known || containsID(m.hidden, providerID) {
		return
	}
	m.hidden = append(m.hidden, providerID)
	_ = m.persistLocked()
}

// ShowProvider brings a hidden provider back into the display.
func (m *Manager) ShowProvider(providerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hidden = removeID(m.hidden, providerID)
	_ = m.persistLocked()
}

func removeID(list []string, id string) []string {
	out := list[:0]
	for _, v := range list {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// ActiveID returns the active account of one provider ("" when none).
func (m *Manager) ActiveID(providerID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active[providerID]
}

// Activate selects an account explicitly within its provider. This is the
// only manual switch path; it succeeds even when the account is currently
// marked exhausted (the user is in charge).
func (m *Manager) Activate(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	account, err := m.store.Get(id)
	if err != nil {
		return err
	}
	m.active[account.Provider] = id
	m.selectionRevision[account.Provider]++
	delete(m.exhausted, id)
	return m.persistLocked()
}

// Remove forgets routing bookkeeping for a deleted account.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation[id]++
	m.accountRevision[id]++
	m.removeLocked(id)
}

func (m *Manager) removeLocked(id string) {
	delete(m.resetEvents, id)
	delete(m.spentCredits, id)
	m.quotaRevision[id]++
	delete(m.planChecked, id)
	delete(m.exhausted, id)
	delete(m.lastUsage, id)
	delete(m.health, id)
	for providerID, activeID := range m.active {
		if activeID == id {
			delete(m.active, providerID)
			m.selectionRevision[providerID]++
			_ = m.persistLocked()
		}
	}
}

// DeleteAccount serializes deletion with token refresh and invalidates queued
// usage results. Callers should use this instead of Store.Delete plus Remove.
func (m *Manager) DeleteAccount(id string) error {
	lock := m.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()
	if err := m.store.Delete(id); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation[id]++
	m.accountRevision[id]++
	m.removeLocked(id)
	return nil
}

// ResetAccount clears cached usage and health after an external account
// overwrite. External writers must serialize token writes with refreshes;
// use ReplaceAccount when replacing credentials for an existing ID.
func (m *Manager) ResetAccount(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation[id]++
	m.accountRevision[id]++
	m.quotaRevision[id]++
	delete(m.health, id)
	delete(m.lastUsage, id)
}

// ReplaceAccount saves credentials under the same per-account lock used by
// refreshes. Callers may Activate the account afterward if it is new.
func (m *Manager) ReplaceAccount(a store.Account) error {
	lock := m.refreshLock(a.ID)
	lock.Lock()
	defer lock.Unlock()
	// Replacing credentials must not silently drop the user's per-account
	// preferences: provider-built accounts never carry them, so keep the
	// ones already on disk.
	if existing, err := m.store.Get(a.ID); err == nil {
		a.AutoUseReset = existing.AutoUseReset
	}
	return m.saveAccount(a)
}

// ReplaceReloginAccount checks identity and saves under the same account lock
// used by deletion and refresh. The selected account cannot be recreated if
// it was deleted while the provider sign-in was in flight.
func (m *Manager) ReplaceReloginAccount(a *store.Account, target string) error {
	lock := m.refreshLock(target)
	lock.Lock()
	defer lock.Unlock()
	existing, err := m.store.Get(target)
	if err != nil {
		return ErrReloginTargetUnavailable
	}
	if existing.Provider != a.Provider || !strings.EqualFold(existing.Email, a.Email) {
		return ErrReloginIdentityMismatch
	}
	a.ID = existing.ID
	// A relogin refreshes credentials only. Keep the account's per-account
	// preferences, which the provider never sets.
	a.AutoUseReset = existing.AutoUseReset
	return m.saveAccount(*a)
}

// UpdateAccount applies a per-account preference under the same lock token
// rotation uses, so a concurrent refresh cannot be reverted by a stale save.
// Credentials and cached usage are untouched: only the record changes.
func (m *Manager) UpdateAccount(id string, mutate func(*store.Account) error) error {
	lock := m.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()
	a, err := m.store.Get(id)
	if err != nil {
		return err
	}
	if err := mutate(&a); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Save(a); err != nil {
		return err
	}
	m.quotaRevision[id]++
	return nil
}

func (m *Manager) saveAccount(a store.Account) error {
	// QueueRecheck admits work under m.mu. Keep the disk save and generation
	// change in one critical section so it cannot accept a check for the new
	// file with the old generation in between them.
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Save(a); err != nil {
		return err
	}
	m.generation[a.ID]++
	m.accountRevision[a.ID]++
	delete(m.resetEvents, a.ID)
	m.quotaRevision[a.ID]++
	delete(m.planChecked, a.ID)
	delete(m.health, a.ID)
	delete(m.lastUsage, a.ID)
	return nil
}

// ClearExhausted lifts a parked/exhausted mark on an account (e.g. after
// a banked reset was redeemed for it).
func (m *Manager) ClearExhausted(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exhausted, id)
	m.quotaRevision[id]++
	_ = m.persistLocked()
}

// ExhaustedUntil reports when the account re-enters rotation, if at all.
func (m *Manager) Exhausted(id string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.exhausted[id]
	return t, ok
}

// Usage returns the last observed usage snapshot for an account.
func (m *Manager) LastUsage(id string) (provider.Usage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.lastUsage[id]
	return u, ok
}

// AccountHealth derives staleness without querying the provider.
func (m *Manager) AccountHealth(id string) Health {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.health[id]
	if h == nil {
		return Health{Condition: "checking"}
	}
	out := Health{}
	if !h.lastChecked.IsZero() {
		out.LastChecked = h.lastChecked.Unix()
	}
	if !h.lastSuccess.IsZero() {
		out.LastUsageSuccess = h.lastSuccess.Unix()
	}
	switch {
	case h.checking > 0 || h.queued:
		out.Condition = "checking"
	case h.relogin:
		out.Condition = "needs_relogin"
	case h.lastChecked.IsZero():
		out.Condition = "checking"
	case h.lastSuccess.IsZero(), !m.lastUsage[id].Available:
		out.Condition = "usage_unavailable"
	case time.Since(h.lastSuccess) >= 5*time.Minute:
		out.Condition = "usage_stale"
	default:
		out.Condition = "usage_current"
	}
	return out
}

func (m *Manager) healthLocked(id string) *healthState {
	h := m.health[id]
	if h == nil {
		h = &healthState{}
		m.health[id] = h
	}
	return h
}

// QueueRecheck schedules at most one outstanding forced poll per account.
func (m *Manager) QueueRecheck(id string) error {
	m.mu.Lock()
	account, err := m.store.Get(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if _, ok := m.providers[account.Provider]; !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown provider %q", account.Provider)
	}
	h := m.healthLocked(id)
	if h.queued {
		m.mu.Unlock()
		return nil
	}
	h.queued = true
	gen := m.generation[id]
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		m.refreshAccount(ctx, id, gen, true)
		m.mu.Lock()
		if m.generation[id] == gen {
			m.healthLocked(id).queued = false
		}
		m.mu.Unlock()
	}()
	return nil
}

// RefreshUsageAll refreshes usage for every stored account (used by the
// background sync so the UI and menu bar always read fresh data without
// depending on a client to trigger refreshes).
func (m *Manager) RefreshUsageAll(ctx context.Context) {
	// In-flight guard: overlapping cycles (slow upstream, overlapping
	// callers) would duplicate every upstream call.
	if !m.syncing.CompareAndSwap(false, true) {
		return
	}
	defer m.syncing.Store(false)
	accounts, err := m.store.List()
	if err != nil {
		return
	}
	type job struct {
		id  string
		gen uint64
	}
	jobs := []job{}
	for _, account := range accounts {
		if _, ok := m.providers[account.Provider]; ok {
			m.mu.Lock()
			jobs = append(jobs, job{id: account.ID, gen: m.generation[account.ID]})
			m.mu.Unlock()
		}
	}
	// Parallel with a small pool: each account is one upstream call.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			m.refreshAccount(ctx, j.id, j.gen, false)
		}(j)
	}
	wg.Wait()
}

// refreshAccount refreshes the token if needed and polls usage for one
// account. Serialised per account: the 60s background sync, the proxy's
// 401 path, and a manual refresh can otherwise interleave and persist a
// stale rotating refresh token, bricking the account.
func (m *Manager) refreshAccount(ctx context.Context, id string, gen uint64, force bool) provider.Usage {
	lock := m.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()
	m.mu.Lock()
	valid := m.generation[id] == gen
	m.mu.Unlock()
	if !valid {
		return provider.Usage{}
	}
	a, err := m.store.Get(id)
	if err != nil {
		return provider.Usage{}
	}
	prov, ok := m.providers[a.Provider]
	if !ok {
		return provider.Usage{}
	}
	m.mu.Lock()
	h := m.healthLocked(id)
	if !force && time.Now().Before(h.retryAt) {
		if staleSnapshot(m.lastUsage[id]) {
			delete(m.lastUsage, id)
			m.quotaRevision[id]++
		}
		cached := m.lastUsage[id]
		m.mu.Unlock()
		return cached
	}
	forceRefresh := force && h.relogin
	h.checking++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.generation[id] == gen {
			m.healthLocked(id).checking--
		}
		m.mu.Unlock()
	}()
	refresh := func() bool {
		if err := prov.Refresh(ctx, &a); err != nil {
			m.recordRefresh(id, gen, err)
			log.Printf("proxy: usage refresh for %s failed: %v", a.Email, err)
			return false
		}
		if err := m.store.Save(a); err != nil {
			log.Printf("proxy: save refreshed token: %v", err)
			m.recordRefresh(id, gen, err)
			return false
		}
		m.mu.Lock()
		m.accountRevision[id]++
		m.mu.Unlock()
		m.recordRefresh(id, gen, nil)
		return true
	}
	refreshed := prov.IsExpired(a) || forceRefresh
	if refreshed {
		if !refresh() {
			return m.recordUsage(id, gen, provider.Usage{}, false, nil)
		}
	}
	usage, err := prov.Usage(ctx, a)
	if !refreshed && usageAuthenticationFailed(err) {
		if refresh() {
			usage, err = prov.Usage(ctx, a)
		} else {
			return m.recordUsage(id, gen, provider.Usage{}, false, nil)
		}
	}
	if err != nil || !usage.Available {
		if err != nil {
			log.Printf("proxy: usage unavailable for %s (%s): %v", a.Email, a.Provider, err)
		}
		return m.recordUsage(id, gen, provider.Usage{}, false, err)
	}
	// Some providers expose plan metadata separately from usage. Save it
	// under the same account lock as rotating credentials and preferences.
	if reader, ok := prov.(interface {
		ResolvePlan(context.Context, store.Account) (string, error)
	}); ok {
		m.mu.Lock()
		due := force || time.Since(m.planChecked[id]) >= time.Hour
		if due {
			// Failures retry in five minutes; successful metadata stays fresh
			// for an hour. Manual Recheck bypasses this metadata backoff.
			m.planChecked[id] = time.Now().Add(-55 * time.Minute)
		}
		m.mu.Unlock()
		if due {
			if plan, err := reader.ResolvePlan(ctx, a); err == nil && plan != "" {
				if plan != a.Plan {
					a.Plan = plan
					err = m.store.Save(a)
				}
				if err != nil {
					log.Printf("proxy: save account plan: %v", err)
				} else {
					m.mu.Lock()
					m.planChecked[id] = time.Now()
					m.mu.Unlock()
				}
			}
		}
	}
	return m.recordUsage(id, gen, usage, true, nil)
}

// Only the usage endpoint's known HTTP 401 errors indicate a stale access
// token. In particular, a quota response body mentioning "401" is not auth.
func usageAuthenticationFailed(err error) bool {
	return errors.Is(err, provider.ErrUsageAuthRequired)
}

func (m *Manager) recordRefresh(id string, gen uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation[id] != gen {
		return
	}
	h := m.healthLocked(id)
	if err == nil {
		h.relogin = false
	} else if errors.Is(err, provider.ErrReloginRequired) {
		h.relogin = true
	}
}

func (m *Manager) recordUsage(id string, gen uint64, usage provider.Usage, success bool, err error) provider.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation[id] != gen {
		return provider.Usage{}
	}
	h := m.healthLocked(id)
	h.lastChecked = time.Now()
	if success {
		h.retryAt = time.Time{}
		h.lastSuccess = h.lastChecked
		m.lastUsage[id] = usage
		m.quotaRevision[id]++
		return usage
	}
	if errors.Is(err, provider.ErrUsageRateLimited) {
		h.retryAt = h.lastChecked.Add(usageRateLimitCooldown)
	}
	if last, ok := m.lastUsage[id]; ok {
		if staleSnapshot(last) {
			delete(m.lastUsage, id)
			m.quotaRevision[id]++
		} else if last.Available {
			return last
		}
	}
	return provider.Usage{}
}

// staleSnapshot reports whether a usage snapshot's every reset time has
// already passed: the windows provably rolled during an outage.
func staleSnapshot(u provider.Usage) bool {
	if !u.Available || len(u.Windows) == 0 {
		return false
	}
	now := time.Now().Unix()
	for _, w := range u.Windows {
		if w.ResetsAt == 0 || w.ResetsAt > now {
			return false
		}
	}
	return true
}

// refreshLock returns the per-account refresh mutex.
func (m *Manager) refreshLock(id string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshing == nil {
		m.refreshing = map[string]*sync.Mutex{}
	}
	mu, ok := m.refreshing[id]
	if !ok {
		mu = &sync.Mutex{}
		m.refreshing[id] = mu
	}
	return mu
}

// RefreshUsage queries upstream usage for one account, best effort, and
// remembers the result for the UI. The token is refreshed first when the
// expiry says so, or once after the usage endpoint rejects authentication.
func (m *Manager) RefreshUsage(ctx context.Context, a store.Account) provider.Usage {
	m.mu.Lock()
	gen := m.generation[a.ID]
	m.mu.Unlock()
	return m.refreshAccount(ctx, a.ID, gen, false)
}

// pick returns the account that should serve the next request for a
// provider: the active one when usable, otherwise the first usable account
// of that provider. An error means none.
func (m *Manager) pick(providerID string) (store.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, until := range m.exhausted {
		if now.After(until) {
			delete(m.exhausted, id)
		}
	}
	accounts, err := m.store.List()
	if err != nil {
		return store.Account{}, fmt.Errorf("list accounts: %w", err)
	}
	var active *store.Account
	for i := range accounts {
		a := accounts[i]
		if a.Provider != providerID {
			continue
		}
		if a.ID == m.active[providerID] && a.Token.AccessToken != "" {
			active = &accounts[i]
			break
		}
	}
	if active != nil {
		return *active, nil
	}
	// No usable active account: fall back to the first usable one of this
	// provider. This is the only automatic selection Switcher ever performs.
	for i := range accounts {
		a := accounts[i]
		if a.Provider == providerID && a.Token.AccessToken != "" && !m.exhaustedNow(a.ID) {
			m.active[providerID] = a.ID
			m.selectionRevision[providerID]++
			_ = m.persistLocked()
			return a, nil
		}
	}
	// Keep the client-facing message generic; details go to the log only.
	return store.Account{}, errNoAccount
}

func (m *Manager) exhaustedNow(id string) bool {
	until, ok := m.exhausted[id]
	return ok && time.Now().Before(until)
}

// persistLocked writes routing state; callers must hold m.mu.
func (m *Manager) persistLocked() error {
	exhausted := map[string]int64{}
	for id, until := range m.exhausted {
		exhausted[id] = until.Unix()
	}
	active := make(map[string]string, len(m.active))
	for providerID, id := range m.active {
		active[providerID] = id
	}
	order := append([]string(nil), m.order...)
	hidden := append([]string(nil), m.hidden...)
	return m.store.SaveState(store.State{
		Active: active, Exhausted: exhausted,
		ProviderOrder: order, HiddenProviders: hidden,
		ManagementKey: m.managementKey,
	})
}

// ServeHTTP forwards an API request to the active upstream account of the
// provider addressed by the request prefix (/codex, /claude, /grok,
// /opencode), switching and retrying per the rules on the package.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	providerID, rest, ok := m.splitPrefix(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider prefix")
		return
	}
	prov, ok := m.providers[providerID]
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider: "+providerID)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if closeErr := r.Body.Close(); closeErr != nil {
		log.Printf("proxy: close request body: %v", closeErr)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "read request body")
		return
	}
	if len(body) > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	refreshed := map[string]bool{}
	usedAutoReset := false
	for attempt := 0; attempt < 4; attempt++ {
		account, pickErr := m.pick(providerID)
		if pickErr != nil {
			writeError(w, http.StatusServiceUnavailable, pickErr.Error())
			return
		}

		if prov.IsExpired(account) && !refreshed[account.ID] {
			refreshed[account.ID] = true
			account, err = m.refreshForProxy(r.Context(), prov, account, false)
			if err != nil {
				log.Printf("proxy: refresh %s failed: %v", account.Email, err)
				continue // pick() selects another account
			}
		}

		resetBeforeForward := m.resetID(account.ID)
		resp, err := m.forward(prov, account, rest, r, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
			return
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshed[account.ID]:
			refreshed[account.ID] = true
			drain(resp)
			account, err = m.refreshForProxy(r.Context(), prov, account, true)
			if err != nil {
				log.Printf("proxy: refresh %s after 401 failed: %v", account.Email, err)
				continue
			}
			continue // retry the same account with fresh tokens

		case resp.StatusCode >= 400:
			body429, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
			drain(resp)
			if readErr != nil || len(body429) > 1<<20 {
				writeError(w, http.StatusBadGateway, "upstream error response too large or unreadable")
				return
			}
			until, exhausted := prov.ParseRateLimit(r.Context(), account, resp.StatusCode, body429)
			if !exhausted {
				copyResponseStatus(w, resp.StatusCode, body429, resp.Header)
				return
			}
			// A concurrent request may have restored this account while this
			// older upstream response was in flight. Retry once instead of
			// re-parking it based on the pre-reset 429.
			if !m.markExhaustedIfResetUnchanged(account.ID, until, resetBeforeForward) {
				if !usedAutoReset {
					usedAutoReset = true
					continue
				}
				copyResponseStatus(w, resp.StatusCode, body429, resp.Header)
				return
			}
			log.Printf("proxy: %s exhausted until %s", account.Email, until.Format(time.RFC3339))
			// Failover first: another usable account costs nothing extra,
			// while a banked reset is a finite credit.
			if next := m.nextAvailable(account.ID, providerID); next != "" {
				m.setActive(providerID, next)
				continue // transparent retry on the new account
			}
			// Last resort: with nowhere to fail over, spend one of this
			// account's banked resets and retry the same account.
			if !usedAutoReset && m.autoUseBankedReset(r.Context(), prov, account) {
				usedAutoReset = true
				continue
			}
			writeJSON(w, resp.StatusCode, map[string]any{
				"error": map[string]any{
					"type":    "usage_limit_reached",
					"message": "every account is out of usage; switch or sign in to another one",
				},
			})
			return

		default:
			copyResponse(w, resp)
			return
		}
	}
	writeError(w, http.StatusServiceUnavailable, "upstream kept failing; see Switcher logs")
}

// refreshForProxy shares the token lock with usage checks and account writes.
// If another request already rotated the token after a 401, use that token.
func (m *Manager) refreshForProxy(ctx context.Context, prov provider.Provider, picked store.Account, force bool) (store.Account, error) {
	lock := m.refreshLock(picked.ID)
	lock.Lock()
	defer lock.Unlock()
	a, err := m.store.Get(picked.ID)
	if err != nil {
		return picked, err
	}
	if (force && a.Token.AccessToken != picked.Token.AccessToken) || (!force && !prov.IsExpired(a)) {
		return a, nil
	}
	m.mu.Lock()
	gen := m.generation[a.ID]
	m.mu.Unlock()
	if err := prov.Refresh(ctx, &a); err != nil {
		m.recordRefresh(a.ID, gen, err)
		m.deactivate(a.ID)
		return a, err
	}
	if err := m.store.Save(a); err != nil {
		log.Printf("proxy: save refreshed token: %v", err)
		m.recordRefresh(a.ID, gen, err)
		m.deactivate(a.ID)
		return picked, fmt.Errorf("save refreshed token: %w", err)
	}
	m.mu.Lock()
	m.accountRevision[a.ID]++
	m.mu.Unlock()
	m.recordRefresh(a.ID, gen, nil)
	return a, nil
}

// forward performs one upstream request with the account's credentials.
func (m *Manager) forward(prov provider.Provider, account store.Account, path string, r *http.Request, body []byte) (*http.Response, error) {
	target := prov.UpstreamURL(path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Pass client headers through, minus hop-by-hop headers and the auth
	// headers that must reflect the switched account instead of the CLI's
	// own login.
	for key, vals := range r.Header {
		if isHopByHop(key) || isConnectionToken(r.Header.Get("Connection"), key) ||
			strings.EqualFold(key, "Cookie") || strings.EqualFold(key, "X-Switcher-CSRF") {
			continue
		}
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}
	if err := prov.ApplyAuth(req, account); err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// nextAvailable returns another account that can serve traffic, the given
// account excluded.
func (m *Manager) nextAvailable(exclude, providerID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextAvailableLocked(exclude, providerID)
}

// nextAvailableLocked requires m.mu.
func (m *Manager) nextAvailableLocked(exclude, providerID string) string {
	accounts, err := m.store.List()
	if err != nil {
		return ""
	}
	for _, a := range accounts {
		if a.Provider != providerID {
			continue
		}
		if a.ID == exclude || a.Token.AccessToken == "" || m.exhaustedNow(a.ID) {
			continue
		}
		return a.ID
	}
	return ""
}

func (m *Manager) setActive(providerID, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active[providerID] = id
	m.selectionRevision[providerID]++
	if err := m.persistLocked(); err != nil {
		log.Printf("proxy: persist active account: %v", err)
	}
	log.Printf("proxy: switched active %s account to %s", providerID, id)
}

func (m *Manager) markExhausted(id string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markExhaustedLocked(id, until)
}

// The epoch check and park must be one critical section: redemption may
// complete at any point while another response is being handled.
func (m *Manager) markExhaustedIfResetUnchanged(id string, until time.Time, expected string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resetEvents[id].ID != expected {
		return false
	}
	m.markExhaustedLocked(id, until)
	return true
}

func (m *Manager) markExhaustedLocked(id string, until time.Time) {
	m.exhausted[id] = until
	m.quotaRevision[id]++
	if err := m.persistLocked(); err != nil {
		log.Printf("proxy: persist exhaustion: %v", err)
	}
}

// SetAutoUseResetPolicy wires the user's preference for spending a banked
// reset when an account runs out of usage. The resolver receives the
// exhausted account so a per-account override can win over the global
// setting. Passing nil (the zero value) disables the feature entirely.
func (m *Manager) SetAutoUseResetPolicy(resolver func(store.Account) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoUseReset = resolver
}

// autoUseBankedReset redeems one banked reset for an exhausted account and
// lifts its local park so the request can retry on the same account. It is
// a last resort: callers only reach it after failover found no other usable
// account. Returns true when the account is ready to serve again. At most
// one credit is spent per resolution, and a provider without banked resets
// (or a user who left the feature off) is never touched.
func (m *Manager) autoUseBankedReset(ctx context.Context, prov provider.Provider, account store.Account) bool {
	m.mu.Lock()
	resolver := m.autoUseReset
	m.mu.Unlock()
	if resolver == nil || !resolver(account) {
		return false
	}
	rc, ok := prov.(provider.ResetCreditProvider)
	if !ok {
		return false
	}
	// Serialize per account so a burst of concurrent requests cannot spend
	// one credit each: whoever wins the lock lifts the park and the rest
	// retry without redeeming anything.
	lock := m.refreshLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	// Re-read after taking the account lock: a queued request must honor a
	// recent preference change, token rotation, or completed redemption.
	fresh, err := m.store.Get(account.ID)
	if err != nil || !resolver(fresh) {
		return false
	}
	account = fresh
	if _, stillExhausted := m.Exhausted(account.ID); !stillExhausted {
		return true
	}
	if event := m.LastReset(account.ID); event != nil && event.Pending {
		return false
	}
	m.mu.Lock()
	cooldownUntil := m.autoResetAt[account.ID]
	m.mu.Unlock()
	if time.Now().Before(cooldownUntil) {
		return false
	}
	credits, err := rc.ListResetCredits(ctx, account)
	if err != nil {
		log.Printf("proxy: list banked resets for %s: %v", account.Email, err)
		return false
	}
	if len(credits) == 0 {
		return false
	}
	credits = m.AvailableResetCredits(account.ID, credits)
	if len(credits) == 0 {
		return false
	}
	outcome, err := rc.ConsumeResetCredit(ctx, account, credits[0].ID)
	if err != nil {
		log.Printf("proxy: auto-use banked reset for %s: %v", account.Email, err)
		return false
	}
	switch outcome {
	case "reset", "already_redeemed":
		log.Printf("proxy: auto-used a banked reset for %s (%s)", account.Email, outcome)
		m.recordBankedReset(account.ID, credits[0].ID, outcome, credits[1:], true)
		return true
	default:
		log.Printf("proxy: banked reset for %s did not clear the limit (%s)", account.Email, outcome)
		return false
	}
}

// deactivate parks an account without a known reset time (e.g. a failed
// token refresh) so the next pick moves on. One hour is enough to keep a
// broken account out of the way without losing it forever.
func (m *Manager) deactivate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.exhausted[id]; !ok {
		m.exhausted[id] = time.Now().Add(time.Hour)
	}
	for providerID, activeID := range m.active {
		if activeID == id {
			if next := m.nextAvailableLocked(id, providerID); next != "" {
				m.active[providerID] = next
			} else {
				delete(m.active, providerID)
			}
			m.selectionRevision[providerID]++
		}
	}
	_ = m.persistLocked()
}

// splitPrefix splits "/codex/v1/x" into ("codex", "/v1/x"). The path must
// start with a registered provider prefix.
func (m *Manager) splitPrefix(path string) (providerID, rest string, ok bool) {
	for id := range m.providers {
		prefix := "/" + id
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return id, strings.TrimPrefix(path, prefix), true
		}
	}
	return "", "", false
}

// copyResponseStatus relays an upstream error body we already buffered.
func copyResponseStatus(w http.ResponseWriter, status int, body []byte, header http.Header) {
	for key, vals := range header {
		if isHopByHop(key) || key == "Set-Cookie" || key == "Content-Length" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(status)
	if len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// drain reads and discards a response we will not forward.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// copyResponse streams the upstream response to the client with immediate
// flushing so SSE events arrive in real time.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	connectionTokens := resp.Header.Values("Connection")
	for key, vals := range resp.Header {
		if isHopByHop(key) || isConnectionToken(strings.Join(connectionTokens, ","), key) || key == "Set-Cookie" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// hopByHopHeaders are never meaningful end to end (RFC 9110 §7.6.1).
func isHopByHop(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// isConnectionToken reports whether key is named in a Connection header.
func isConnectionToken(connectionHeader, key string) bool {
	for _, token := range strings.Split(connectionHeader, ",") {
		if strings.EqualFold(strings.TrimSpace(token), key) {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
