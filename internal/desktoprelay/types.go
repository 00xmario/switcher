// Package desktoprelay provides an opt-in, scope-authenticated Desktop CONNECT
// relay. Credential ownership and management authorization belong to its caller.
package desktoprelay

import (
	"context"
	"crypto/x509"
	"errors"
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
)

type Credential struct {
	AccountID   string `json:"account_id"`
	AccessToken string `json:"-"`
	// AccountUUID is the Anthropic account UUID the token belongs to, if known.
	AccountUUID string `json:"-"`
}

// CredentialSource owns refresh serialization and generation adoption. It must
// never activate native credentials as a side effect of these methods.
type CredentialSource interface {
	Prepare(context.Context, string) (Credential, error)
	RefreshRejected(context.Context, string, string) (Credential, error)
}

// ConversationResolver maps request session UUIDs to Desktop conversation
// UUIDs using native metadata. Missing keys are unknown sessions.
type ConversationResolver interface {
	Resolve(context.Context, []string) (map[string]string, error)
}

// CachedConversationResolver optionally answers from an in-memory snapshot
// without I/O, for use on the request path.
type CachedConversationResolver interface {
	ResolveCached(ids []string) map[string]string
}

// RemoteInference sends a Claude inference request to the Switcher host this
// Mac is connected to, which applies account (or its own selection when
// empty). ok is false when no host is connected.
type RemoteInference func(ctx context.Context, account string, r *http.Request, body []byte) (resp *http.Response, ok bool, err error)

type Config struct {
	DataRoot      string
	Port          int
	Source        CredentialSource
	Conversations ConversationResolver
	// Transport and DialContext are trusted fixture hooks. Production leaves both nil.
	Transport   http.RoundTripper
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// SyncDirectory is a trusted fixture fault-injection hook. Production leaves it nil.
	SyncDirectory func(*os.File) error
	// SyncSetupFile injects settings-file fsync failures in isolated fixtures.
	// Production leaves it nil.
	SyncSetupFile func(*os.File) error
	// FixtureTLSRoots avoids consulting system trust during fixture TLS tests.
	// Production leaves this nil and verifies upstream with system roots.
	FixtureTLSRoots *x509.CertPool
}

type Status struct {
	Enabled   bool `json:"enabled"`
	Listening bool `json:"listening"`
	// InFlight counts requests currently being relayed.
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
	ScopeID         string            `json:"scope_id"`
	SessionID       string            `json:"session_id"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	AccountID       string            `json:"account_id"`
	Revision        uint64            `json:"revision"`
	LastSeen        time.Time         `json:"last_seen"`
	Requests        uint64            `json:"requests"`
	InFlight        uint64            `json:"in_flight"`
	AgentID         string            `json:"agent_id,omitempty"`
	ParentSessionID string            `json:"parent_session_id,omitempty"`
	Model           string            `json:"model,omitempty"`
	LastResponse    *ResponseEvidence `json:"last_response,omitempty"`
}

// ResponseEvidence is the last upstream status received for a Messages request.
type ResponseEvidence struct {
	Route     string    `json:"route"`
	AccountID string    `json:"account_id,omitempty"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	At        time.Time `json:"at"`
}

type ConversationBinding struct {
	ScopeID        string `json:"scope_id"`
	ConversationID string `json:"conversation_id"`
	AccountID      string `json:"account_id"`
	Revision       uint64 `json:"revision"`
}
