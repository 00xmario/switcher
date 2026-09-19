// Package proxy implements Switcher's request forwarding and its switching
// rules. There is exactly one active account; traffic is forwarded to it
// verbatim. The active account changes only in two cases: the user picks a
// different one, or the active account reports an exhausted usage limit —
// then Switcher moves to another usable account and transparently retries
// the in-flight request once.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// maxBodyBytes caps how much of a request body is buffered so a retry can
// resend it. Codex request payloads are well under this.
const maxBodyBytes = 64 << 20

// errNoAccount is the message returned when nothing can serve a request.
var errNoAccount = errors.New("no usable account is active; add one in the Switcher UI")

// Upstream result of a request that hit an exhausted account.
type usageLimitBody struct {
	Error struct {
		Type     string `json:"type"`
		ResetsAt int64  `json:"resets_at"`
	} `json:"error"`
}

// Manager owns the routing state and the forwarding handler.
type Manager struct {
	mu        sync.Mutex
	store     *store.Store
	providers map[string]provider.Provider
	active    string
	exhausted map[string]time.Time
	lastUsage map[string]provider.Usage
}

// New loads persisted state and returns the proxy manager.
func New(st *store.Store, providers map[string]provider.Provider) (*Manager, error) {
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
	return &Manager{
		store:     st,
		providers: providers,
		active:    state.Active,
		exhausted: exhausted,
		lastUsage: map[string]provider.Usage{},
	}, nil
}

// ActiveID returns the currently selected account ("" when none).
func (m *Manager) ActiveID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// Activate selects an account explicitly. This is the only manual switch
// path; it succeeds even when the account is currently marked exhausted
// (the user is in charge).
func (m *Manager) Activate(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.store.Get(id); err != nil {
		return err
	}
	m.active = id
	delete(m.exhausted, id)
	return m.persistLocked()
}

// Remove forgets routing bookkeeping for a deleted account.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exhausted, id)
	delete(m.lastUsage, id)
	if m.active == id {
		m.active = ""
		_ = m.persistLocked()
	}
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

// RefreshUsage queries upstream usage for one account, best effort, and
// remembers the result for the UI. An expired token is refreshed first.
func (m *Manager) RefreshUsage(ctx context.Context, a store.Account) provider.Usage {
	prov, ok := m.providers[a.Provider]
	if !ok {
		return provider.Usage{}
	}
	if prov.IsExpired(a) {
		if err := prov.Refresh(ctx, &a); err != nil {
			return provider.Usage{}
		}
		if err := m.store.Save(a); err != nil {
			log.Printf("proxy: save refreshed token: %v", err)
		}
	}
	usage, err := prov.Usage(ctx, a)
	if err != nil {
		usage = provider.Usage{}
	}
	m.mu.Lock()
	m.lastUsage[a.ID] = usage
	m.mu.Unlock()
	return usage
}

// pick returns the account that should serve the next request: the active
// one when usable, otherwise the first usable account. nil means none.
func (m *Manager) pick() (store.Account, error) {
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
		if accounts[i].ID == m.active && accounts[i].Token.AccessToken != "" {
			active = &accounts[i]
			break
		}
	}
	if active != nil {
		return *active, nil
	}
	// No usable active account: fall back to the first usable one. This is
	// the only automatic selection Switcher ever performs.
	for i := range accounts {
		a := accounts[i]
		if a.Token.AccessToken != "" && !m.exhaustedNow(a.ID) {
			m.active = a.ID
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
	return m.store.SaveState(store.State{Active: m.active, Exhausted: exhausted})
}

// ServeHTTP forwards an API request to the active upstream account,
// switching and retrying per the rules described on the package.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	for attempt := 0; attempt < 4; attempt++ {
		account, pickErr := m.pick()
		if pickErr != nil {
			writeError(w, http.StatusServiceUnavailable, pickErr.Error())
			return
		}
		prov := m.providers[account.Provider]
		if prov == nil {
			writeError(w, http.StatusServiceUnavailable, "no provider for account "+account.Provider)
			return
		}

		if prov.IsExpired(account) && !refreshed[account.ID] {
			refreshed[account.ID] = true
			if refreshErr := prov.Refresh(r.Context(), &account); refreshErr != nil {
				log.Printf("proxy: refresh %s failed: %v", account.Email, refreshErr)
				m.deactivate(account.ID)
				continue // pick() selects another account
			}
			if err := m.store.Save(account); err != nil {
				log.Printf("proxy: save refreshed token: %v", err)
			}
		}

		resp, err := m.forward(r, prov, account, body)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
			return
		}

		switch {
		case resp.StatusCode == http.StatusUnauthorized && !refreshed[account.ID]:
			refreshed[account.ID] = true
			drain(resp)
			if refreshErr := prov.Refresh(r.Context(), &account); refreshErr != nil {
				log.Printf("proxy: refresh %s after 401 failed: %v", account.Email, refreshErr)
				m.deactivate(account.ID)
				continue
			}
			if err := m.store.Save(account); err != nil {
				log.Printf("proxy: save refreshed token: %v", err)
			}
			continue // retry the same account with fresh tokens

		case resp.StatusCode == http.StatusTooManyRequests:
			until, isLimit := classifyUsageLimit(resp)
			drain(resp)
			if !isLimit {
				copyResponse(w, resp)
				return
			}
			log.Printf("proxy: %s exhausted until %s", account.Email, time.Unix(until, 0).Format(time.RFC3339))
			m.markExhausted(account.ID, until)
			if next := m.nextAvailable(account.ID); next != "" {
				m.setActive(next)
				continue // transparent retry on the new account
			}
			writeJSON(w, http.StatusTooManyRequests, map[string]any{
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

// forward performs one upstream request with the account's credentials.
func (m *Manager) forward(r *http.Request, prov provider.Provider, account store.Account, body []byte) (*http.Response, error) {
	target := prov.UpstreamURL(r.URL.Path)
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
		if isHopByHop(key) || isConnectionToken(r.Header.Get("Connection"), key) {
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
func (m *Manager) nextAvailable(exclude string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nextAvailableLocked(exclude)
}

// nextAvailableLocked is nextAvailable for callers already holding m.mu.
func (m *Manager) nextAvailableLocked(exclude string) string {
	accounts, err := m.store.List()
	if err != nil {
		return ""
	}
	for _, a := range accounts {
		if a.ID == exclude || a.Token.AccessToken == "" || m.exhaustedNow(a.ID) {
			continue
		}
		return a.ID
	}
	return ""
}

func (m *Manager) setActive(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = id
	if err := m.persistLocked(); err != nil {
		log.Printf("proxy: persist active account: %v", err)
	}
	log.Printf("proxy: switched active account to %s", id)
}

func (m *Manager) markExhausted(id string, until int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exhausted[id] = time.Unix(until, 0)
	if err := m.persistLocked(); err != nil {
		log.Printf("proxy: persist exhaustion: %v", err)
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
	if m.active == id {
		m.active = m.nextAvailableLocked(id)
	}
	_ = m.persistLocked()
}

// classifyUsageLimit reads a 429 response body once and reports whether it
// is a usage-limit exhaustion (the switch trigger) and, if so, when the
// upstream says usage resets.
func classifyUsageLimit(resp *http.Response) (resetsAt int64, isLimit bool) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, false
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var parsed usageLimitBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, false
	}
	if parsed.Error.Type != "usage_limit_reached" {
		return 0, false
	}
	if parsed.Error.ResetsAt <= 0 {
		return time.Now().Add(time.Hour).Unix(), true
	}
	return parsed.Error.ResetsAt, true
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
