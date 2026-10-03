// Package desktoprelay provides an opt-in, scope-authenticated Desktop CONNECT
// relay. Credential ownership and management authorization belong to its caller.
package desktoprelay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

var (
	ErrConflict    = errors.New("desktop relay revision conflict")
	ErrNotFound    = errors.New("desktop relay task or scope not found")
	ErrUnavailable = errors.New("desktop relay unavailable")
	ErrBusy        = errors.New("desktop relay busy")
	// Credential contention maps to 503, separately from lifecycle/CAS busy 409.
	ErrCredentialBusy = fmt.Errorf("%w: credential source busy", ErrUnavailable)
)

type Credential struct {
	AccountID   string `json:"account_id"`
	AccessToken string `json:"-"`
}

// CredentialSource owns refresh serialization and generation adoption. It must
// never activate native credentials as a side effect of these methods.
type CredentialSource interface {
	Prepare(context.Context, string) (Credential, error)
	RefreshRejected(context.Context, string, string) (Credential, error)
}

// ConversationResolver reads trusted native metadata. Keys are requested network
// session UUIDs; values are canonical Desktop UUIDs without a local_ prefix.
// Missing keys are unknown associations. Implementations must honor cancellation
// and refresh metadata when a requested alias is absent from their cache.
type ConversationResolver interface {
	Resolve(context.Context, []string) (map[string]string, error)
}

// CachedConversationResolver optionally supplies discovery from an existing
// snapshot only. It must not perform I/O, refresh, or wait for a metadata scan.
// Cold/unavailable snapshots return no associations. Known contains only the
// core's scoped, previously verified associations, as with historical resolution.
type CachedConversationResolver interface {
	ResolveCached(ids []string, known map[string]string) map[string]string
}

// HistoricalConversationResolver is an optional capability of Conversations.
// Its result includes current proof and may retain an alias from known only if
// the same immutable native conversation remains valid and no current claim,
// ambiguity, veto or truncated snapshot contradicts that association. Known is
// supplied exclusively from unambiguous core-persisted Session.ConversationID.
// The resolver must never infer history from titles, bindings or caller fields.
type HistoricalConversationResolver interface {
	ResolveHistorical(ctx context.Context, ids []string, known map[string]string) (map[string]string, error)
}

type Config struct {
	DataRoot      string
	Port          int
	Source        CredentialSource
	Conversations ConversationResolver
	// Transport and DialContext are trusted fixture hooks. Production leaves both nil.
	Transport   http.RoundTripper
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// LookupIP is a fixture DNS seam. Production uses the system resolver.
	LookupIP func(context.Context, string) ([]net.IP, error)
	// SyncDirectory is a trusted fixture fault-injection hook. Production leaves it nil.
	SyncDirectory func(*os.File) error
	// SyncSetupFile injects settings-file fsync failures in isolated fixtures.
	// Production leaves it nil.
	SyncSetupFile func(*os.File) error
	// FixtureTLSRoots avoids consulting system trust during fixture TLS tests.
	// Production leaves this nil and verifies upstream with system roots.
	FixtureTLSRoots *x509.CertPool
	// FixtureReadTimeout shortens body reads and rejection cleanup in fixtures.
	// Production leaves this zero.
	FixtureReadTimeout time.Duration
}

type Status struct {
	Enabled   bool `json:"enabled"`
	Listening bool `json:"listening"`
	// InFlight counts active runtime requests, including revoked sessions and
	// requests still preparing credentials. It is independent of session state.
	InFlight   uint64 `json:"in_flight"`
	Address    string `json:"address"`
	CAPath     string `json:"ca_path"`
	Condition  string `json:"condition"`
	Validation string `json:"validation"`
}

// ReplyStatus is an alias for management integrations that use reply type names.
type ReplyStatus = Status

type Scope struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type ScopeSetup struct {
	Scope
	ProxyURL string            `json:"proxy_url"`
	CAPath   string            `json:"ca_path"`
	Env      map[string]string `json:"env"`
}

type Session struct {
	ScopeID              string            `json:"scope_id"`
	SessionID            string            `json:"session_id"`
	ConversationID       string            `json:"conversation_id,omitempty"`
	AccountID            string            `json:"account_id"`
	Revision             uint64            `json:"revision"`
	LastSeen             time.Time         `json:"last_seen"`
	Requests             uint64            `json:"requests"`
	InFlight             uint64            `json:"in_flight"`
	AgentID              string            `json:"agent_id,omitempty"`
	ParentSessionID      string            `json:"parent_session_id,omitempty"`
	Model                string            `json:"model,omitempty"`
	LastResponse         *ResponseEvidence `json:"last_response,omitempty"`
	conversationRevision uint64
}

// ResponseEvidence proves receipt of upstream headers for a Messages request.
// It does not claim completed generation or independently verified billing.
type ResponseEvidence struct {
	Route     string    `json:"route"`
	AccountID string    `json:"account_id,omitempty"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	At        time.Time `json:"at"`
	Sequence  uint64    `json:"sequence,omitempty"`
}

type ConversationBinding struct {
	ScopeID        string `json:"scope_id"`
	ConversationID string `json:"conversation_id"`
	AccountID      string `json:"account_id"`
	Revision       uint64 `json:"revision"`
}
