// Package login orchestrates OAuth logins: browser (PKCE callback) flows
// and device-code flows, where the provider polls upstream itself and the
// outcome simply takes longer to land. Results are kept briefly so a
// completed login is never lost to a poll race, and every successful
// exchange is persisted through the configured hook the moment it
// completes.
package login

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"log"
	"switcher/internal/provider"
	"switcher/internal/store"
)

const (
	// pendingTTL bounds how long a started-but-unfinished login stays alive.
	pendingTTL = 10 * time.Minute
	// resultTTL bounds how long a finished outcome stays pollable.
	resultTTL = 15 * time.Minute
	// pollWait is how long a single UI poll may block.
	pollWait = time.Second
)

// Handle identifies one started login attempt.
type Handle struct {
	State           string `json:"state"`
	URL             string `json:"url,omitempty"`
	Kind            string `json:"kind"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
}

// ErrUnknown is returned for unknown or expired logins.
var ErrUnknown = errors.New("unknown or expired login attempt")

type pending struct {
	provider      provider.Provider
	reloginTarget string
	done          chan struct{}
	started       time.Time
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
	// lookup reads one stored account, used by relogin flows to adopt the
	// existing account id instead of minting a duplicate.
	lookup func(id string) (store.Account, error)

	mu      sync.Mutex
	pending map[string]*pending
	results map[string]result
}

// New creates a login manager. persist is called exactly once per
// successful login, from the goroutine that completes it.
func New(persist func(store.Account) error, lookup func(id string) (store.Account, error)) *Manager {
	return &Manager{
		persist: persist,
		lookup:  lookup,
		pending: map[string]*pending{},
		results: map[string]result{},
	}
}

// Start begins a browser (PKCE) login for the given provider.
func (m *Manager) Start(ctx context.Context, prov provider.Provider, reloginTarget string) (Handle, error) {
	info, err := prov.LoginStart(ctx)
	if err != nil {
		return Handle{}, fmt.Errorf("start login: %w", err)
	}
	m.track(info.State, prov, reloginTarget)
	return Handle{State: info.State, URL: info.URL, Kind: info.Kind}, nil
}

// StartDevice begins a device-code login and spawns the background poller:
// when the user authorizes, the flow completes itself and persists. Like
// Start, reloginTarget adopts the existing account id on completion.
func (m *Manager) StartDevice(ctx context.Context, prov provider.Provider, reloginTarget string) (Handle, error) {
	info, poll, err := prov.DeviceStart(ctx)
	if err != nil {
		return Handle{}, fmt.Errorf("start device login: %w", err)
	}
	m.track(info.State, prov, reloginTarget)
	go func() {
		account, err := poll(ctx)
		if err == nil && m.lookup != nil && reloginTarget != "" {
			// Relogin: adopt the existing account id so the tokens
			// overwrite in place and the active slot survives.
			if old, lerr := m.lookup(reloginTarget); lerr == nil &&
				old.Provider == account.Provider && old.Email == account.Email {
				account.ID = old.ID
			}
		}
		if err == nil && m.persist != nil {
			err = m.persist(account)
		}
		m.mu.Lock()
		m.gcResultsLocked()
		m.results[info.State] = result{account: account, err: err, at: time.Now()}
		delete(m.pending, info.State)
		m.mu.Unlock()
	}()
	return Handle{State: info.State, Kind: info.Kind, VerificationURL: info.VerificationURL, UserCode: info.UserCode}, nil
}

// Complete finishes a browser flow identified by state. It is called by the
// OAuth callback endpoint. The exchanged account is persisted immediately
// so completion never depends on anyone polling.
func (m *Manager) Complete(ctx context.Context, state, code string) error {
	m.mu.Lock()
	p, ok := m.pending[state]
	delete(m.pending, state)
	m.mu.Unlock()
	if !ok {
		// The callback can arrive without a usable state (OpenAI omits it,
		// Anthropic puts it in a fragment): a single in-flight login is the
		// one being completed.
		if state == "" {
			state = m.SinglePending()
		}
		if state == "" {
			log.Printf("login: callback rejected, %d pending, none resolvable", len(m.pendingStates()))
			return ErrUnknown
		}
		m.mu.Lock()
		p, ok = m.pending[state]
		delete(m.pending, state)
		m.mu.Unlock()
		if !ok {
			return ErrUnknown
		}
	}
	log.Printf("login: completing flow for %s (state %.8s...)", p.provider.ID(), state)

	account, err := p.provider.LoginExchange(ctx, state, code)
	if err == nil && p.reloginTarget != "" {
		// Relogin: when the signed-in identity is the account being
		// re-logged, adopt its id so the tokens overwrite in place and the
		// active slot and windows survive.
		if m.lookup != nil {
			if old, lerr := m.lookup(p.reloginTarget); lerr == nil &&
				old.Provider == account.Provider && old.Email == account.Email {
				account.ID = old.ID
			}
		}
	}
	if err == nil && m.persist != nil {
		err = m.persist(account)
	}
	if err != nil {
		err = fmt.Errorf("login exchange: %w", err)
	}
	if err != nil {
		log.Printf("login: flow %.8s... failed: %v", state, err)
	} else {
		log.Printf("login: flow %.8s... completed for %s", state, account.Email)
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

// track records a new pending flow.
func (m *Manager) track(state string, prov provider.Provider, reloginTarget string) {
	m.mu.Lock()
	m.gcLocked()
	m.pending[state] = &pending{provider: prov, reloginTarget: reloginTarget, done: make(chan struct{}), started: time.Now()}
	m.mu.Unlock()
}

// SinglePending returns the only tracked pending login state, or "" when
// none or more than one exist. OpenAI's redirect does not echo the state
// parameter back on the callback URL, so a single in-flight login resolves
// by being the only one (the Codex CLI works the same way).
func (m *Manager) SinglePending() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 1 {
		for state := range m.pending {
			return state
		}
	}
	return ""
}

// pendingStates lists the tracked pending login states.
func (m *Manager) pendingStates() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.pending))
	for state := range m.pending {
		out = append(out, state)
	}
	return out
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
