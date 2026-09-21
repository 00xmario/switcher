package settings

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HashPassword derives the stored password hash: PBKDF2-SHA256 with the
// configured iterations, hex-encoded.
func HashPassword(password string, salt []byte) []byte {
	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, 32)
	if err != nil {
		// PBKDF2 with a valid hash never fails; a zero key would still be
		// stored, but that cannot happen here.
		return make([]byte, 32)
	}
	return key
}

// pbkdf2Hash returns the hex digest for constant-time comparison.
func pbkdf2Hash(password string, salt []byte, iterations int) string {
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(key)
}

// SetPassword stores a PBKDF2 hash of the password with a fresh salt and
// switches authentication on. A new CSRF token is issued for the session.
func (s *Store) SetPassword(password string) error {
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	st := s.Load()
	st.AuthEnabled = true
	st.PasswordSalt = hex.EncodeToString(salt)
	st.PasswordHash = pbkdf2Hash(password, salt, Iterations)
	st.Iterations = Iterations
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return err
	}
	st.CSRFToken = hex.EncodeToString(csrf)
	if err := s.Save(st); err != nil {
		return err
	}
	s.ResetFailures()
	return nil
}

// ChangePassword verifies the current password (when one exists) and stores
// the new one, invalidating every session.
func (s *Store) ChangePassword(current, next string) error {
	st := s.Load()
	if st.PasswordHash != "" && !s.VerifyPassword(current) {
		return errors.New("wrong password")
	}
	if err := s.SetPassword(next); err != nil {
		return err
	}
	s.clearSessions()
	return nil
}

// VerifyPassword checks a candidate password in constant time against the
// stored hash. A dummy derivation runs when no password exists so the
// missing-password state does not create a timing oracle.
func (s *Store) VerifyPassword(password string) bool {
	st := s.Load()
	if st.PasswordHash == "" {
		_ = pbkdf2Hash("switcher-dummy", make([]byte, 16), Iterations)
		return false
	}
	salt, err := hex.DecodeString(st.PasswordSalt)
	if err != nil {
		return false
	}
	got := pbkdf2Hash(password, salt, st.Iterations)
	return subtle.ConstantTimeCompare([]byte(got), []byte(st.PasswordHash)) == 1
}

// ---- login rate limiting ----

const maxFailures = 5

// CheckLockout reports whether login attempts are currently locked out.
// The check runs before any PBKDF2 work so the KDF cannot be abused as a
// CPU oracle on an unauthenticated endpoint.
func (s *Store) CheckLockout() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails >= maxFailures && time.Now().Before(s.lockout) {
		remaining := time.Until(s.lockout).Round(time.Second)
		return fmt.Errorf("too many failed attempts; try again in %s", remaining)
	}
	return nil
}

// RecordFailure counts a failed login and escalates the lockout.
func (s *Store) RecordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails++
	backoff := time.Duration(1<<min(s.fails, 6)) * 30 * time.Second
	s.lockout = time.Now().Add(backoff)
}

// resetFailures clears the failure counter after a successful login.
func (s *Store) ResetFailures() {
	s.mu.Lock()
	s.fails = 0
	s.lockout = time.Time{}
	s.mu.Unlock()
}

// ---- sessions ----

type sessionFile struct {
	Sessions map[string]int64 `json:"sessions"` // sha256(token) hex -> expiry unix
}

func (s *Store) sessionsPath() string { return filepath.Join(s.tokenDir, "sessions.json") }

// NewSession mints a random session token, persists only its SHA-256 hash
// with the expiry, and returns the raw token for the cookie.
func (s *Store) NewSession() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256(raw)

	s.mu.Lock()
	sessions := s.loadSessionsLocked()
	sessions[hex.EncodeToString(sum[:])] = time.Now().Add(SessionTTL)
	if err := s.persistSessionsLocked(sessions); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	return token, nil
}

// ValidateSession reports whether a session token is alive, renewing its
// sliding expiry when it is.
func (s *Store) ValidateSession(token string) bool {
	if token == "" {
		return false
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.loadSessionsLocked()
	expiry, ok := sessions[key]
	if !ok || time.Now().After(expiry) {
		if ok {
			delete(sessions, key)
			_ = s.persistSessionsLocked(sessions)
		}
		return false
	}
	sessions[key] = time.Now().Add(SessionTTL)
	_ = s.persistSessionsLocked(sessions)
	return true
}

// SessionCount reports how many live sessions exist.
func (s *Store) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.loadSessionsLocked()
	return len(sessions)
}

// DeleteSession removes one session by its raw token.
func (s *Store) DeleteSession(token string) {
	if token == "" {
		return
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	s.mu.Lock()
	sessions := s.loadSessionsLocked()
	delete(sessions, key)
	_ = s.persistSessionsLocked(sessions)
	s.mu.Unlock()
}

// HasDeviceToken reports whether a local device token file exists.
func (s *Store) HasDeviceToken() bool {
	_, err := s.ReadDeviceToken()
	return err == nil
}

// clearSessions invalidates every session (password change, disable).
func (s *Store) clearSessions() {
	s.mu.Lock()
	s.loadSessionsLocked()
	s.sessions = map[string]time.Time{}
	s.mu.Unlock()
	_ = os.Remove(s.sessionsPath())
}

// CSRFToken returns the CSRF token issued when the password was set.
func (s *Store) CSRFToken() string { return s.Load().CSRFToken }

func (s *Store) loadSessionsLocked() map[string]time.Time {
	if s.sessions != nil {
		return s.sessions
	}
	raw, err := os.ReadFile(s.sessionsPath())
	if err != nil {
		s.sessions = map[string]time.Time{}
		return s.sessions
	}
	var file sessionFile
	if json.Unmarshal(raw, &file) != nil || file.Sessions == nil {
		s.sessions = map[string]time.Time{}
		return s.sessions
	}
	// Drop expired entries on load.
	now := time.Now()
	s.sessions = map[string]time.Time{}
	for key, expiryUnix := range file.Sessions {
		expiry := time.Unix(expiryUnix, 0)
		if now.After(expiry) {
			continue
		}
		s.sessions[key] = expiry
	}
	return s.sessions
}

func (s *Store) persistSessionsLocked(sessions map[string]time.Time) error {
	out := map[string]int64{}
	for key, expiry := range sessions {
		out[key] = expiry.Unix()
	}
	data, err := json.MarshalIndent(sessionFile{Sessions: out}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.sessionsPath(), data)
}

// ---- device token (menu bar app / local scripts) ----

func (s *Store) deviceTokenPath() string { return filepath.Join(s.tokenDir, "local-token") }

// EnsureDeviceToken creates the local device token file if missing and
// returns its value. The file is 0600: the same trust tier as the account
// tokens stored next to it.
func (s *Store) EnsureDeviceToken() (string, error) {
	if raw, err := os.ReadFile(s.deviceTokenPath()); err == nil {
		token := string(raw)
		if len(token) == 64 {
			return token, nil
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := writeAtomic(s.deviceTokenPath(), []byte(token)); err != nil {
		return "", err
	}
	return token, nil
}

// ReadDeviceToken returns the device token value from disk.
func (s *Store) ReadDeviceToken() (string, error) {
	raw, err := os.ReadFile(s.deviceTokenPath())
	if err != nil {
		return "", err
	}
	token := string(raw)
	if len(token) != 64 {
		return "", errors.New("malformed device token")
	}
	return token, nil
}

// RotateDeviceToken replaces the local device token; existing holders stop
// working until they re-read the file.
func (s *Store) RotateDeviceToken() (string, error) {
	if err := os.Remove(s.deviceTokenPath()); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return s.EnsureDeviceToken()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
