// Package settings persists user preferences for the server: optional
// authentication, network exposure, and TLS. The file lives next to the
// account store with 0600 permissions and atomic writes, and every field
// degrades to the historic (no-auth, loopback) behavior when absent.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Iterations is the PBKDF2 work factor for password hashing. 600k is the
// current OWASP guidance for PBKDF2-SHA256 on password storage.
const Iterations = 600000

// SessionTTL is how long a login session stays valid without a renewal.
const SessionTTL = 7 * 24 * time.Hour

// Settings mirrors settings.json. Missing file means all defaults: auth
// disabled, loopback binding, no TLS.
type Settings struct {
	AuthEnabled        bool   `json:"auth_enabled"`
	PasswordSalt       string `json:"password_salt,omitempty"` // hex
	PasswordHash       string `json:"password_hash,omitempty"` // hex of the PBKDF2 output
	Iterations         int    `json:"iterations,omitempty"`
	BindLAN            bool   `json:"bind_lan,omitempty"`
	TLS                bool   `json:"tls,omitempty"`             // the LAN listener is TLS-only
	MenuUsageBars      *bool  `json:"menu_usage_bars,omitempty"` // nil means on
	ResetNotifications bool   `json:"reset_notifications,omitempty"`
	CompactAccounts    bool   `json:"compact_accounts,omitempty"`
	AutoUseReset       bool   `json:"auto_use_reset,omitempty"` // spend a banked reset when an account runs out
	CSRFToken          string `json:"csrf_token,omitempty"`
	UpdatedAt          int64  `json:"updated_at,omitempty"`
}

// Store owns settings.json plus the session and device-token files.
type Store struct {
	path     string
	tokenDir string

	mu                     sync.Mutex
	settingsMu             sync.Mutex
	tokenMu                sync.Mutex
	lastValid              Settings
	hasValid               bool
	readFailed             bool
	sessions               map[string]time.Time // sha256(token hex) -> expiry
	sessionGeneration      string
	sessionDeletionRetries map[string]sessionDeletionRetry
	fails                  int
	lockout                time.Time
}

// New builds a store rooted at the Switcher data directory.
func New(root string) *Store {
	return &Store{
		path:     filepath.Join(root, "settings.json"),
		tokenDir: root,
	}
}

// Path is the settings file location.
func (s *Store) Path() string { return s.path }

// Load retains the last valid snapshot on read errors. An unreadable initial
// configuration requires authentication, rather than exposing the dashboard.
func (s *Store) Load() Settings {
	st, _ := s.Snapshot()
	return st
}

// Snapshot distinguishes a new installation from a damaged or missing file
// after settings have been loaded. Security-sensitive callers must check err.
func (s *Store) Snapshot() (Settings, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.load()
}

func (s *Store) load() (Settings, error) {
	raw, err := readPrivateFile(s.path)
	var st Settings
	if os.IsNotExist(err) && !s.hasValid && !s.readFailed {
		return Settings{}, nil
	}
	if err == nil {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			err = errors.New("settings must be a JSON object")
		} else {
			err = json.Unmarshal(raw, &st)
		}
	}
	if err != nil {
		s.readFailed = true
		if s.hasValid {
			return cloneSettings(s.lastValid), err
		}
		return Settings{AuthEnabled: true}, err
	}
	s.lastValid, s.hasValid = cloneSettings(st), true
	s.readFailed = false
	return st, nil
}

func cloneSettings(st Settings) Settings {
	if st.MenuUsageBars != nil {
		value := *st.MenuUsageBars
		st.MenuUsageBars = &value
	}
	return st
}

// Save persists the settings atomically.
func (s *Store) Save(st Settings) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.save(st)
}

// Update atomically applies a read-modify-write alongside other settings
// writers, so independent preferences do not overwrite one another.
func (s *Store) Update(change func(*Settings) error) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if err := change(&st); err != nil {
		return err
	}
	return s.save(st)
}

func (s *Store) save(st Settings) error {
	st.UpdatedAt = time.Now().Unix()
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDirectory(filepath.Dir(s.path)); err != nil {
		return err
	}
	if err := writeAtomic(s.path, raw); err != nil {
		return err
	}
	s.lastValid, s.hasValid = cloneSettings(st), true
	return nil
}

// HasPassword reports whether a password hash exists on disk. LAN binding
// may only activate when this is true (no TOFU window).
func (s *Store) HasPassword() bool {
	return s.Load().PasswordHash != ""
}

// DisableAuth clears the password and turns authentication off, removing
// every credential file so the install is back to the historic behavior.
func (s *Store) DisableAuth() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	return s.disableAuthLocked(st)
}

func (s *Store) disableAuthLocked(st Settings) error {
	st.AuthEnabled, st.BindLAN, st.TLS = false, false, false
	st.PasswordHash, st.PasswordSalt, st.CSRFToken = "", "", ""
	if err := s.save(st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]time.Time{}
	s.sessionGeneration = credentialGeneration(st)
	s.sessionDeletionRetries = nil
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	return errors.Join(removePrivateFile(s.deviceTokenPath()), removePrivateFile(s.sessionsPath()),
		removePrivateFile(filepath.Join(s.tokenDir, "server.json")))
}

// Enabled reports whether authentication is switched on.
func (s *Store) Enabled() bool {
	st, err := s.Snapshot()
	return err != nil || st.AuthEnabled
}

// MenuUsageBars reports whether compact usage bars appear in the menu bar.
// Existing installs have no setting, so the bars default to on.
func (s *Store) MenuUsageBars() bool {
	value := s.Load().MenuUsageBars
	return value == nil || *value
}
