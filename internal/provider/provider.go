// Package provider defines the contract every upstream provider (Codex
// today; Grok and Claude later) implements. Adding a provider means
// implementing this interface and registering it.
package provider

import (
	"context"
	"errors"
	"net/http"

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

// Provider encapsulates everything Switcher needs to log in, refresh, and
// forward traffic for one upstream.
type Provider interface {
	// ID is the stable provider key, e.g. "codex".
	ID() string
	// DisplayName is shown in the UI.
	DisplayName() string
	// LoginStart starts an OAuth login and returns the URL to open in a
	// browser plus an opaque state value that round-trips the callback.
	LoginStart(ctx context.Context) (url string, state string, err error)
	// LoginExchange converts an OAuth callback code into an Account.
	LoginExchange(ctx context.Context, state, code string) (store.Account, error)
	// Refresh renews the account's token in place (updates Token fields).
	Refresh(ctx context.Context, a *store.Account) error
	// IsExpired reports whether the access token needs a refresh before use.
	IsExpired(a store.Account) bool
	// UpstreamURL maps a Switcher request path to the upstream URL.
	UpstreamURL(path string) string
	// ApplyAuth sets the authentication headers on an outgoing request.
	ApplyAuth(req *http.Request, a store.Account) error
	// Usage queries the account's current quota, best effort.
	Usage(ctx context.Context, a store.Account) (Usage, error)
}
