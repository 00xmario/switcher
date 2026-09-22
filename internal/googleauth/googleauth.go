// Package googleauth shares the Google OAuth2 token plumbing used by the
// Antigravity and Gemini providers: code exchange, refresh, userinfo, and
// the Cloud Code onboardUser flow their logins fall back to. The endpoints
// are package vars so tests can point them at a stub server.
package googleauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"switcher/internal/provider"
)

var (
	// TokenURL is Google's OAuth2 token endpoint.
	TokenURL = "https://oauth2.googleapis.com/token"
	// UserinfoURL is the endpoint that resolves an access token to an email.
	UserinfoURL = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
)

// Token is the subset of Google's token response Switcher stores.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// TokenExchange swaps an authorization code for tokens. secret is the
// OAuth client secret (Google web clients are confidential).
func TokenExchange(ctx context.Context, secret, code, clientID, redirectURI string) (Token, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {clientID},
		"client_secret": {secret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	return postToken(ctx, form)
}

// RefreshToken exchanges a refresh token for a fresh access token.
func RefreshToken(ctx context.Context, secret, refreshToken, clientID string) (Token, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {secret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	return postToken(ctx, form)
}

// Userinfo resolves an access token to the account's email.
func Userinfo(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserinfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("google userinfo: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("google userinfo: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("google userinfo: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google userinfo failed: http %d", resp.StatusCode)
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", fmt.Errorf("google userinfo: %w", err)
	}
	if info.Email == "" {
		return "", fmt.Errorf("google userinfo has no email")
	}
	return info.Email, nil
}

// postToken posts one form-encoded grant request and decodes the token.
func postToken(ctx context.Context, form url.Values) (Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("google token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("google token request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("google token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("google token request failed: http %d", resp.StatusCode)
	}
	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Token{}, fmt.Errorf("google token response: %w", err)
	}
	if tok.AccessToken == "" {
		return Token{}, fmt.Errorf("google token response missing access_token")
	}
	return tok, nil
}

// ExpiresAt converts an expires_in to a unix deadline.
func (t Token) ExpiresAt() int64 {
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
}

// OnboardOp is one Cloud Code onboardUser long-running operation response.
type OnboardOp struct {
	Done     bool `json:"done"`
	Response struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	} `json:"response"`
}

// OnboardUser runs the Cloud Code onboardUser flow: post body against
// base (the daily Cloud Code endpoint), then poll up to 5 times 2 seconds
// apart until the long-running operation reports the companion project.
// Requests impersonate the client described by userAgent and apiClient,
// which for both providers is the Antigravity hub client. The returned id
// may carry a "projects/<id>" path; strip it before storing.
func OnboardUser(ctx context.Context, accessToken, base, userAgent, apiClient string, body []byte) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var op OnboardOp
		if err := postOnboard(ctx, accessToken, base+"/v1internal:onboardUser", userAgent, apiClient, body, &op); err != nil {
			return "", err
		}
		if op.Done && op.Response.CloudaicompanionProject != "" {
			return op.Response.CloudaicompanionProject, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", errors.New("cloud code project onboarding did not complete")
}

// postOnboard posts one onboardUser request and decodes the operation.
func postOnboard(ctx context.Context, accessToken, target, userAgent, apiClient string, body []byte, out *OnboardOp) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("onboardUser: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Client", apiClient)
	req.Header.Set("User-Agent", userAgent)
	resp, err := provider.OAuthHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("onboardUser: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("onboardUser: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("onboardUser failed: http %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("onboardUser response: %w", err)
	}
	return nil
}
