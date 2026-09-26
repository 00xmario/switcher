// Package settings persists user preferences for the server: optional
// authentication, network exposure, and TLS. The file lives next to the
// account store with 0600 permissions and atomic writes, and every field
// degrades to the historic (no-auth, loopback) behavior when absent.
package settings

import (
	"encoding/json"
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
	CSRFToken          string `json:"csrf_token,omitempty"`
	UpdatedAt          int64  `json:"updated_at,omitempty"`
}

// Store owns settings.json plus the session and device-token files.
type Store struct {
	path     string
	tokenDir string

	mu         sync.Mutex
	settingsMu sync.Mutex
	sessions   map[string]time.Time // sha256(token hex) -> expiry
	fails      int
	lockout    time.Time
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

// Load reads settings.json; any error (missing, corrupt) means defaults.
func (s *Store) Load() Settings {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.load()
}

func (s *Store) load() Settings {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return Settings{}
	}
	var st Settings
	if json.Unmarshal(raw, &st) != nil {
		return Settings{}
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
	st := s.load()
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return writeAtomic(s.path, raw)
}

// HasPassword reports whether a password hash exists on disk. LAN binding
// may only activate when this is true (no TOFU window).
func (s *Store) HasPassword() bool {
	return s.Load().PasswordHash != ""
}

// DisableAuth clears the password and turns authentication off, removing
// every credential file so the install is back to the historic behavior.
func (s *Store) DisableAuth() error {
	if err := s.Update(func(st *Settings) error {
		st.AuthEnabled = false
		st.PasswordHash = ""
		st.PasswordSalt = ""
		st.CSRFToken = ""
		st.BindLAN = false
		st.TLS = false
		return nil
	}); err != nil {
		return err
	}
	// Drop the in-memory session cache too: stale cookies must not revive
	// when a password is set again later.
	s.mu.Lock()
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
	_ = os.Remove(s.deviceTokenPath())
	_ = os.Remove(s.sessionsPath())
	_ = os.Remove(filepath.Join(s.tokenDir, "server.json"))
	return nil
}

// Enabled reports whether authentication is switched on.
func (s *Store) Enabled() bool { return s.Load().AuthEnabled }

// MenuUsageBars reports whether compact usage bars appear in the menu bar.
// Existing installs have no setting, so the bars default to on.
func (s *Store) MenuUsageBars() bool {
	value := s.Load().MenuUsageBars
	return value == nil || *value
}

func writeAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
