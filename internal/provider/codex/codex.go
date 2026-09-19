// Package codex implements the provider.Provider interface for Codex
// (ChatGPT Plus/Pro subscription logins). It performs the same OAuth PKCE
// flow the Codex CLI uses and forwards requests to the ChatGPT
// backend-api Codex endpoint.
package codex

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
	"sort"
	"strings"
	"sync"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Public OAuth client of the Codex CLI. It is a native (public) client, so
// no client secret is involved anywhere.
const (
	clientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	authBase     = "https://auth.openai.com"
	authorizeURL = authBase + "/oauth/authorize"
	tokenURL     = authBase + "/oauth/token"
	upstreamBase = "https://chatgpt.com/backend-api/codex"
	scope        = "openid email profile offline_access"
	redirectURI  = "http://localhost:1455/auth/callback"
)

// Provider implements provider.Provider for Codex (ChatGPT Plus/Pro
// subscription logins). It performs the same OAuth PKCE flow the Codex CLI
// uses and forwards requests to the ChatGPT backend-api Codex endpoint.
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

// New returns a ready-to-register Codex provider.
func New() *Provider {
	return &Provider{verifier: map[string]verifierEntry{}}
}

// ID implements provider.Provider.
func (p *Provider) ID() string { return "codex" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Codex (ChatGPT)" }

// LoginStart builds the OAuth authorization URL the user must open.
func (p *Provider) LoginStart(_ context.Context) (provider.LoginInfo, error) {
	verifierBytes := make([]byte, 32)
	if _, err := rand.Read(verifierBytes); err != nil {
		return provider.LoginInfo{}, fmt.Errorf("generate pkce verifier: %w", err)
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return provider.LoginInfo{}, fmt.Errorf("generate state: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	p.mu.Lock()
	p.gcVerifiersLocked()
	p.verifier[state] = verifierEntry{value: verifier, startedAt: time.Now()}
	p.mu.Unlock()

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scope)
	q.Set("state", state)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("id_token_add_organizations", "true")
	q.Set("prompt", "login")
	return provider.LoginInfo{Kind: "browser", URL: authorizeURL + "?" + q.Encode(), State: state}, nil
}

// DeviceStart implements provider.Provider: codex has no device flow.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	return provider.LoginInfo{}, nil, provider.ErrUnsupported
}

// AddByKey implements provider.Provider: codex accounts come from OAuth.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// LoginExchange swaps the authorization code for tokens and derives the
// account identity from the id_token claims.
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

	tok, err := tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {entry.value},
		"redirect_uri":  {redirectURI},
	})
	if err != nil {
		return store.Account{}, err
	}
	return accountFromToken(tok)
}

// Refresh renews the account's tokens using the stored refresh token.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	if a.Token.RefreshToken == "" {
		return errors.New("no refresh token; sign in again")
	}
	tok, err := tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {a.Token.RefreshToken},
		"client_id":     {clientID},
	})
	if err != nil {
		return err
	}
	applyToken(a, tok)
	return nil
}

// UpstreamURL maps a Switcher path (e.g. "/v1/responses") to the Codex
// backend endpoint.
func (p *Provider) UpstreamURL(path string) string {
	return upstreamBase + strings.TrimPrefix(path, "/v1")
}

// ApplyAuth sets the headers Codex's backend expects for OAuth requests.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	if a.Token.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", a.Token.AccountID)
	}
	return nil
}

// rateLimitWindow is the upstream usage shape of one quota window.
type rateLimitWindow struct {
	LimitWindowSeconds int64 `json:"limit_window_seconds"`
	ResetAfterSeconds  int64 `json:"reset_after_seconds"`
	ResetAt            int64 `json:"reset_at"`
	UsedPercent        int   `json:"used_percent"`
}

// Usage queries the Codex usage endpoint and maps the upstream rate-limit
// windows (session, weekly, ...) into provider-agnostic UsageWindows.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamBase+"/usage", nil)
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("originator", "codex_cli_rs")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}

	var parsed struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *rateLimitWindow `json:"primary_window"`
			Secondary *rateLimitWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}

	var windows []provider.UsageWindow
	for _, w := range []*rateLimitWindow{parsed.RateLimit.Primary, parsed.RateLimit.Secondary} {
		if w == nil || w.LimitWindowSeconds <= 0 {
			continue
		}
		windows = append(windows, provider.UsageWindow{
			Label:       windowLabel(w.LimitWindowSeconds),
			UsedPercent: w.UsedPercent,
			ResetsAt:    w.ResetAt,
		})
	}
	return provider.Usage{Available: true, Windows: windows}, nil
}

// windowLabel names a quota window by its duration.
func windowLabel(seconds int64) string {
	switch {
	case seconds <= 6*3600:
		return "Session"
	case seconds <= 8*24*3600:
		return "Weekly"
	case seconds <= 31*24*3600:
		return "Monthly"
	default:
		return fmt.Sprintf("%dd", seconds/(24*3600))
	}
}

// codexUserAgent mirrors the CLI's User-Agent so usage queries pass the
// upstream's client checks the same way proxied traffic does.
const codexUserAgent = "codex_cli_rs/0.154.0 (Mac OS 26.0.0; arm64)"

// ParseRateLimit implements provider.Provider. Codex reports subscription
// exhaustion as a 429 whose body carries error.type "usage_limit_reached"
// and an upstream reset timestamp.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	var parsed struct {
		Error struct {
			Type     string `json:"type"`
			ResetsAt int64  `json:"resets_at"`
		} `json:"error"`
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(body, &parsed) != nil {
		return time.Time{}, false
	}
	if parsed.Error.Type != "usage_limit_reached" {
		return time.Time{}, false
	}
	if parsed.Error.ResetsAt > 0 {
		return time.Unix(parsed.Error.ResetsAt, 0), true
	}
	return time.Now().Add(time.Hour), true
}

// codexCreditBase is the OpenAI control-plane endpoint that lists and
// consumes banked rate-limit reset credits for a Codex subscription.
const codexCreditBase = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

// ListResetCredits implements provider.ResetCreditProvider. It returns the
// account's unexpired, unused banked resets, earliest expiry first.
func (p *Provider) ListResetCredits(ctx context.Context, a store.Account) ([]provider.ResetCredit, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexCreditBase, nil)
	if err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("originator", "codex_cli_rs")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list reset credits: http %d", resp.StatusCode)
	}
	var parsed struct {
		Credits []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ResetType string `json:"reset_type"`
			ExpiresAt string `json:"expires_at"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("list reset credits: %w", err)
	}
	now := time.Now()
	var out []provider.ResetCredit
	for _, c := range parsed.Credits {
		if c.ResetType != "codex_rate_limits" || c.Status != "available" {
			continue
		}
		exp, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil || !exp.After(now) {
			continue
		}
		out = append(out, provider.ResetCredit{ID: c.ID, ExpiresAt: exp.Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpiresAt < out[j].ExpiresAt })
	return out, nil
}

// ConsumeResetCredit spends one banked reset and reports the upstream
// outcome ("reset", "nothing_to_reset", "no_credit", "already_redeemed").
// The redeem request id is deterministic per account and credit so retries
// cannot double-spend a credit.
// ConsumeResetCredit implements provider.ResetCreditProvider.
func (p *Provider) ConsumeResetCredit(ctx context.Context, a store.Account, creditID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reqBody, err := json.Marshal(map[string]any{
		"redeem_request_id": redeemRequestID(a.Token.AccountID, creditID),
		"credit_id":         creditID,
	})
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexCreditBase+"/consume", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("originator", "codex_cli_rs")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("consume reset credit: http %d", resp.StatusCode)
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("consume reset credit: %w", err)
	}
	if out.Code == "" {
		return "", errors.New("consume reset credit: missing code")
	}
	return out.Code, nil
}

// redeemRequestIDSalt reproduces the per-account redemption identity used
// across the Codex ecosystem; the value must stay stable.
const redeemRequestIDSalt = "6f1c2a9e2d4b4c1e9a7f3b8d5e0c1a42"

func redeemRequestID(accountID, creditID string) string {
	digest := sha256.New()
	digest.Write([]byte(redeemRequestIDSalt))
	digest.Write([]byte(accountID + ":" + creditID))
	raw := digest.Sum(nil)[:16]
	// Format as a UUID so the server accepts it.
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// IsExpired implements provider.Provider.
func (p *Provider) IsExpired(a store.Account) bool {
	return a.Token.ExpiresAt > 0 && time.Now().Unix() >= a.Token.ExpiresAt-60
}

// oauthToken is the wire shape of the token endpoint response.
type oauthToken struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"chatgpt_account_id,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
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

func tokenRequest(ctx context.Context, form url.Values) (oauthToken, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthToken{}, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return oauthToken{}, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return oauthToken{}, fmt.Errorf("token request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return oauthToken{}, fmt.Errorf("token request: http %d", resp.StatusCode)
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

func applyToken(a *store.Account, tok oauthToken) {
	a.Token.IDToken = tok.IDToken
	a.Token.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.Token.RefreshToken = tok.RefreshToken
	}
	if tok.AccountID != "" {
		a.Token.AccountID = tok.AccountID
	}
	a.Token.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	a.LastRefresh = time.Now().Unix()
}

// accountFromToken builds the stored account, deriving identity and plan
// from the id_token JWT claims.
func accountFromToken(tok oauthToken) (store.Account, error) {
	claims, err := decodeJWTClaims(tok.IDToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("decode id_token: %w", err)
	}
	email := stringClaim(claims, "email")
	if email == "" {
		return store.Account{}, errors.New("id_token has no email claim")
	}
	authClaim, _ := claims["https://api.openai.com/auth"].(map[string]any)
	plan := stringClaim(claims, "chatgpt_plan_type")
	if plan == "" && authClaim != nil {
		plan = stringClaim(authClaim, "chatgpt_plan_type")
	}
	accountID := stringClaim(claims, "chatgpt_account_id")
	if accountID == "" && authClaim != nil {
		accountID = stringClaim(authClaim, "chatgpt_account_id")
	}
	if tok.AccountID != "" {
		accountID = tok.AccountID
	}

	acc := store.Account{
		Provider:  "codex",
		Email:     email,
		Plan:      plan,
		CreatedAt: time.Now().Unix(),
	}
	acc.Token = store.Token{
		IDToken:      tok.IDToken,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		AccountID:    accountID,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + acc.Email + "|" + accountID))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

func decodeJWTClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	return claims, nil
}

func stringClaim(claims map[string]any, key string) string {
	v, _ := claims[key].(string)
	return strings.TrimSpace(v)
}
