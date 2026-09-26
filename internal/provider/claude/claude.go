// Package claude implements the provider.Provider interface for Claude
// (Claude Pro/Max subscription logins). It performs the same OAuth PKCE
// flow the Claude Code CLI uses and forwards requests to the Anthropic API.
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Public OAuth client of the Claude Code CLI.
const (
	authURL      = "https://claude.ai/oauth/authorize"
	tokenURL     = "https://platform.claude.com/v1/oauth/token"
	refreshURL   = "https://platform.claude.com/v1/oauth/token"
	upstreamBase = "https://api.anthropic.com"
	scope        = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	clientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	redirectURI  = "http://localhost:54545/callback"
	oauthBeta    = "oauth-2025-04-20"
)

// Provider implements provider.Provider for Claude.
type Provider struct {
	mu       sync.Mutex
	verifier map[string]verifierEntry
}

type verifierEntry struct {
	value     string
	startedAt time.Time
}

// verifierTTL bounds how long PKCE verifiers for abandoned logins linger.
const verifierTTL = 10 * time.Minute

// New returns a ready-to-register Claude provider.
func New() *Provider {
	return &Provider{verifier: map[string]verifierEntry{}}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return "claude" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Claude (Anthropic)" }

// LoginStart builds the OAuth authorization URL the user must open. The
// redirect URI is fixed by Anthropic's client registration and uses port
// 54545, so Switcher runs a callback listener on that port too.
func (p *Provider) LoginStart(_ context.Context) (provider.LoginInfo, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return provider.LoginInfo{}, fmt.Errorf("generate pkce verifier: %w", err)
	}
	// Anthropic's authorize endpoint rejects a random state nonce with
	// "Invalid request format": the state must be the PKCE verifier (the
	// Claude Code CLI does the same), so we key the verifier map by the
	// verifier itself.
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	p.mu.Lock()
	p.gcVerifiersLocked()
	p.verifier[verifier] = verifierEntry{value: verifier, startedAt: time.Now()}
	p.mu.Unlock()

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scope)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", verifier)
	return provider.LoginInfo{Kind: "browser", URL: authURLPath() + "?" + q.Encode(), State: verifier}, nil
}

func authURLPath() string {
	return "https://claude.ai/oauth/authorize"
}

// LoginExchange swaps the authorization code for tokens. The callback code
// may carry a "#state" fragment that takes precedence over the query state.
func (p *Provider) LoginExchange(ctx context.Context, state, code string) (store.Account, error) {
	p.mu.Lock()
	entry, ok := p.verifier[state]
	if ok {
		delete(p.verifier, state)
	}
	p.mu.Unlock()
	if !ok {
		return store.Account{}, errors.New("unknown or expired login state")
	}

	// Claude Code appends the OAuth state to the code after a '#'; honor it.
	exchangedState := state
	if i := strings.Index(code, "#"); i >= 0 {
		if fragment := strings.TrimSpace(code[i+1:]); fragment != "" {
			exchangedState = fragment
		}
		code = code[:i]
	}

	reqBody, err := json.Marshal(struct {
		GrantType    string `json:"grant_type"`
		Code         string `json:"code"`
		RedirectURI  string `json:"redirect_uri"`
		ClientID     string `json:"client_id"`
		CodeVerifier string `json:"code_verifier"`
		State        string `json:"state"`
	}{
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  redirectURI,
		ClientID:     clientID,
		CodeVerifier: entry.value,
		State:        exchangedState,
	})
	if err != nil {
		return store.Account{}, fmt.Errorf("encode token request: %w", err)
	}

	tok, err := tokenRequest(ctx, tokenURL, reqBody)
	if err != nil {
		return store.Account{}, err
	}
	return accountFromToken(tok)
}

// Refresh renews the account's tokens with a JSON refresh request.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	if a.Token.RefreshToken == "" {
		return fmt.Errorf("claude refresh: missing refresh token: %w", provider.ErrReloginRequired)
	}
	reqBody, err := json.Marshal(map[string]any{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": a.Token.RefreshToken,
		"scope":         scope,
	})
	if err != nil {
		return fmt.Errorf("encode refresh request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-beta", oauthBeta)
	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if provider.RefreshCredentialRejected(resp.StatusCode, raw) {
			return fmt.Errorf("refresh failed: http %d: %w", resp.StatusCode, provider.ErrReloginRequired)
		}
		return fmt.Errorf("refresh failed: http %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("refresh response: %w", err)
	}
	if tok.AccessToken != "" {
		a.Token.AccessToken = tok.AccessToken
	}
	if tok.RefreshToken != "" {
		a.Token.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		a.Token.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a.LastRefresh = time.Now().Unix()
	return nil
}

// DeviceStart implements provider.Provider: claude has no device flow.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}

// AddByKey implements provider.Provider: claude accounts come from OAuth.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// UpstreamURL maps a Switcher path (e.g. "/v1/messages") to the Anthropic API.
func (p *Provider) UpstreamURL(path string) string {
	return "https://api.anthropic.com" + strings.TrimPrefix(path, "/v1")
}

// ApplyAuth sets the headers Claude's API expects for OAuth requests.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	req.Header.Set("anthropic-beta", oauthBeta)
	return nil
}

// IsExpired implements provider.Provider.
func (p *Provider) IsExpired(a store.Account) bool {
	return a.Token.ExpiresAt > 0 && time.Now().Unix() >= a.Token.ExpiresAt-60
}

// Usage queries Anthropic's OAuth usage endpoint, which reports the five
// hour and seven day windows plus per-model weekly scopes.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.Usage{}, provider.UsageStatusError(resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}

	var parsed struct {
		FiveHour *claudeWindow `json:"five_hour"`
		SevenDay *claudeWindow `json:"seven_day"`
		Limits   []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt *string  `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}

	var windows []provider.UsageWindow
	if w := windowOf("Session", parsed.FiveHour); w != nil {
		windows = append(windows, *w)
	}
	if w := windowOf("Weekly", parsed.SevenDay); w != nil {
		windows = append(windows, *w)
	}
	for _, l := range parsed.Limits {
		if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil || l.Percent == nil {
			continue
		}
		windows = append(windows, provider.UsageWindow{
			Label:       l.Scope.Model.DisplayName,
			UsedPercent: int(*l.Percent),
			ResetsAt:    parseTimeOrZero(l.ResetsAt),
		})
	}
	return provider.Usage{Available: true, Windows: windows}, nil
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func windowOf(label string, w *claudeWindow) *provider.UsageWindow {
	if w == nil || w.Utilization == nil {
		return nil
	}
	return &provider.UsageWindow{
		Label:       label,
		UsedPercent: int(*w.Utilization),
		ResetsAt:    parseTimeOrZero(w.ResetsAt),
	}
}

func parseTimeOrZero(s *string) int64 {
	if s == nil || *s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05.999Z"} {
		if t, err := time.Parse(layout, *s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// ParseRateLimit implements provider.Provider. Claude reports subscription
// exhaustion as a 429 on /v1/messages; the reset time comes from the usage
// endpoint, falling back to a one hour park.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	if status != http.StatusTooManyRequests {
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

// oauthToken mirrors the token endpoint response, which embeds the account
// identity directly.
type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"organization"`
	Account struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
}

func tokenRequest(ctx context.Context, target string, body []byte) (oauthToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return oauthToken{}, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", oauthBeta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return oauthToken{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return oauthToken{}, fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return oauthToken{}, fmt.Errorf("token request failed: http %d", resp.StatusCode)
	}
	var tok oauthToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return oauthToken{}, fmt.Errorf("token response: %w", err)
	}
	if tok.AccessToken == "" {
		return oauthToken{}, errors.New("token response missing access_token")
	}
	return tok, nil
}

// accountFromToken builds the stored account from a token exchange.
func accountFromToken(tok oauthToken) (store.Account, error) {
	acc := store.Account{
		Provider:  "claude",
		Email:     tok.Account.EmailAddress,
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			IDToken:      "",
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			AccountID:    tok.Account.UUID,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		},
	}
	if acc.Email == "" {
		return store.Account{}, errors.New("token response has no account email")
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + acc.Email + "|" + tok.Account.UUID))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

// oauthHTTPClient bounds token and refresh exchanges so a stalled upstream
// cannot hold the callback request open.
var oauthHTTPClient = &http.Client{Timeout: 30 * time.Second}

// keychainCreds mirrors the JSON the Claude Code CLI stores in the macOS
// keychain (service "Claude Code-credentials").
type keychainCreds struct {
	McpOAuth      map[string]any `json:"mcpOAuth"`
	ClaudeAiOauth struct {
		AccessToken      string   `json:"accessToken"`
		RefreshToken     string   `json:"refreshToken"`
		ExpiresAt        int64    `json:"expiresAt"` // unix ms
		SubscriptionType string   `json:"subscriptionType"`
		Scopes           []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

// profile mirrors api.anthropic.com/api/oauth/profile.
type claudeProfile struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		Name string `json:"name"`
	} `json:"organization"`
	HasClaudeMax bool `json:"has_claude_max"`
	HasClaudePro bool `json:"has_claude_pro"`
}

// ImportFromKeychain implements the optional importer interface: it lifts
// the tokens the Claude Code CLI already stores in the macOS keychain, so a
// machine that signed in to Claude Code gets a Switcher account with no
// browser flow at all (the same trick T3 Code plays). The first import may
// prompt the user to approve keychain access for the `security` tool.
func (p *Provider) ImportFromKeychain(ctx context.Context) (store.Account, error) {
	out, err := exec.CommandContext(ctx, "security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return store.Account{}, fmt.Errorf("no Claude Code CLI credentials in the keychain")
	}
	var creds keychainCreds
	if err := json.Unmarshal(bytes.TrimSpace(out), &creds); err != nil {
		return store.Account{}, fmt.Errorf("decode keychain credentials: %w", err)
	}
	oauth := creds.ClaudeAiOauth
	if oauth.AccessToken == "" || oauth.RefreshToken == "" {
		return store.Account{}, errors.New("keychain credentials have no OAuth token")
	}

	acc := store.Account{
		Provider:  "claude",
		Plan:      oauth.SubscriptionType,
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			AccessToken:  oauth.AccessToken,
			RefreshToken: oauth.RefreshToken,
			ExpiresAt:    oauth.ExpiresAt / 1000,
		},
	}
	// Expired or about to: refresh right away so the account is usable.
	if time.Now().Add(2*time.Minute).Unix() >= acc.Token.ExpiresAt {
		if err := p.Refresh(ctx, &acc); err != nil {
			return store.Account{}, fmt.Errorf("refresh expired CLI token: %w", err)
		}
	}

	profile, err := p.fetchProfile(ctx, acc.Token.AccessToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("claude profile: %w", err)
	}
	acc.Email = profile.Account.Email
	acc.Token.AccountID = profile.Account.UUID
	if profile.HasClaudeMax {
		acc.Plan = "max"
	} else if profile.HasClaudePro {
		acc.Plan = "pro"
	}
	if acc.Email == "" {
		return store.Account{}, errors.New("claude profile has no email")
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + acc.Email + "|" + profile.Account.UUID))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

func (p *Provider) fetchProfile(ctx context.Context, accessToken string) (claudeProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		return claudeProfile{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", oauthBeta)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return claudeProfile{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return claudeProfile{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return claudeProfile{}, fmt.Errorf("profile http %d", resp.StatusCode)
	}
	var profile claudeProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return claudeProfile{}, err
	}
	return profile, nil
}

// gcVerifiersLocked drops verifiers for abandoned logins.
func (p *Provider) gcVerifiersLocked() {
	now := time.Now()
	for state, entry := range p.verifier {
		if now.Sub(entry.startedAt) > verifierTTL {
			delete(p.verifier, state)
		}
	}
}
