// Package grok implements the provider.Provider interface for xAI Grok
// (Grok CLI subscription logins). Login uses OAuth2 device authorization,
// and requests forward to the Grok CLI chat proxy.
package grok

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

// Public OAuth client of the Grok CLI, and the endpoints its OIDC
// discovery document resolves to.
const (
	clientID        = "b1a00492-073a-47ea-816f-4c329264a828"
	scope           = "openid profile email offline_access grok-cli:access api:access"
	issuer          = "https://auth.x.ai"
	discoveryURL    = issuer + "/.well-known/openid-configuration"
	upstreamBase    = "https://cli-chat-proxy.grok.com/v1"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
)

// Provider implements provider.Provider for Grok.
type Provider struct{}

// New returns a ready-to-register Grok provider.
func New() *Provider { return &Provider{} }

// ID implements provider.Provider.
func (p *Provider) ID() string { return "grok" }

// DisplayName implements provider.Provider.
func (p *Provider) DisplayName() string { return "Grok (xAI)" }

// LoginStart implements provider.Provider: grok uses the device flow.
func (p *Provider) LoginStart(ctx context.Context) (provider.LoginInfo, error) {
	return provider.LoginInfo{}, provider.ErrUnsupported
}

// LoginExchange implements provider.Provider: no browser callback exists.
func (p *Provider) LoginExchange(ctx context.Context, state, code string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// DeviceStart begins the OAuth2 device authorization grant: request a
// device code, show the user the verification URL and code, and return a
// poll function that resolves once xAI issues the tokens.
func (p *Provider) DeviceStart(ctx context.Context) (provider.LoginInfo, func(ctx context.Context) (store.Account, error), error) {
	discovery, err := discover(ctx)
	if err != nil {
		return provider.LoginInfo{}, nil, err
	}

	form := url.Values{
		"client_id": {clientID},
		"scope":     {scope},
	}
	body, err := postForm(ctx, discovery.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return provider.LoginInfo{}, nil, err
	}
	var device struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := json.Unmarshal(body, &device); err != nil {
		return provider.LoginInfo{}, nil, fmt.Errorf("device code response: %w", err)
	}
	if device.DeviceCode == "" || device.VerificationURI == "" {
		return provider.LoginInfo{}, nil, errors.New("device code response incomplete")
	}

	state := device.DeviceCode // opaque handle for login polling
	info := provider.LoginInfo{
		Kind:            "device",
		VerificationURL: device.VerificationURI,
		UserCode:        device.UserCode,
		State:           state,
	}
	poll := func(ctx context.Context) (store.Account, error) {
		return p.pollForToken(ctx, discovery.TokenEndpoint, device.DeviceCode, device.Interval)
	}
	return info, poll, nil
}

// pollForToken polls the token endpoint with the device code until the
// user authorizes, the code expires, or the context ends. While waiting,
// xAI answers with HTTP 400 carrying error "authorization_pending": that
// is the protocol working, not a failure.
func (p *Provider) pollForToken(ctx context.Context, tokenEndpoint, deviceCode string, interval int64) (store.Account, error) {
	intervalDur := time.Duration(interval) * time.Second
	if intervalDur < 5*time.Second {
		intervalDur = 5 * time.Second
	}
	deadline := time.Now().Add(30 * time.Minute)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return store.Account{}, ctx.Err()
		case <-time.After(intervalDur):
		}

		form := url.Values{
			"grant_type":  {deviceGrantType},
			"device_code": {deviceCode},
			"client_id":   {clientID},
		}
		status, body, err := postFormLenient(ctx, tokenEndpoint, form)
		if err != nil {
			return store.Account{}, err
		}
		var payload struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
			AccessToken      string `json:"access_token"`
			RefreshToken     string `json:"refresh_token"`
			IDToken          string `json:"id_token"`
			ExpiresIn        int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return store.Account{}, fmt.Errorf("device token response: %w", err)
		}
		switch {
		case payload.AccessToken != "":
			return accountFromToken(oauthToken{
				AccessToken:  payload.AccessToken,
				RefreshToken: payload.RefreshToken,
				IDToken:      payload.IDToken,
				ExpiresIn:    payload.ExpiresIn,
			})
		case payload.Error == "authorization_pending" || payload.Error == "slow_down":
			continue // keep polling
		case payload.Error != "":
			return store.Account{}, fmt.Errorf("device flow failed: %s", payload.Error)
		default:
			return store.Account{}, fmt.Errorf("device token poll: unexpected http %d", status)
		}
	}
	return store.Account{}, errors.New("device flow timed out")
}

// postFormLenient posts a form and returns the body for ANY status: the
// device-grant protocol signals "still waiting" through 400 responses.
func postFormLenient(ctx context.Context, target string, form url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("grok token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("grok token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("grok token response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// AddByKey implements provider.Provider: grok accounts come from OAuth.
func (p *Provider) AddByKey(ctx context.Context, key string) (store.Account, error) {
	return store.Account{}, provider.ErrUnsupported
}

// Refresh renews the account's tokens using the OIDC token endpoint.
func (p *Provider) Refresh(ctx context.Context, a *store.Account) error {
	if a.Token.RefreshToken == "" {
		return errors.New("no refresh token; sign in again")
	}
	// Resolve the token endpoint fresh; xAI rotates it through discovery.
	discovery, err := discover(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {a.Token.RefreshToken},
	}
	body, err := postForm(ctx, discovery.TokenEndpoint, form)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return fmt.Errorf("refresh response: %w", err)
	}
	if tok.AccessToken == "" {
		return errors.New("refresh response missing access_token")
	}
	a.Token.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.Token.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		a.Token.IDToken = tok.IDToken
	}
	if tok.ExpiresIn > 0 {
		a.Token.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a.LastRefresh = time.Now().Unix()
	return nil
}

// UpstreamURL maps a Switcher path to the Grok CLI chat proxy.
func (p *Provider) UpstreamURL(path string) string {
	return upstreamBase + strings.TrimPrefix(path, "/v1")
}

// ApplyAuth sets the headers the Grok chat proxy expects.
func (p *Provider) ApplyAuth(req *http.Request, a store.Account) error {
	req.Header.Set("Authorization", "Bearer "+a.Token.AccessToken)
	return nil
}

// IsExpired implements provider.Provider.
func (p *Provider) IsExpired(a store.Account) bool {
	return a.Token.ExpiresAt > 0 && time.Now().Unix() >= a.Token.ExpiresAt-60
}

// Usage queries the Grok CLI billing endpoint, which reports the current
// subscription period's credit usage.
func (p *Provider) Usage(ctx context.Context, a store.Account) (provider.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		upstreamBase+"/billing?format=credits", nil)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("%w: build request: %v", provider.ErrUsageUnavailable, err)
	}
	if err := p.ApplyAuth(req, a); err != nil {
		return provider.Usage{}, fmt.Errorf("%w: auth: %v", provider.ErrUsageUnavailable, err)
	}
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return provider.Usage{}, fmt.Errorf("%w: %v", provider.ErrUsageUnavailable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return provider.Usage{}, fmt.Errorf("%w: read body: %v", provider.ErrUsageUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return provider.Usage{}, fmt.Errorf("%w: upstream status %d: %s", provider.ErrUsageUnavailable, resp.StatusCode, truncForLog(raw))
	}

	var parsed struct {
		Config struct {
			CreditUsagePercent *float64 `json:"creditUsagePercent"`
			CurrentPeriod      *struct {
				Type string `json:"type"`
				End  string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return provider.Usage{}, fmt.Errorf("%w: decode: %v", provider.ErrUsageUnavailable, err)
	}
	if parsed.Config.CreditUsagePercent == nil {
		return provider.Usage{}, fmt.Errorf("%w: no creditUsagePercent in response", provider.ErrUsageUnavailable)
	}

	label := "Subscription"
	kind := strings.TrimPrefix(parsed.Config.CurrentPeriod.Type, "USAGE_PERIOD_TYPE_")
	switch strings.ToLower(kind) {
	case "weekly":
		label = "Weekly"
	case "monthly":
		label = "Monthly"
	}
	window := provider.UsageWindow{
		Label:       label,
		UsedPercent: int(*parsed.Config.CreditUsagePercent),
	}
	if parsed.Config.CurrentPeriod != nil && parsed.Config.CurrentPeriod.End != "" {
		if t, err := time.Parse(time.RFC3339, parsed.Config.CurrentPeriod.End); err == nil {
			window.ResetsAt = t.Unix()
		}
	}
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{window}}, nil
}

// ParseRateLimit implements provider.Provider. Grok's subscription exhaustion
// surfaces as a non-200 from the chat proxy; the billing snapshot decides
// whether the account is really out of usage and until when.
func (p *Provider) ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (time.Time, bool) {
	if status < 400 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 401/403 can also be token expiry; the refresh path handles those.
		if !strings.Contains(strings.ToLower(string(body)), "usage") {
			return time.Time{}, false
		}
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

// discovery resolves xAI's OAuth endpoints.
func discover(ctx context.Context) (*struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("grok discovery: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok discovery: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("grok discovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok discovery failed: http %d", resp.StatusCode)
	}
	var d struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("grok discovery: %w", err)
	}
	if d.DeviceAuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("grok discovery incomplete")
	}
	return &d, nil
}

func postForm(ctx context.Context, target string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("grok request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grok token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("grok token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grok token request failed: http %d", resp.StatusCode)
	}
	return raw, nil
}

// jwtIdentity extracts email and subject from the id_token claims.
func jwtIdentity(idToken string) (email, subject string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	email, _ = claims["email"].(string)
	subject, _ = claims["sub"].(string)
	return strings.TrimSpace(email), strings.TrimSpace(subject)
}

// oauthToken carries the token fields shared by device and refresh flows.
type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// accountFromToken derives the stored account from the token set.
func accountFromToken(tok oauthToken) (store.Account, error) {
	email, subject := jwtIdentity(tok.IDToken)
	if email == "" {
		return store.Account{}, errors.New("id_token has no email claim")
	}
	acc := store.Account{
		Provider:  "grok",
		Email:     email,
		CreatedAt: time.Now().Unix(),
		Token: store.Token{
			IDToken:      tok.IDToken,
			AccessToken:  tok.AccessToken,
			RefreshToken: tok.RefreshToken,
			AccountID:    subject,
			ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		},
	}
	sum := sha256.Sum256([]byte(acc.Provider + "|" + email + "|" + subject))
	acc.ID = fmt.Sprintf("%s-%x", acc.Provider, sum[:4])
	return acc, nil
}

// truncForLog shortens an upstream body for log lines.
func truncForLog(b []byte) string {
	const n = 200
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
