// Package opencode implements the provider.Provider interface for OpenCode
// (the OpenCode Go plan, authenticated with an API key). Login is manual:
// the user pastes a key from opencode.ai, and usage queries the Zen usage
// endpoint.
package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Upstream is the OpenCode Zen Go endpoint.
const upstreamBase = "https://opencode.ai/zen/go/v1"

// Provider implements provider.Provider for OpenCode.
type Provider struct{}

// New returns a ready-to-register OpenCode provider.
func New() *Provider { return &Provider{} }

// ID implements provider.Provider.
func (p *Provider) ID() string { return "opencode" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "OpenCode" }

// LoginStart implements provider.Provider: opencode has no OAuth login.
func (p *Provider) LoginStart(ctx context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{}, provider.ErrUnsupported
}

// LoginExchange implements provider.Provider: no browser callback exists.
func (p *Provider) LoginExchange(ctx context.Context, state, code string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// DeviceStart implements provider.Provider: no device flow exists.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}

// AddByKey validates an OpenCode Go API key against the usage endpoint and
// stores the resulting account.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	snap, err := probeKey(ctx, key)
	if err != nil {
		return store.Account{}, err
	}
	acc := store.Account{
		Provider:  "opencode",
		Email:     snap.Email,
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			AccessToken: key,
			ExpiresAt:   0, // API keys do not expire
		},
	}
	if acc.Email == "" {
		acc.Email = "OpenCode key " + tail(key)
	}
	acc.Plan = snap.Plan
	sum := sha256.Sum256([]byte(acc.Provider + "|" + key))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

// tail masks most of the key for display purposes.
func tail(key string) string {
	if len(key) <= 10 {
		return "…"
	}
	return key[:7] + "…" + key[len(key)-4:]
}

// Refresh implements provider.Provider: API keys never expire.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	return nil
}

// UpstreamURL maps a Switcher path to the Zen Go endpoint.
func (p *Provider) UpstreamURL(path string) string {
	return upstreamBase + path
}

// ApplyAuth sets the Bearer authentication the Zen API expects.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	return nil
}

// IsExpired implements provider.Provider: API keys never expire.
func (p *Provider) IsExpired(a store.Account) bool { return false }

// usageWindow is the upstream shape of one Zen quota window.
type usageWindow struct {
	Status   string `json:"status"`
	Percent  int    `json:"percent"`
	ResetsAt string `json:"resetsAt"`
}

// usageSnapshot is the decoded upstream usage payload.
type usageSnapshot struct {
	Plan    string      `json:"plan_type"`
	Email   string      `json:"email"`
	Rolling usageWindow `json:"rolling"`
	Weekly  usageWindow `json:"weekly"`
	Monthly usageWindow `json:"monthly"`
}

// probeKey validates a key and returns its usage snapshot.
func probeKey(ctx context.Context, key string) (usageSnapshot, error) {
	return fetchUsage(ctx, key)
}

// fetchUsage queries the Zen usage endpoint.
func fetchUsage(ctx context.Context, key string) (usageSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamBase+"/usage", nil)
	if err != nil {
		return usageSnapshot{}, provider.ErrUsageUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return usageSnapshot{}, provider.ErrUsageUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return usageSnapshot{}, provider.ErrUsageUnavailable
	}
	var snap struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
		Usage    struct {
			Rolling usageWindow `json:"rolling"`
			Weekly  usageWindow `json:"weekly"`
			Monthly usageWindow `json:"monthly"`
		} `json:"usage"`
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil || json.Unmarshal(raw, &snap) != nil {
		return usageSnapshot{}, provider.ErrUsageUnavailable
	}
	return usageSnapshot{
		Plan:    snap.PlanType,
		Email:   snap.Email,
		Rolling: snap.Usage.Rolling,
		Weekly:  snap.Usage.Weekly,
		Monthly: snap.Usage.Monthly,
	}, nil
}

// Usage implements provider.Provider. The three Zen windows map to
// Go Session (rolling), Go Weekly, and Go Monthly, matching the upstream
// labels. Absent windows are skipped.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	snap, err := fetchUsage(ctx, a.Token.AccessToken)
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	windows := make([]provider.UsageWindow, 0, 3)
	present := func(label string, w usageWindow) {
		if w.ResetsAt == "" && w.Percent == 0 && w.Status == "" {
			return // the upstream omits windows without a subscription
		}
		windows = append(windows, provider.UsageWindow{
			Label:       label,
			UsedPercent: w.Percent,
			ResetsAt:    parseISO(w.ResetsAt),
		})
	}
	present("Go · Session", snap.Rolling)
	present("Go · Weekly", snap.Weekly)
	present("Go · Monthly", snap.Monthly)
	return provider.Usage{Available: true, Windows: windows}, nil
}

// ParseRateLimit implements provider.Provider. OpenCode reports
// exhaustion through the usage windows; any 429 parks the account until
// the earliest still-limited window resets.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	if status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	snap, err := fetchUsage(ctx, a.Token.AccessToken)
	if err != nil {
		return time.Now().Add(time.Hour), true
	}
	var latest int64
	for _, w := range []usageWindow{snap.Rolling, snap.Weekly, snap.Monthly} {
		if w.Percent >= 100 {
			if t := parseISO(w.ResetsAt); t > latest {
				latest = t
			}
		}
	}
	if latest == 0 {
		latest = time.Now().Add(time.Hour).Unix()
	}
	return time.Unix(latest, 0), true
}

// parseISO parses the Zen reset timestamps (RFC3339 or epoch seconds).
func parseISO(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}
