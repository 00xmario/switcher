// Package copilot implements the provider.Provider interface for GitHub
// Copilot (Copilot Pro/Business/Enterprise subscription logins). Login uses
// the GitHub OAuth device flow; requests forward to the Copilot API with a
// short-lived Copilot token minted on demand from the GitHub OAuth token.
package copilot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Public OAuth client of the GitHub Copilot extension and the endpoints the
// device flow and the token chain use. They are vars so tests can point
// them at a stub server.
var (
	deviceCodeURL   = "https://github.com/login/device/code"
	tokenURL        = "https://github.com/login/oauth/access_token"
	userURL         = "https://api.github.com/user"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	copilotUserURL  = "https://api.github.com/copilot_internal/user"
	upstreamBase    = "https://api.githubcopilot.com"
)

// deviceGrant is the OAuth device authorization grant type, apiVersion the
// GitHub API version Copilot clients pin.
const (
	deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	apiVersion  = "2022-11-28"
	// clientID is the public OAuth client of the GitHub Copilot extension.
	clientID = "Iv1.b507a08c87ecfe98"
)

// copilotTokenTTL is assumed when the token response omits expires_at.
const copilotTokenTTL = 25 * time.Minute

// extraCopilotExpires keys the Copilot token expiry inside Token.Extra.
const extraCopilotExpires = "copilot_expires"

// minPollInterval and slowDownPenalty shape the device poll cadence; they
// are vars so tests can shrink them.
var (
	minPollInterval = 5 * time.Second
	slowDownPenalty = 5 * time.Second
)

// appsJSONPath returns the path of the GitHub Copilot CLI's stored app
// tokens (a package var so tests can point it at a fixture).
var appsJSONPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "github-copilot", "apps.json")
}()

// Provider implements provider.Provider for GitHub Copilot.
type Provider struct{}

// New returns a ready-to-register Copilot provider.
func New() *Provider { return &Provider{} }

// ID implements provider.Provider.
func (p *Provider) ID() string { return "copilot" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Copilot (GitHub)" }

// LoginStart implements provider.Provider: copilot uses the device flow.
func (p *Provider) LoginStart(ctx context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{}, provider.ErrUnsupported
}

// LoginExchange implements provider.Provider: no browser callback exists.
func (p *Provider) LoginExchange(ctx context.Context, state, code string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// DeviceStart begins the GitHub device authorization grant: request a
// device code, show the user the verification URL and code, and return a
// poll function that resolves once GitHub issues the OAuth token. The
// poller stores the GitHub OAuth identity; Copilot tokens are minted on use.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	form := url.Values{
		"client_id": {clientID},
		"scope":     {"read:user"},
	}
	body, err := postForm(ctx, deviceCodeURL, form)
	if err != nil {
		return provider.LoginInfo{}, nil, err
	}
	var device struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int64  `json:"interval"`
		// expires_in is ignored: the poll deadline is the fixed 30 minute
		// window in pollForToken, which is well under GitHub's own expiry.
	}
	if err := json.Unmarshal(body, &device); err != nil {
		return provider.LoginInfo{}, nil, fmt.Errorf("device code response: %w", err)
	}
	if device.DeviceCode == "" || device.VerificationURI == "" {
		return provider.LoginInfo{}, nil, errors.New("device code response incomplete")
	}

	info := provider.LoginInfo{
		Kind:            "device",
		VerificationURL: device.VerificationURI,
		UserCode:        device.UserCode,
		State:           device.DeviceCode,
	}
	poll := func(ctx context.Context) (store.Account, error) {
		return p.pollForToken(ctx, device.DeviceCode, device.Interval)
	}
	return info, poll, nil
}

// pollForToken polls GitHub with the device code until the user authorizes.
// slow_down grows the interval by 5s each time (the adaptive behaviour the
// OAuth spec asks for); pending keeps the cadence.
func (p *Provider) pollForToken(ctx context.Context, deviceCode string, interval int64) (store.Account, error) {
	intervalDur := time.Duration(interval) * time.Second
	if intervalDur < minPollInterval {
		intervalDur = minPollInterval
	}
	deadline := time.Now().Add(30 * time.Minute)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return store.Account{}, ctx.Err()
		case <-time.After(intervalDur):
		}

		form := url.Values{
			"client_id":   {clientID},
			"device_code": {deviceCode},
			"grant_type":  {deviceGrant},
		}
		body, err := postForm(ctx, tokenURL, form)
		if err != nil {
			return store.Account{}, err
		}
		var payload struct {
			Error        string `json:"error"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return store.Account{}, fmt.Errorf("device token response: %w", err)
		}
		switch {
		case payload.AccessToken != "":
			return p.accountFromGithubToken(ctx, payload.AccessToken)
		case payload.Error == "authorization_pending":
			continue
		case payload.Error == "slow_down":
			intervalDur += slowDownPenalty
		case payload.Error == "expired_token" || payload.Error == "access_denied":
			return store.Account{}, fmt.Errorf("device flow failed: %s", payload.Error)
		case payload.Error != "":
			return store.Account{}, fmt.Errorf("device flow failed: %s", payload.Error)
		default:
			return store.Account{}, errors.New("device token poll: unexpected response")
		}
	}
	return store.Account{}, errors.New("device flow timed out")
}

// accountFromGithubToken resolves the GitHub login independently of the
// Copilot token endpoint. A temporary mint failure must not discard a
// successfully authorized account.
func (p *Provider) accountFromGithubToken(ctx context.Context, githubToken string) (store.Account, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userURL, nil)
	if err != nil {
		return store.Account{}, fmt.Errorf("github user: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return store.Account{}, fmt.Errorf("github user: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return store.Account{}, fmt.Errorf("github user: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return store.Account{}, fmt.Errorf("github user failed: http %d", resp.StatusCode)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &user); err != nil {
		return store.Account{}, fmt.Errorf("github user: %w", err)
	}
	if user.Login == "" {
		return store.Account{}, errors.New("github user response has no login")
	}

	acc := store.Account{
		Provider:  "copilot",
		Email:     user.Login + "@copilot",
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			RefreshToken: githubToken,
			AccountID:    user.Login,
		},
	}
	// Plan is prefixed ("copilot_pro") so it cannot collide with the plan
	// values other providers store; the UI maps it to a label.
	if u, uerr := fetchCopilotUsage(ctx, githubToken); uerr == nil && u.CopilotPlan != "" {
		acc.Plan = "copilot_" + u.CopilotPlan
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + user.Login + "|" + user.Login))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

// copilotToken is the short-lived API token GitHub mints for Copilot
// clients from a GitHub OAuth token. expires_at arrives as unix seconds or
// an RFC3339 string depending on deployment.
type copilotToken struct {
	Token     string      `json:"token"`
	ExpiresAt unixSeconds `json:"expires_at"`
}

type tokenMintHTTPError struct{ status int }

func (e tokenMintHTTPError) Error() string {
	return fmt.Sprintf("copilot token request failed: http %d", e.status)
}

// unixSeconds decodes a unix timestamp that may arrive as a JSON number or
// an RFC3339 string.
type unixSeconds int64

// UnmarshalJSON implements json.Unmarshaler.
func (u *unixSeconds) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		*u = 0
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			*u = unixSeconds(t.Unix())
			return nil
		}
		*u = 0
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*u = unixSeconds(n)
	return nil
}

// fetchCopilotToken exchanges a GitHub OAuth token for a Copilot API token.
func fetchCopilotToken(ctx context.Context, githubToken string) (copilotToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return copilotToken{}, fmt.Errorf("copilot token: %w", err)
	}
	// Match GitHub's Copilot token exchange. The device OAuth token itself
	// works for /user, but the mint endpoint expects the GitHub token scheme.
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("X-GitHub-Api-Version", "2025-04-01")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Switcher/0.5")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return copilotToken{}, fmt.Errorf("copilot token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return copilotToken{}, fmt.Errorf("copilot token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return copilotToken{}, tokenMintHTTPError{status: resp.StatusCode}
	}
	var ct copilotToken
	if err := json.Unmarshal(raw, &ct); err != nil {
		return copilotToken{}, fmt.Errorf("copilot token response: %w", err)
	}
	if ct.Token == "" {
		return copilotToken{}, errors.New("copilot token response missing token")
	}
	if ct.ExpiresAt == 0 {
		ct.ExpiresAt = unixSeconds(time.Now().Add(copilotTokenTTL).Unix())
	}
	return ct, nil
}

// copilotExpiry returns the stored Copilot token expiry (unix seconds).
func copilotExpiry(a store.Account) int64 {
	switch v := a.Token.Extra[extraCopilotExpires].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return a.Token.ExpiresAt
}

// AddByKey implements provider.Provider: copilot accounts come from OAuth.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// Refresh re-mints the short-lived Copilot API token from the stored
// GitHub OAuth token. A 401 from the token mint confirms rejection of the
// stored GitHub credential; a 403 may instead mean no Copilot plan.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	githubToken := a.Token.RefreshToken
	if githubToken == "" {
		return fmt.Errorf("copilot refresh: missing GitHub token: %w", provider.ErrReloginRequired)
	}
	ct, err := fetchCopilotToken(ctx, githubToken)
	if err != nil {
		var statusErr tokenMintHTTPError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusUnauthorized {
			return fmt.Errorf("copilot refresh: %w: %w", err, provider.ErrReloginRequired)
		}
		return err
	}
	a.Token.AccessToken = ct.Token
	a.Token.ExpiresAt = int64(ct.ExpiresAt)
	if a.Token.Extra == nil {
		a.Token.Extra = map[string]any{}
	}
	a.Token.Extra[extraCopilotExpires] = int64(ct.ExpiresAt)
	a.LastRefresh = time.Now().Unix()
	return nil
}

// UpstreamURL maps a Switcher path to the Copilot API.
func (p *Provider) UpstreamURL(path string) string {
	return upstreamBase + path
}

// ApplyAuth sets the headers the Copilot API expects from an editor client.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "GithubCopilot/1.0")
	req.Header.Set("Editor-Version", "vscode/1.100.0")
	req.Header.Set("Editor-Plugin-Version", "copilot/1.300.0")
	req.Header.Set("Copilot-Integration-Id", "vscode-vscode")
	return nil
}

// IsExpired implements provider.Provider: refresh 5 minutes before the
// Copilot token dies.
func (p *Provider) IsExpired(a store.Account) bool {
	if a.Token.AccessToken == "" {
		return true
	}
	expiry := copilotExpiry(a)
	return expiry > 0 && time.Now().Unix() >= expiry-5*60
}

// copilotUsage mirrors copilot_internal/user.
type copilotUsage struct {
	CopilotPlan    string `json:"copilot_plan"`
	AccessTypeSKU  string `json:"access_type_sku"`
	QuotaResetDate string `json:"quota_reset_date"`
	QuotaSnapshots map[string]struct {
		Entitlement *float64 `json:"entitlement"`
		Unlimited   bool     `json:"unlimited"`
		Used        *float64 `json:"used"`
		Entitled    *float64 `json:"entitled"`
	} `json:"quota_snapshots"`
}

// snapshotLabels name the windows the UI shows, in display order.
var snapshotLabels = []struct {
	key   string
	label string
}{
	{"chat", "Copilot Chat"},
	{"completions", "Completions"},
	{"premium_interactions", "Premium Requests"},
}

// Usage queries the Copilot internal usage endpoint with the long-lived
// GitHub token.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	githubToken := a.Token.RefreshToken
	if githubToken == "" {
		return provider.Usage{}, fmt.Errorf("%w: no GitHub token", provider.ErrUsageUnavailable)
	}
	parsed, err := fetchCopilotUsage(ctx, githubToken)
	if err != nil {
		return provider.Usage{}, err
	}

	windows := windowsOf(parsed)
	if len(windows) == 0 {
		return provider.Usage{}, fmt.Errorf("%w: no quota data", provider.ErrUsageUnavailable)
	}
	return provider.Usage{Available: true, Windows: windows}, nil
}

// fetchCopilotUsage reads the copilot_internal/user snapshot.
func fetchCopilotUsage(ctx context.Context, githubToken string) (copilotUsage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotUserURL, nil)
	if err != nil {
		return copilotUsage{}, provider.ErrUsageUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Accept", "application/json")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return copilotUsage{}, provider.ErrUsageUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return copilotUsage{}, provider.UsageStatusError(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return copilotUsage{}, provider.ErrUsageUnavailable
	}
	var parsed copilotUsage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return copilotUsage{}, provider.ErrUsageUnavailable
	}
	return parsed, nil
}

// windowsOf maps quota snapshots to usage windows. A finite entitlement
// with a used value yields a percent; unlimited snapshots show 0% used.
func windowsOf(u copilotUsage) []provider.UsageWindow {
	var windows []provider.UsageWindow
	for _, s := range snapshotLabels {
		snap, ok := u.QuotaSnapshots[s.key]
		if !ok {
			continue
		}
		w := provider.UsageWindow{Label: s.label}
		if snap.Unlimited {
			windows = append(windows, w)
			continue
		}
		if snap.Entitlement == nil || *snap.Entitlement <= 0 {
			continue
		}
		if snap.Used != nil {
			pct := int(*snap.Used / *snap.Entitlement * 100)
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			w.UsedPercent = pct
		} else if snap.Entitled != nil && *snap.Entitled > 0 {
			pct := int(*snap.Entitled / *snap.Entitlement * 100)
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			w.UsedPercent = pct
		}
		if t, err := time.ParseInLocation("2006-01-02", u.QuotaResetDate, time.Local); err == nil {
			w.ResetsAt = t.Unix()
		}
		windows = append(windows, w)
	}
	return windows
}

// ParseRateLimit implements provider.Provider. The Copilot API reports
// exhaustion as a 429 with a premium_request_exceeded body; the usage
// snapshot decides until when.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	if status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	if !strings.Contains(string(body), "rate limit") && !strings.Contains(string(body), "premium") &&
		!strings.Contains(string(body), "exceeded") {
		return time.Time{}, false
	}
	usage, err := p.Usage(ctx, a)
	if err != nil {
		return time.Now().Add(time.Hour), true
	}
	var latest time.Time
	for _, w := range usage.Windows {
		if w.UsedPercent >= 100 && w.ResetsAt > 0 {
			if t := time.Unix(w.ResetsAt, 0); t.After(latest) {
				latest = t
			}
		}
	}
	if latest.IsZero() {
		return time.Now().Add(time.Hour), true
	}
	return latest, true
}

// ImportFromKeychain implements the optional importer interface: it lifts
// the GitHub OAuth token the GitHub Copilot CLI already stores in
// ~/Library/Application Support/github-copilot/apps.json, so a machine
// signed in to the CLI gets a Switcher account with no device flow.
func (p *Provider) ImportFromKeychain(ctx context.Context) (store.Account, error) {
	raw, err := os.ReadFile(appsJSONPath)
	if err != nil {
		return store.Account{}, errors.New("no GitHub Copilot CLI credentials on disk")
	}
	var apps map[string]struct {
		OAuthToken string `json:"oauth_token"`
	}
	if err := json.Unmarshal(raw, &apps); err != nil {
		return store.Account{}, fmt.Errorf("decode Copilot CLI credentials: %w", err)
	}
	var githubToken string
	for _, app := range apps {
		if app.OAuthToken != "" {
			githubToken = app.OAuthToken
			break
		}
	}
	if githubToken == "" {
		return store.Account{}, errors.New("Copilot CLI credentials have no OAuth token")
	}
	return p.accountFromGithubToken(ctx, githubToken)
}

// postForm posts a form and returns the body for a 200 response.
func postForm(ctx context.Context, target string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("copilot request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("copilot response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot request failed: http %d", resp.StatusCode)
	}
	return raw, nil
}
