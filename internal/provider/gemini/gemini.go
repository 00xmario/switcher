// Package gemini implements the provider.Provider interface for the Gemini
// CLI (Google subscription logins). Login uses a Google OAuth code flow
// with the Gemini CLI client's own credentials, and requests forward to the
// Cloud Code backend the CLI posts generateContent to.
package gemini

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"switcher/internal/config"
	"switcher/internal/googleauth"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// Public OAuth client of the Gemini CLI (a Google confidential web client)
// and the Cloud Code endpoints it talks to.
const (
	ClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	ClientSecret = "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"
	CallbackPort = config.GeminiCallbackPort
	Scopes       = "https://www.googleapis.com/auth/cloud-platform openid email profile"
	authorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	version      = "v1internal"
	apiClient    = "gl-node/22.21.1"
	// clientVersion is reported in the User-Agent. Pinned: fetching it
	// live would require network at startup.
	clientVersion = "1.0.0"
	userAgent     = "GeminiCLI/v" + clientVersion + " (darwin; arm64)"
	// The onboardUser fallback talks to the daily Cloud Code endpoint as
	// the Antigravity client, so it reuses that client's User-Agent.
	onboardUserAgent = "antigravity/hub/2.9.1 darwin/arm64"
)

// redirectURI is derived from CallbackPort so the port registered with
// Google has one source of truth.
var redirectURI = fmt.Sprintf("http://localhost:%d/oauth2callback", config.GeminiCallbackPort)

// apiBase and onboardBase are vars so tests can point them at a stub
// server.
var (
	apiBase     = "https://cloudcode-pa.googleapis.com"
	onboardBase = "https://daily-cloudcode-pa.googleapis.com"
)

// stateTTL bounds how long login states for abandoned logins linger.
const stateTTL = 10 * time.Minute

// Provider implements provider.Provider for the Gemini CLI.
type Provider struct {
	mu    sync.Mutex
	state map[string]time.Time
}

// New returns a ready-to-register Gemini provider.
func New() *Provider {
	return &Provider{state: map[string]time.Time{}}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return "gemini" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Gemini (Google)" }

// LoginStart builds the Google authorization URL the user must open. The
// redirect URI is fixed by the client registration (port 51122), so
// Switcher runs a callback listener on that port too. No PKCE: this client
// is confidential and Google echoes the state back in the query, so the
// state itself is the single-use marker.
func (p *Provider) LoginStart(_ context.Context) (provider.LoginInfo, error) {
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return provider.LoginInfo{}, fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	p.mu.Lock()
	p.gcStatesLocked()
	p.state[state] = time.Now()
	p.mu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scopes)
	q.Set("state", state)
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")
	return provider.LoginInfo{Kind: "browser", URL: authorizeURL + "?" + q.Encode(), State: state}, nil
}

// LoginExchange swaps the authorization code for tokens, resolves the
// account email, and discovers the Cloud Code project id.
func (p *Provider) LoginExchange(ctx context.Context, state, code string) (store.Account, error) {
	p.mu.Lock()
	p.gcStatesLocked()
	started, ok := p.state[state]
	delete(p.state, state)
	p.mu.Unlock()
	if !ok {
		return store.Account{}, errors.New("unknown or expired login state")
	}
	if time.Since(started) > stateTTL {
		return store.Account{}, errors.New("login state expired; start over")
	}

	tok, err := googleauth.TokenExchange(ctx, ClientSecret, code, ClientID, redirectURI)
	if err != nil {
		return store.Account{}, err
	}
	email, err := googleauth.Userinfo(ctx, tok.AccessToken)
	if err != nil {
		return store.Account{}, err
	}
	projectID, err := p.discoverProject(ctx, tok.AccessToken)
	if err != nil {
		// Onboarding failed, but the tokens are valid: keep the account
		// with no project id instead of discarding it. Usage reports
		// ErrUsageUnavailable until a successful relogin discovers the
		// project.
		log.Printf("gemini: project discovery failed for %s: %v", email, err)
		return accountFromToken(tok, email, ""), nil
	}
	return accountFromToken(tok, email, projectID), nil
}

// discoverProject resolves the Cloud Code project: loadCodeAssist first,
// then an onboarding round trip (polled) when no project exists yet.
func (p *Provider) discoverProject(ctx context.Context, accessToken string) (string, error) {
	var load struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
		ProjectID               string `json:"projectId"`
		Project                 string `json:"project"`
	}
	if err := p.cloudcodePost(ctx, accessToken, version+":loadCodeAssist",
		[]byte(`{"metadata":{"ideType":"IDE_UNSPECIFIED"}}`), &load); err != nil {
		return "", err
	}
	if id := projectIDOf(load.CloudaicompanionProject, load.ProjectID, load.Project); id != "" {
		return id, nil
	}

	// No project yet: onboard a free-tier Gemini project and poll until
	// the long-running operation reports the companion project. Same
	// flow as antigravity, shared through googleauth.
	id, err := googleauth.OnboardUser(ctx, accessToken, onboardBase, onboardUserAgent, apiClient,
		[]byte(`{"tier_id":"gemini-free","metadata":{"ide_type":"IDE_UNSPECIFIED","ide_name":"gemini","ide_version":"1.0.0"}}`))
	if err != nil {
		return "", err
	}
	return projectIDOf(id), nil
}

// cloudcodePost posts one authenticated Cloud Code JSON request.
func (p *Provider) cloudcodePost(ctx context.Context, accessToken, method string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gemini %s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Client", apiClient)
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("gemini %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("gemini %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gemini %s failed: http %d", method, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gemini %s response: %w", method, err)
	}
	return nil
}

// projectIDOf takes the first non-empty value and strips a "projects/<id>"
// path down to its last segment.
func projectIDOf(values ...string) string {
	for _, v := range values {
		if v == "" {
			continue
		}
		if i := strings.LastIndex(v, "/"); i >= 0 {
			v = v[i+1:]
		}
		return v
	}
	return ""
}

// DeviceStart implements provider.Provider: gemini has no device flow.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}

// AddByKey implements provider.Provider: gemini accounts come from OAuth.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// accountFromToken builds the stored account from a token exchange.
func accountFromToken(tok googleauth.Token, email, projectID string) store.Account {
	acc := store.Account{
		Provider:  "gemini",
		Email:     email,
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			ExpiresAt:    tok.ExpiresAt(),
			Extra:        map[string]any{"project_id": projectID},
		},
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + email + "|" + projectID))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc
}

// ProjectID returns the Cloud Code project id stored in the account.
func ProjectID(a store.Account) string {
	if a.Token.Extra == nil {
		return ""
	}
	id, _ := a.Token.Extra["project_id"].(string)
	return id
}

// Refresh renews the account's tokens with the Google token endpoint. The
// request keeps Go's default User-Agent: Google rejects impersonated
// clients on the refresh grant.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	if a.Token.RefreshToken == "" {
		return errors.New("no refresh token; sign in again")
	}
	tok, err := googleauth.RefreshToken(ctx, ClientSecret, a.Token.RefreshToken, ClientID)
	if err != nil {
		return err
	}
	a.Token.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.Token.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresIn > 0 {
		a.Token.ExpiresAt = tok.ExpiresAt()
	}
	a.LastRefresh = time.Now().Unix()
	return nil
}

// UpstreamURL maps a Switcher path to the Cloud Code backend (the CLI
// posts generateContent under v1internal).
func (p *Provider) UpstreamURL(path string) string {
	return apiBase + path
}

// ApplyAuth sets the headers the Cloud Code backend expects from the
// Gemini CLI.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	req.Header.Set("X-Goog-Api-Client", apiClient)
	req.Header.Set("User-Agent", userAgent)
	return nil
}

// IsExpired implements provider.Provider: refresh 30 minutes early.
func (p *Provider) IsExpired(a store.Account) bool {
	return a.Token.ExpiresAt > 0 && time.Now().Unix() >= a.Token.ExpiresAt-30*60
}

// Usage reuses the Cloud Code quota summary Antigravity reads. Without a
// project id there is nothing to ask, and a summary without groups means
// usage is simply not available. Note: the bucket parsing below is
// intentionally duplicated with antigravity's quotaSummary; extracting it
// would need googleauth to depend on the provider.UsageWindow type, which
// inverts the layering.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if ProjectID(a) == "" {
		return provider.Usage{}, fmt.Errorf("%w: no Cloud Code project", provider.ErrUsageUnavailable)
	}
	var summary struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				RemainingFraction  *float64 `json:"remainingFraction"`
				RemainingFraction2 *float64 `json:"remaining_fraction"`
				ResetTime          string   `json:"resetTime"`
				ResetTime2         string   `json:"reset_at"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := p.cloudcodePost(ctx, a.Token.AccessToken, version+":retrieveUserQuotaSummary",
		[]byte(fmt.Sprintf(`{"project_id":%q}`, ProjectID(a))), &summary); err != nil {
		return provider.Usage{}, fmt.Errorf("%w: %v", provider.ErrUsageUnavailable, err)
	}
	var windows []provider.UsageWindow
	for _, g := range summary.Groups {
		for _, b := range g.Buckets {
			fraction := b.RemainingFraction
			if fraction == nil {
				fraction = b.RemainingFraction2
			}
			if fraction == nil {
				continue
			}
			used := int((1 - *fraction) * 100)
			if used < 0 {
				used = 0
			}
			if used > 100 {
				used = 100
			}
			reset := b.ResetTime
			if reset == "" {
				reset = b.ResetTime2
			}
			window := provider.UsageWindow{
				Label:       g.DisplayName,
				UsedPercent: used,
			}
			if t, err := time.Parse(time.RFC3339, reset); err == nil {
				window.ResetsAt = t.Unix()
			}
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		return provider.Usage{}, fmt.Errorf("%w: no quota data", provider.ErrUsageUnavailable)
	}
	return provider.Usage{Available: true, Windows: windows}, nil
}

// ParseRateLimit implements provider.Provider. Cloud Code exhaustion
// surfaces as a 429 or a RESOURCE_EXHAUSTED body; park until the latest
// reset among exhausted windows, one hour when nothing is known.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	if status != http.StatusTooManyRequests && !strings.Contains(string(body), "RESOURCE_EXHAUSTED") {
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

// gcStatesLocked drops states for abandoned logins.
func (p *Provider) gcStatesLocked() {
	now := time.Now()
	for state, startedAt := range p.state {
		if now.Sub(startedAt) > stateTTL {
			delete(p.state, state)
		}
	}
}
