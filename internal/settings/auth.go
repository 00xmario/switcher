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

var ErrWrongPassword = errors.New("wrong password")
var ErrCredentialsChanged = errors.New("credentials changed; authenticate again")
var ErrPasswordAlreadySet = errors.New("a password is already set; provide the current password")

// PasswordProof binds a successful verification to one credential generation.
// Its contents are private so callers cannot construct a verification result.
type PasswordProof struct{ generation string }

func credentialGeneration(st Settings) string {
	raw, _ := json.Marshal(struct {
		Enabled          bool
		Salt, Hash, CSRF string
		Iterations       int
	}{st.AuthEnabled, st.PasswordSalt, st.PasswordHash, st.CSRFToken, st.Iterations})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func passwordSettings(password string) (Settings, error) {
	if len(password) < 8 {
		return Settings{}, errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return Settings{}, err
	}
	hash := pbkdf2Hash(password, salt, Iterations)
	csrf := make([]byte, 32)
	if _, err := rand.Read(csrf); err != nil {
		return Settings{}, err
	}
	return Settings{AuthEnabled: true, PasswordSalt: hex.EncodeToString(salt), PasswordHash: hash,
		Iterations: Iterations, CSRFToken: hex.EncodeToString(csrf)}, nil
}

func (s *Store) setPasswordLocked(st, password Settings) error {
	st.AuthEnabled, st.PasswordSalt, st.PasswordHash = true, password.PasswordSalt, password.PasswordHash
	st.Iterations, st.CSRFToken = password.Iterations, password.CSRFToken
	if err := s.save(st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]time.Time{}
	s.sessionGeneration = credentialGeneration(st)
	s.sessionDeletionRetries = nil
	return s.persistSessionsLocked(st, s.sessions)
}

// SetPassword performs initial setup only. It cannot overwrite a password
// installed by another setup request while the new hash was being derived.
func (s *Store) SetPassword(password string) error {
	next, err := passwordSettings(password)
	if err != nil {
		return err
	}
	s.settingsMu.Lock()
	st, err := s.load()
	if err == nil && st.PasswordHash != "" {
		err = ErrPasswordAlreadySet
	}
	if err == nil {
		err = s.setPasswordLocked(st, next)
	}
	s.settingsMu.Unlock()
	if err != nil {
		return err
	}
	s.ResetFailures()
	return nil
}

// ChangePassword verifies the current password (when one exists) and stores
// the new one, invalidating every session.
func (s *Store) ChangePassword(current, next string) error {
	st, err := s.Snapshot()
	if err != nil {
		return err
	}
	if st.PasswordHash == "" {
		return s.SetPassword(next)
	}
	if !verifyStoredPassword(st, current) {
		return ErrWrongPassword
	}
	password, err := passwordSettings(next)
	if err != nil {
		return err
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	latest, err := s.load()
	if err != nil {
		return err
	}
	if credentialGeneration(st) != credentialGeneration(latest) {
		return ErrCredentialsChanged
	}
	return s.setPasswordLocked(latest, password)
}

// VerifyPassword checks a candidate password in constant time against the
// stored hash. A dummy derivation runs when no password exists so the
// missing-password state does not create a timing oracle.
func (s *Store) VerifyPassword(password string) bool {
	_, ok := s.VerifyPasswordForSession(password)
	return ok
}

func (s *Store) VerifyPasswordForSession(password string) (PasswordProof, bool) {
	st, err := s.Snapshot()
	if err != nil {
		return PasswordProof{}, false
	}
	verified := verifyStoredPassword(st, password)
	if !st.AuthEnabled || !verified {
		return PasswordProof{}, false
	}
	return PasswordProof{credentialGeneration(st)}, true
}

func verifyStoredPassword(st Settings, password string) bool {
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

// DisableAuthWithPassword checks the verified generation again under the
// mutation lock, so a concurrently rotated password cannot authorize disable.
func (s *Store) DisableAuthWithPassword(password string) error {
	proof, ok := s.VerifyPasswordForSession(password)
	if !ok {
		return ErrWrongPassword
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	if credentialGeneration(st) != proof.generation {
		return ErrCredentialsChanged
	}
	return s.disableAuthLocked(st)
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
	Sessions   map[string]int64 `json:"sessions"` // sha256(token) hex -> expiry unix
	Generation string           `json:"credential_generation"`
}

type sessionDeletionRetry struct {
	generation string
	expiresAt  time.Time
}

func (s *Store) sessionsPath() string { return filepath.Join(s.tokenDir, "sessions.json") }

// NewSession mints a random session token, persists only its SHA-256 hash
// with the expiry, and returns the raw token for the cookie.
func (s *Store) NewSession() (string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return "", err
	}
	return s.newSessionLocked(st)
}

// NewVerifiedSession atomically checks the password generation and persists
// the session. The returned CSRF token belongs to that same generation.
func (s *Store) NewVerifiedSession(proof PasswordProof) (string, string, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return "", "", err
	}
	if !st.AuthEnabled || st.PasswordHash == "" || proof.generation != credentialGeneration(st) {
		return "", "", ErrCredentialsChanged
	}
	token, err := s.newSessionLocked(st)
	return token, st.CSRFToken, err
}

func (s *Store) newSessionLocked(st Settings) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.loadSessionsLocked(st)
	sessions[hex.EncodeToString(sum[:])] = time.Now().Add(SessionTTL)
	if err := s.persistSessionsLocked(st, sessions); err != nil {
		delete(sessions, hex.EncodeToString(sum[:]))
		return "", err
	}
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
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.loadSessionsLocked(st)
	expiry, ok := sessions[key]
	if !ok || time.Now().After(expiry) {
		if ok {
			delete(sessions, key)
			_ = s.persistSessionsLocked(st, sessions)
		}
		return false
	}
	sessions[key] = time.Now().Add(SessionTTL)
	_ = s.persistSessionsLocked(st, sessions)
	return true
}

// SessionCount reports how many live sessions exist.
func (s *Store) SessionCount() int {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := s.loadSessionsLocked(st)
	return len(sessions)
}

// DeleteSession revokes a session in memory even if persistence fails. Every
// call persists the deletion, including retries after the map entry is gone.
func (s *Store) DeleteSession(token string) error {
	if token == "" {
		return nil
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return nil
	}
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	sessions, generation := s.sessions, s.sessionGeneration
	if err == nil {
		sessions, generation = s.loadSessionsLocked(st), credentialGeneration(st)
	}
	if expiry, ok := sessions[key]; ok && time.Now().Before(expiry) {
		if s.sessionDeletionRetries == nil {
			s.sessionDeletionRetries = map[string]sessionDeletionRetry{}
		}
		s.sessionDeletionRetries[key] = sessionDeletionRetry{generation: generation, expiresAt: expiry}
	}
	delete(sessions, key)
	if err != nil {
		return err
	}
	if err := s.persistSessionsLocked(st, sessions); err != nil {
		return err
	}
	delete(s.sessionDeletionRetries, key)
	return nil
}

// CanRetrySessionDeletion verifies an unexpired, generation-bound failed
// deletion or disk record. It only authorizes CSRF-protected logout retries,
// never access to other authenticated routes. A retained deletion proof also
// permits retry when rename succeeded but its durability check failed.
func (s *Store) CanRetrySessionDeletion(token string) bool {
	if token == "" {
		return false
	}
	raw, err := hex.DecodeString(token)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(raw)
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil || !st.AuthEnabled {
		return false
	}
	key, generation := hex.EncodeToString(sum[:]), credentialGeneration(st)
	s.mu.Lock()
	retry, pending := s.sessionDeletionRetries[key]
	if pending && (retry.generation != generation || !time.Now().Before(retry.expiresAt)) {
		delete(s.sessionDeletionRetries, key)
		pending = false
	}
	s.mu.Unlock()
	if pending {
		return true
	}
	data, err := readPrivateFile(s.sessionsPath())
	if err != nil {
		return false
	}
	var file sessionFile
	if json.Unmarshal(data, &file) != nil || file.Generation != generation {
		return false
	}
	expiry, ok := file.Sessions[key]
	return ok && time.Now().Before(time.Unix(expiry, 0))
}

// DeleteAllSessions clears every session: the in-memory map and the
// persisted sessions file. Every signed-in browser or device is logged out.
// A missing sessions file is fine: there was nothing to clear.
func (s *Store) DeleteAllSessions() error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.load()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]time.Time{}
	s.sessionGeneration = credentialGeneration(st)
	// Persist an empty file inside the same critical section so a
	// concurrent NewSession cannot leave a session alive in memory but
	// gone on disk.
	return s.persistSessionsLocked(st, s.sessions)
}

// HasDeviceToken reports whether a local device token file exists.
func (s *Store) HasDeviceToken() bool {
	_, err := s.ReadDeviceToken()
	return err == nil
}

// CSRFToken returns the CSRF token issued when the password was set.
func (s *Store) CSRFToken() string { return s.Load().CSRFToken }

func (s *Store) loadSessionsLocked(st Settings) map[string]time.Time {
	generation := credentialGeneration(st)
	if s.sessions != nil && s.sessionGeneration == generation {
		return s.sessions
	}
	s.sessionGeneration = generation
	raw, err := readPrivateFile(s.sessionsPath())
	if err != nil {
		s.sessions = map[string]time.Time{}
		return s.sessions
	}
	var file sessionFile
	if json.Unmarshal(raw, &file) != nil || file.Sessions == nil || file.Generation != generation {
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

func (s *Store) persistSessionsLocked(st Settings, sessions map[string]time.Time) error {
	out := map[string]int64{}
	for key, expiry := range sessions {
		out[key] = expiry.Unix()
	}
	data, err := json.MarshalIndent(sessionFile{Sessions: out, Generation: credentialGeneration(st)}, "", "  ")
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
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if token, err := s.readDeviceToken(); err == nil {
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return s.newDeviceToken()
}

func (s *Store) newDeviceToken() (string, error) {
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
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	return s.readDeviceToken()
}

func (s *Store) readDeviceToken() (string, error) {
	raw, err := readPrivateFile(s.deviceTokenPath())
	if err != nil {
		return "", err
	}
	token := string(raw)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return "", errors.New("malformed device token")
	}
	return token, nil
}

// RotateDeviceToken replaces the local device token; existing holders stop
// working until they re-read the file.
func (s *Store) RotateDeviceToken() (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	return s.newDeviceToken()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
