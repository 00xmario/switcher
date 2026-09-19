// Package provider defines the contract every upstream provider (Codex,
// Claude, Grok, OpenCode) implements. Adding a provider means implementing
// this interface and registering it.
package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"switcher/internal/store"
)

// UsageWindow is one quota window (session, weekly, ...) of one account.
type UsageWindow struct {
	Label       string `json:"label"`
	UsedPercent int    `json:"used_percent"`
	ResetsAt    int64  `json:"resets_at,omitempty"` // unix seconds
}

// Usage is a provider-specific usage snapshot for the UI. It is best
// effort: providers that cannot query usage return ErrUsageUnavailable and
// the UI shows "unknown" instead of a hard failure.
type Usage struct {
	Available bool          `json:"available"`
	Windows   []UsageWindow `json:"windows,omitempty"`
}

// ErrUsageUnavailable reports that usage could not be determined.
var ErrUsageUnavailable = errors.New("usage unavailable")

// ErrUnsupported reports that a provider does not implement an optional
// flow (e.g. an OAuth-only provider asked for device login).
var ErrUnsupported = errors.New("unsupported flow for this provider")

// LoginInfo describes how the user completes a login.
type LoginInfo struct {
	// Kind is "browser" (open URL, provider calls back) or "device"
	// (user visits VerificationURL and enters UserCode).
	Kind string `json:"kind"`
	// URL is the browser authorization URL for kind "browser".
	URL string `json:"url,omitempty"`
	// VerificationURL is where the user confirms a device code.
	VerificationURL string `json:"verification_url,omitempty"`
	// UserCode is the short code shown to the user for device flows.
	UserCode string `json:"user_code,omitempty"`
	// State identifies the attempt for polling.
	State string `json:"state"`
}

// Provider encapsulates everything Switcher needs to log in, refresh, and
// forward traffic for one upstream.
type Provider interface {
	// ID is the stable provider key, e.g. "codex".
	ID() string
	// DisplayName is shown in the UI.
	DisplayName() string

	// LoginStart begins a browser (PKCE) OAuth login. Returns the URL the
	// user must open plus an opaque state that round-trips the callback.
	LoginStart(ctx context.Context) (LoginInfo, error)
	// LoginExchange converts an OAuth callback code into an Account.
	LoginExchange(ctx context.Context, state, code string) (store.Account, error)

	// DeviceStart begins a device-code login (grok). The returned poll
	// function blocks until the user authorizes or the flow expires.
	DeviceStart(ctx context.Context) (info LoginInfo, poll func(ctx context.Context) (store.Account, error), err error)

	// AddByKey creates an account from an API key (opencode).
	AddByKey(ctx context.Context, key string) (store.Account, error)

	// Refresh renews the account's token in place (updates Token fields).
	// API-key accounts are never expired, so this is a no-op for them.
	Refresh(ctx context.Context, a *store.Account) error
	// IsExpired reports whether the access token needs a refresh before use.
	IsExpired(a store.Account) bool
	// UpstreamURL maps a Switcher request path (already stripped of the
	// provider prefix) to the upstream URL.
	UpstreamURL(path string) string
	// ApplyAuth sets the authentication headers on an outgoing request.
	ApplyAuth(req *http.Request, a store.Account) error

	// Usage queries the account's current quota, best effort.
	Usage(ctx context.Context, a store.Account) (Usage, error)
	// ParseRateLimit classifies an upstream response: is the account out of
	// usage, and if so until when? exhausted=false means the failure is
	// something else (e.g. a burst limit) and must not trigger switching.
	ParseRateLimit(ctx context.Context, a store.Account, status int, body []byte) (resetsAt time.Time, exhausted bool)
}
