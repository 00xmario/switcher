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
	mu            sync.Mutex
	store         *store.Store
	providers     map[string]provider.Provider
	active        map[string]string // provider -> active account id
	exhausted     map[string]time.Time
	lastUsage     map[string]provider.Usage
	order         []string // display order of provider sections
	hidden        []string // providers dismissed from the UI
	managementKey string   // hub management key, kept in state.json
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
	// Providers registered but never ordered land at the end, stable.
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
		store:         st,
		providers:     providers,
		active:        active,
		exhausted:     exhausted,
		lastUsage:     map[string]provider.Usage{},
		order:         order,
		hidden:        hidden,
		managementKey: state.ManagementKey,
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
	delete(m.exhausted, id)
	return m.persistLocked()
}

// Remove forgets routing bookkeeping for a deleted account.
func (m *Manager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exhausted, id)
	delete(m.lastUsage, id)
	for providerID, activeID := range m.active {
		if activeID == id {
			delete(m.active, providerID)
			_ = m.persistLocked()
		}
	}
}

// ClearExhausted lifts a parked/exhausted mark on an account (e.g. after
// a banked reset was redeemed for it).
func (m *Manager) ClearExhausted(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.exhausted, id)
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

// RefreshUsageAll refreshes usage for every stored account (used by the
// background sync so the UI and menu bar always read fresh data without
// depending on a client to trigger refreshes).
func (m *Manager) RefreshUsageAll(ctx context.Context) {
	accounts, err := m.store.List()
	if err != nil {
		return
	}
	for _, account := range accounts {
		prov, ok := m.providers[account.Provider]
		if !ok {
			continue
		}
		if prov.IsExpired(account) {
			if err := prov.Refresh(ctx, &account); err == nil {
				_ = m.store.Save(account)
			}
		}
		usage, uerr := prov.Usage(ctx, account)
		if uerr != nil {
			// Keep the last good snapshot: a single failed poll must not
			// blank out usage the UI was showing a minute ago.
			log.Printf("proxy: usage sync for %s (%s) failed, keeping last value: %v", account.Email, account.Provider, uerr)
			continue
		}
		m.mu.Lock()
		m.lastUsage[account.ID] = usage
		m.mu.Unlock()
	}
}

// RefreshUsage queries upstream usage for one account, best effort, and
// remembers the result for the UI. The token is refreshed first when the
// expiry says so, and once more on any usage failure: stored expiry
// timestamps can go stale after credential rotation, so a failing Usage
// call must never be trusted as final.
func (m *Manager) RefreshUsage(ctx context.Context, a store.Account) provider.Usage {
	prov, ok := m.providers[a.Provider]
	if !ok {
		return provider.Usage{}
	}
	refresh := func() bool {
		if err := prov.Refresh(ctx, &a); err != nil {
			log.Printf("proxy: usage refresh for %s failed: %v", a.Email, err)
			return false
		}
		if err := m.store.Save(a); err != nil {
			log.Printf("proxy: save refreshed token: %v", err)
		}
		return true
	}
	if prov.IsExpired(a) && !refresh() {
		return provider.Usage{}
	}
	usage, err := prov.Usage(ctx, a)
	if err != nil && refresh() {
		// Stored expiry timestamps can go stale after credential rotation:
		// retry once with a fresh token before giving up.
		usage, err = prov.Usage(ctx, a)
	}
	if err != nil {
		log.Printf("proxy: usage unavailable for %s (%s): %v", a.Email, a.Provider, err)
		if last, ok := m.LastUsage(a.ID); ok && last.Available {
			return last
		}
		usage = provider.Usage{}
	}
	m.mu.Lock()
	m.lastUsage[a.ID] = usage
	m.mu.Unlock()
	return usage
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
	for attempt := 0; attempt < 4; attempt++ {
		account, pickErr := m.pick(providerID)
		if pickErr != nil {
			writeError(w, http.StatusServiceUnavailable, pickErr.Error())
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

		resp, err := m.forward(prov, account, rest, r, body)
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

		case resp.StatusCode >= 400:
			body429, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			drain(resp)
			until, exhausted := prov.ParseRateLimit(r.Context(), account, resp.StatusCode, body429)
			if !exhausted {
				copyResponseStatus(w, resp.StatusCode, body429, resp.Header)
				return
			}
			log.Printf("proxy: %s exhausted until %s", account.Email, until.Format(time.RFC3339))
			m.markExhausted(account.ID, until)
			if next := m.nextAvailable(account.ID, providerID); next != "" {
				m.setActive(providerID, next)
				continue // transparent retry on the new account
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
func (m *Manager) nextAvailable(exclude, providerID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
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
	if err := m.persistLocked(); err != nil {
		log.Printf("proxy: persist active account: %v", err)
	}
	log.Printf("proxy: switched active %s account to %s", providerID, id)
}

func (m *Manager) markExhausted(id string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exhausted[id] = until
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
	for providerID, activeID := range m.active {
		if activeID == id {
			if next := m.nextAvailable(id, providerID); next != "" {
				m.active[providerID] = next
			} else {
				delete(m.active, providerID)
			}
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
		if isHopByHop(key) || key == "Set-Cookie" {
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
