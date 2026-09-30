package proxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"switcher/internal/provider"
)

var ErrNoResetCredits = errors.New("no banked reset available for this account")
var ErrResetUnsupported = errors.New("this provider has no banked resets")
var ErrResetPending = errors.New("quota is still updating after the previous reset")

func (m *Manager) resetID(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resetEvents[id].ID
}

// ResetEvent acknowledges redemption separately from provider usage polling.
// IDs are process-unique so browsers can observe automatic resets exactly once.
type ResetEvent struct {
	ID             string                 `json:"id"`
	Sequence       uint64                 `json:"sequence"`
	At             int64                  `json:"at"`
	Outcome        string                 `json:"outcome"`
	Automatic      bool                   `json:"automatic"`
	Pending        bool                   `json:"pending"`
	UsageConfirmed bool                   `json:"usage_confirmed"`
	CreditID       string                 `json:"-"`
	Remaining      []provider.ResetCredit `json:"-"`
}

func (m *Manager) LastReset(id string) *ResetEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	event, ok := m.resetEvents[id]
	if !ok {
		return nil
	}
	event.Remaining = append([]provider.ResetCredit(nil), event.Remaining...)
	return &event
}

// UseBankedReset shares the credential lock with automatic resets and token
// rotation. It returns as soon as the provider acknowledges the redemption.
func (m *Manager) UseBankedReset(ctx context.Context, id, creditID string) (string, error) {
	lock := m.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()
	account, err := m.store.Get(id)
	if err != nil {
		return "", err
	}
	creditsProvider, ok := m.providers[account.Provider].(provider.ResetCreditProvider)
	if !ok {
		return "", ErrResetUnsupported
	}
	// Pin the request to the credit the user chose. Retrying a delayed/lost
	// response must never advance to the next available credit.
	if event := m.LastReset(id); event != nil {
		if event.CreditID == creditID {
			return "already_redeemed", nil
		}
		if event.Pending {
			return "", ErrResetPending
		}
	}
	credits, err := creditsProvider.ListResetCredits(ctx, account)
	if err != nil {
		return "", fmt.Errorf("could not list banked resets: %w", err)
	}
	remaining := make([]provider.ResetCredit, 0, len(credits))
	found := false
	for _, credit := range m.AvailableResetCredits(id, credits) {
		if credit.ID == creditID {
			found = true
		} else {
			remaining = append(remaining, credit)
		}
	}
	if !found {
		return "", ErrNoResetCredits
	}
	outcome, err := creditsProvider.ConsumeResetCredit(ctx, account, creditID)
	if err != nil {
		return "", fmt.Errorf("the reset did not go through: %w", err)
	}
	if outcome == "reset" || outcome == "already_redeemed" {
		m.recordBankedReset(id, creditID, outcome, remaining, false)
	}
	return outcome, nil
}

// Called under the account lock. Keep the last known quota visible until a
// fresh response arrives rather than inventing a full balance after a reset.
func (m *Manager) recordBankedReset(id, creditID, outcome string, remaining []provider.ResetCredit, automatic bool) {
	m.mu.Lock()
	if m.spentCredits[id] == nil {
		m.spentCredits[id] = map[string]int64{}
	}
	m.spentCredits[id][creditID] = time.Now().Add(30 * 24 * time.Hour).Unix()
	m.autoResetAt[id] = time.Now().Add(autoUseResetCooldown)
	if old, ok := m.resetEvents[id]; ok && old.CreditID == creditID {
		delete(m.exhausted, id)
		_ = m.persistLocked()
		m.mu.Unlock()
		return
	}
	m.resetSerial++
	event := ResetEvent{ID: fmt.Sprintf("%s-%d", m.probeBootID, m.resetSerial), At: time.Now().Unix(), Outcome: outcome,
		Sequence:  m.resetSerial,
		Automatic: automatic, Pending: true, CreditID: creditID, Remaining: append([]provider.ResetCredit(nil), remaining...)}
	m.resetEvents[id] = event
	m.quotaRevision[id]++
	before := m.lastUsage[id]
	gen := m.generation[id]
	m.healthLocked(id).retryAt = time.Time{}
	delete(m.exhausted, id)
	_ = m.persistLocked()
	m.mu.Unlock()
	go m.refreshAfterReset(id, event.ID, gen, before)
}

func (m *Manager) refreshAfterReset(id, eventID string, gen uint64, before provider.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	confirmed := false
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		event := m.resetEvents[id]
		if event.ID == eventID {
			event.Pending = false
			event.UsageConfirmed = confirmed
			m.resetEvents[id] = event
			m.quotaRevision[id]++
		}
	}()
	for _, delay := range []time.Duration{0, time.Second, 3 * time.Second, 6 * time.Second, 10 * time.Second} {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		m.mu.Lock()
		current := m.resetEvents[id]
		valid := current.ID == eventID && m.generation[id] == gen
		m.mu.Unlock()
		if !valid {
			return
		}
		usage := m.refreshAccount(ctx, id, gen, true)
		if quotaChangedAfterReset(before, usage) {
			confirmed = true
			return
		}
		m.mu.Lock()
		rateLimited := time.Now().Before(m.healthLocked(id).retryAt)
		m.mu.Unlock()
		if rateLimited {
			return
		}
	}
}

// QuotaSnapshot makes event/revision/usage one atomic read for mutation
// responses and polling. A browser can discard an older POST snapshot.
func (m *Manager) QuotaSnapshot(id string) (*provider.Usage, *ResetEvent, uint64, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var usage *provider.Usage
	if value, ok := m.lastUsage[id]; ok {
		copied := value
		copied.Windows = append([]provider.UsageWindow(nil), value.Windows...)
		usage = &copied
	}
	var reset *ResetEvent
	if value, ok := m.resetEvents[id]; ok {
		copied := value
		copied.Remaining = append([]provider.ResetCredit(nil), value.Remaining...)
		reset = &copied
	}
	return usage, reset, m.quotaRevision[id], m.probeBootID
}

// Provider credit lists can briefly lag behind redemptions. Never re-offer
// credits this process already spent, even after a later reset event.
func (m *Manager) AvailableResetCredits(id string, credits []provider.ResetCredit) []provider.ResetCredit {
	m.mu.Lock()
	defer m.mu.Unlock()
	for credit, expiry := range m.spentCredits[id] {
		if expiry < time.Now().Unix() {
			delete(m.spentCredits[id], credit)
		}
	}
	filtered := make([]provider.ResetCredit, 0, len(credits))
	for _, credit := range credits {
		if _, spent := m.spentCredits[id][credit.ID]; !spent {
			filtered = append(filtered, credit)
		}
	}
	return filtered
}

func quotaChangedAfterReset(before, after provider.Usage) bool {
	if !after.Available || len(after.Windows) == 0 {
		return false
	}
	if !before.Available {
		return true
	}
	for _, window := range after.Windows {
		for _, old := range before.Windows {
			if old.Label == window.Label && (window.UsedPercent < old.UsedPercent || window.ResetsAt > old.ResetsAt) {
				return true
			}
		}
	}
	return false
}
