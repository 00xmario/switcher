package proxy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Claude moves off an account that runs out of usage. Claude Code's own login
// switches after a usage poll shows its account at the limit; a Claude Desktop
// conversation moves when Anthropic refuses one of its requests for that
// reason (desktop_credentials.go). Both take the account with the most usage
// left, and both follow the "Switch Claude automatically" setting.

// autoSwitchCooldown keeps an account from being switched away from twice in
// quick succession, so a provider whose usage lags cannot make Switcher flip
// back and forth. A switch that did not go through, for instance because
// Claude Code held its lock, is tried again after autoSwitchRetry.
const (
	autoSwitchCooldown = 10 * time.Minute
	autoSwitchRetry    = time.Minute
	// takeoverFreshness is how recent an account's last successful usage
	// poll must be for it to take over: an account Switcher has not heard
	// from lately may need a new sign-in or have run out meanwhile.
	takeoverFreshness = 15 * time.Minute
	// manualHold keeps an account the user picked while it was at its limit,
	// for example one with extra usage, from being switched away from.
	manualHold = time.Hour
)

var errSwitchOvertaken = errors.New("the switch was overtaken")

// AutoSwitch is the latest automatic switch, for the dashboard.
type AutoSwitch struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Where    string `json:"where"` // "claude_code" or "desktop"
	From     string `json:"from"`
	To       string `json:"to"`
	At       int64  `json:"at"`
}

// SetClaudeAutoSwitch wires the user's choice; nil leaves the feature off.
func (m *Manager) SetClaudeAutoSwitch(on func() bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claudeAuto = on
}

func (m *Manager) claudeAutoOn() bool {
	m.mu.Lock()
	on := m.claudeAuto
	m.mu.Unlock()
	return on != nil && on()
}

// LastAutoSwitch returns the latest automatic switch, if any.
func (m *Manager) LastAutoSwitch() *AutoSwitch {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastAutoSwitch.ID == "" {
		return nil
	}
	event := m.lastAutoSwitch
	return &event
}

func (m *Manager) recordAutoSwitchLocked(providerID, where, from, to string) {
	m.autoSwitchSerial++
	m.lastAutoSwitch = AutoSwitch{ID: fmt.Sprintf("%s-%d", m.probeBootID, m.autoSwitchSerial), Provider: providerID,
		Where: where, From: from, To: to, At: time.Now().Unix()}
}

// outOfUsageLocked reports whether an account has run out: parked after a
// refused request, or its latest usage shows the session or weekly window at
// the limit. A model's own window does not make the whole account run out.
// Requires m.mu.
func (m *Manager) outOfUsageLocked(id string) bool {
	if m.exhaustedNow(id) {
		return true
	}
	u, ok := m.cachedUsageLocked(id)
	if !ok {
		return false
	}
	for _, w := range u.Windows {
		if (w.Label == "Session" || w.Label == "Weekly") && w.UsedPercent >= 100 {
			return true
		}
	}
	return false
}

// headroomLocked is the share left in an account's tightest session or weekly
// window, or -1 when it cannot take over: no credentials, parked, needing a
// new sign-in, or with usage Switcher has not seen. Requires m.mu.
func (m *Manager) headroomLocked(a store.Account) int {
	if !hasCredentials(a) || m.exhaustedNow(a.ID) {
		return -1
	}
	if h := m.health[a.ID]; h == nil || h.relogin || h.lastSuccess.IsZero() || time.Since(h.lastSuccess) > takeoverFreshness {
		return -1
	}
	u, ok := m.cachedUsageLocked(a.ID)
	if !ok || !u.Available {
		return -1
	}
	left, seen := 100, false
	for _, w := range u.Windows {
		if w.Label != "Session" && w.Label != "Weekly" {
			continue
		}
		seen = true
		left = min(left, 100-w.UsedPercent)
	}
	if !seen || left <= 0 {
		return -1
	}
	return left
}

// takeoverLocked picks the account of providerID with the most usage left,
// other than exclude, or "" when none has room. native asks for an account
// whose identity Claude Code's login can carry. Requires m.mu.
func (m *Manager) takeoverLocked(providerID, exclude string, native bool) (string, error) {
	accounts, err := m.store.List()
	if err != nil {
		return "", err
	}
	best, bestLeft := "", 0
	for _, a := range accounts {
		if a.Provider != providerID || a.ID == exclude || (native && a.Token.AccountID == "") {
			continue
		}
		if left := m.headroomLocked(a); left > bestLeft {
			best, bestLeft = a.ID, left
		}
	}
	return best, nil
}

// ClaudeRoute is the Claude account a paired Mac's Desktop request should use
// on this host: the one it asked for, unless that account has run out of
// usage and Claude switches automatically, then the one with the most usage
// left.
func (m *Manager) ClaudeRoute(account string) string {
	if account == "" || !m.claudeAutoOn() {
		return account
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.outOfUsageLocked(account) {
		delete(m.claudeReroute, account)
		return account
	}
	// Keep the same stand-in while it has room, so a conversation does not
	// hop between accounts and lose its cache.
	if to := m.claudeReroute[account]; to != "" {
		if a, err := m.store.Get(to); err == nil && m.headroomLocked(a) > 0 {
			return to
		}
	}
	if next, err := m.takeoverLocked("claude", account, false); err == nil && next != "" {
		m.claudeReroute[account] = next
		return next
	}
	return account
}

// AutoSwitchNative moves a native login, Claude Code's, off an account that
// has run out of usage to the account of the same provider with the most
// usage left. It runs after a usage poll, outside every lock the poll holds,
// on its own context so a switch is never cut off halfway, and moves away
// from an account at most once per cooldown. It only acts while Claude Code
// is signed in with an account Switcher manages.
func (m *Manager) AutoSwitchNative() {
	if !m.claudeAutoOn() || !m.autoSwitching.CompareAndSwap(false, true) {
		return
	}
	defer m.autoSwitching.Store(false)
	for providerID, prov := range m.providers {
		native, ok := prov.(provider.NativeLoginProvider)
		if !ok || !native.NativeEnabled() {
			continue
		}
		status := native.NativeStatus()
		if status.Condition != "ready" || status.ActiveID == "" {
			continue
		}
		from := status.ActiveID
		due := func() bool {
			now := time.Now()
			return m.outOfUsageLocked(from) && now.After(m.autoSwitchAt[from]) && now.After(m.manualHold[from])
		}
		m.mu.Lock()
		if !due() {
			m.mu.Unlock()
			continue
		}
		to, err := m.takeoverLocked(providerID, from, true)
		if err == nil && to != "" {
			m.autoSwitchAt[from] = time.Now().Add(autoSwitchRetry)
		}
		m.mu.Unlock()
		if err != nil || to == "" {
			continue
		}
		// Right before switching, under the switch lock, make sure nobody
		// switched meanwhile and the account is still out.
		still := func() bool {
			if native.NativeStatus().ActiveID != from {
				return false
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.outOfUsageLocked(from) && time.Now().After(m.manualHold[from])
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		_, err = m.activateNative(ctx, to, still)
		cancel()
		if errors.Is(err, errSwitchOvertaken) {
			continue
		}
		if err != nil {
			log.Printf("proxy: Claude Code's account ran out of usage, but switching its login did not go through; trying again in a minute")
			continue
		}
		m.mu.Lock()
		m.autoSwitchAt[from] = time.Now().Add(autoSwitchCooldown)
		m.recordAutoSwitchLocked(providerID, "claude_code", from, to)
		m.mu.Unlock()
		log.Printf("proxy: Claude Code's account ran out of usage; switched its login to the account with the most usage left")
	}
}
