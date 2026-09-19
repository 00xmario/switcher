// Package login orchestrates browser OAuth flows: start a login, let the
// OAuth callback complete it in the background, and let the UI poll for
// the outcome. Results are kept briefly so a completed login is never lost
// to a poll race, and every successful exchange is persisted through the
// configured hook the moment it completes.
package login

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

const (
	// pendingTTL bounds how long a started-but-unfinished login stays alive.
	pendingTTL = 10 * time.Minute
	// resultTTL bounds how long a finished outcome stays pollable.
	resultTTL = 5 * time.Minute
	// defaultWait is how long a single poll may block.
	defaultWait = time.Second
)

// Handle identifies one started login attempt.
type Handle struct {
	State string `json:"state"`
	URL   string `json:"url"`
}

// ErrUnknown is returned for unknown or expired logins.
var ErrUnknown = errors.New("unknown or expired login attempt")

type pending struct {
	provider provider.Provider
	done     chan struct{}
	started  time.Time
}

// result is the terminal outcome of one login flow, kept briefly so the UI
// can observe it even if it polls after the callback already landed.
type result struct {
	account store.Account
	err     error
	at      time.Time
}

// Manager tracks pending and recently-finished login flows across providers.
type Manager struct {
	persist func(store.Account) error

	mu      sync.Mutex
	pending map[string]*pending
	results map[string]result
}

// New creates a login manager. persist is called exactly once per
// successful login exchange, from the callback goroutine.
func New(persist func(store.Account) error) *Manager {
	return &Manager{
		persist: persist,
		pending: map[string]*pending{},
		results: map[string]result{},
	}
}

// Start begins an OAuth flow for the given provider and returns the URL
// the user must open plus the handle to poll.
func (m *Manager) Start(ctx context.Context, prov provider.Provider) (Handle, error) {
	url, state, err := prov.LoginStart(ctx)
	if err != nil {
		return Handle{}, fmt.Errorf("start login: %w", err)
	}
	p := &pending{provider: prov, done: make(chan struct{}), started: time.Now()}
	m.mu.Lock()
	m.gcLocked()
	m.pending[state] = p
	m.mu.Unlock()
	return Handle{State: state, URL: url}, nil
}

// Complete finishes the flow identified by state. It is called by the OAuth
// callback endpoint. The exchanged account is persisted immediately (via
// the persist hook) so completion never depends on anyone polling.
func (m *Manager) Complete(ctx context.Context, state, code string) error {
	m.mu.Lock()
	p, ok := m.pending[state]
	delete(m.pending, state)
	m.mu.Unlock()
	if !ok {
		return ErrUnknown
	}

	account, err := p.provider.LoginExchange(ctx, state, code)
	if err == nil && m.persist != nil {
		err = m.persist(account)
	}
	if err != nil {
		// Record the failure so the UI learns why, then surface it to the
		// callback page as well.
		err = fmt.Errorf("login exchange: %w", err)
	}
	m.mu.Lock()
	m.gcResultsLocked()
	m.results[state] = result{account: account, err: err, at: time.Now()}
	m.mu.Unlock()
	close(p.done)
	return err
}

// Outcome reports the current status of a login flow. finished=true means
// the outcome (success or failure) is being reported with this call.
func (m *Manager) Outcome(state string, wait time.Duration) (account store.Account, err error, finished bool) {
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		if res, ok := m.results[state]; ok {
			delete(m.results, state)
			m.mu.Unlock()
			return res.account, res.err, true
		}
		p, ok := m.pending[state]
		m.mu.Unlock()
		if !ok {
			return store.Account{}, ErrUnknown, true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return store.Account{}, nil, false
		}
		select {
		case <-p.done:
			// The result is now in m.results; loop picks it up.
			continue
		case <-time.After(remaining):
			return store.Account{}, nil, false
		}
	}
}

// gcLocked drops pending logins older than the TTL.
func (m *Manager) gcLocked() {
	now := time.Now()
	for state, p := range m.pending {
		if now.Sub(p.started) > pendingTTL {
			delete(m.pending, state)
		}
	}
	m.gcResultsLocked()
}

// gcResultsLocked drops finished outcomes older than the TTL.
func (m *Manager) gcResultsLocked() {
	now := time.Now()
	for state, res := range m.results {
		if now.Sub(res.at) > resultTTL {
			delete(m.results, state)
		}
	}
}
