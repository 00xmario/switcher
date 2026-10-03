package desktoprelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SetupStatus is safe for management JSON. Admission and original env values
// remain in the private journal and never enter this reply.
type SetupStatus struct {
	Condition       string `json:"condition"`
	SettingsPath    string `json:"settings_path"`
	ScopeID         string `json:"scope_id"`
	BackupPath      string `json:"backup_path,omitempty"`
	RestartRequired bool   `json:"restart_required"`
	Configured      bool   `json:"configured,omitempty"`
	Message         string `json:"message,omitempty"`
}

type SetupError struct {
	Code  string `json:"error_code"`
	cause error
}

func (e *SetupError) ErrorCode() string { return e.Code }
func (e *SetupError) Error() string {
	switch e.Code {
	case "setup_owned":
		return "Restore Desktop setup before disabling the relay or deleting its profile"
	case "setup_changed":
		return "Desktop settings changed; owned values were not overwritten"
	case "setup_invalid_settings":
		return "Desktop settings are invalid or too large"
	case "setup_invalid_path":
		return "An explicit absolute user-owned settings path is required"
	case "setup_lock_busy":
		return "Desktop settings are locked; retry shortly"
	case "setup_busy":
		return "Desktop relay is busy or has reached its profile limit"
	default:
		return "Desktop setup is unavailable; retry after reloading relay state"
	}
}
func (e *SetupError) Unwrap() error             { return e.cause }
func setupError(code string, cause error) error { return &SetupError{Code: code, cause: cause} }

type setupRecord struct {
	SettingsPath   string                     `json:"settings_path"`
	ScopeID        string                     `json:"scope_id"`
	BackupName     string                     `json:"backup_name"`
	FileExisted    bool                       `json:"file_existed"`
	EnvShape       string                     `json:"env_shape"`
	OriginalEnv    map[string]json.RawMessage `json:"original_env"`
	OwnedEnv       map[string]string          `json:"owned_env"`
	Phase          string                     `json:"phase"`
	UpdatedAt      time.Time                  `json:"updated_at"`
	OriginalDigest string                     `json:"original_digest"`
	BeforeDigest   string                     `json:"before_digest"`
	AfterDigest    string                     `json:"after_digest"`
	BeforeEnv      map[string]json.RawMessage `json:"before_env"`
}

func setupPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}

func validSetupRecords(s diskState, dataRoot string) bool {
	if len(s.Setups) > 128 {
		return false
	}
	for path, r := range s.Setups {
		if !setupPath(path) || path != r.SettingsPath || !validUUID(r.ScopeID) || len(r.BackupName) != 69 || !strings.HasSuffix(r.BackupName, ".json") || strings.ContainsAny(r.BackupName, "/\\") || r.UpdatedAt.IsZero() {
			return false
		}
		if strings.Trim(strings.TrimSuffix(r.BackupName, ".json"), "0123456789abcdef") != "" {
			return false
		}
		if r.Phase != "configuring" && r.Phase != "configured" && r.Phase != "restoring" && r.Phase != "restored" {
			return false
		}
		if r.EnvShape != "object" && r.EnvShape != "null" && r.EnvShape != "absent" {
			return false
		}
		for _, digest := range []string{r.OriginalDigest, r.BeforeDigest, r.AfterDigest} {
			if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
				return false
			}
		}
		if r.OriginalEnv == nil || r.BeforeEnv == nil || len(r.OwnedEnv) != 2 {
			return false
		}
		if (r.EnvShape != "object" && len(r.OriginalEnv) != 0) || (!r.FileExisted && r.EnvShape != "absent") {
			return false
		}
		u, err := url.Parse(r.OwnedEnv["HTTPS_PROXY"])
		if err != nil || u.Scheme != "http" || u.User == nil || u.User.Username() != r.ScopeID || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return false
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 || u.Host != "127.0.0.1:"+strconv.Itoa(port) || r.OwnedEnv["NODE_EXTRA_CA_CERTS"] != filepath.Join(dataRoot, "ca.pem") {
			return false
		}
		secret, ok := u.User.Password()
		decoded, err := hex.DecodeString(secret)
		if !ok || err != nil || len(decoded) != 32 {
			return false
		}
		if scope, ok := s.Scopes[r.ScopeID]; ok && scope.Secret != secret {
			return false
		}
		for _, env := range []map[string]json.RawMessage{r.OriginalEnv, r.BeforeEnv} {
			for k, v := range env {
				if (k != setupKeys[0] && k != setupKeys[1]) || !uniqueJSON(v) {
					return false
				}
			}
		}
		if r.Phase != "restored" {
			if _, ok := s.Scopes[r.ScopeID]; !ok {
				return false
			}
		}
	}
	return true
}

func hasManagedSetup(s diskState) bool {
	for _, r := range s.Setups {
		if r.Phase != "restored" {
			return true
		}
	}
	return false
}

func setupLockMutex(ctx context.Context, mu *sync.Mutex) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return setupError("setup_lock_busy", ErrBusy)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Configure starts the relay and merges its two owned env keys in one explicit
// operation. The caller resolves its native path; this module never reads HOME.
func (m *Manager) Configure(ctx context.Context, path string) (SetupStatus, error) {
	err := setupFailure(m.configure(ctx, path))
	return m.SetupStatus(path), err
}

func (m *Manager) configure(ctx context.Context, path string) error {
	if !setupPath(path) {
		return setupError("setup_invalid_path", ErrUnavailable)
	}
	if err := setupLockMutex(ctx, &m.lifecycle); err != nil {
		return err
	}
	defer m.lifecycle.Unlock()
	dir, err := openSetupDirectory(filepath.Dir(path), true)
	if err != nil {
		return setupFailure(err)
	}
	defer dir.Close()
	lease, err := acquireSetupLease(ctx, dir, filepath.Base(path)+".lock")
	if err != nil {
		return err
	}
	defer lease.close()
	before, err := readSetupFile(dir, filepath.Base(path))
	if err != nil {
		return setupFailure(err)
	}
	doc, err := parseSetupDocument(before.data, before.exists)
	if err != nil {
		return err
	}
	if err = m.startLifecycleLocked(ctx, false); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return err
	}
	old, exists := m.state.Setups[path]
	r := old
	if exists && r.Phase != "restored" {
		if err = m.validateSetupBackupLocked(r); err != nil {
			return err
		}
		if r.Phase == "restoring" {
			return setupConflict()
		}
		if !doc.matches(setupStrings(r.OwnedEnv)) && !(r.Phase == "configuring" && doc.matches(r.BeforeEnv)) {
			return setupConflict()
		}
	} else {
		// Manual profiles remain independent. A new managed profile records the
		// entire previous two-key baseline and restores it on explicit removal.
		id := r.ScopeID
		if _, ok := m.state.Scopes[id]; !ok {
			if len(m.state.Scopes) >= 128 || (!exists && len(m.state.Setups) >= 128) {
				return ErrBusy
			}
			id, err = randomID()
			if err != nil {
				return ErrUnavailable
			}
		}
		name, err := randomHex(32)
		if err != nil {
			return ErrUnavailable
		}
		r = setupRecord{SettingsPath: path, ScopeID: id, BackupName: name + ".json", FileExisted: before.exists, EnvShape: doc.shape, OriginalEnv: doc.values(), OriginalDigest: setupDigest(before.data, before.exists)}
		if err = m.backupSetupLocked(r, before.data); err != nil {
			return setupFailure(err)
		}
	}
	scope, scopeExists := m.state.Scopes[r.ScopeID]
	if !scopeExists {
		secret, err := randomHex(32)
		if err != nil {
			return ErrUnavailable
		}
		scope = storedScope{Scope: Scope{ID: r.ScopeID, Label: "Claude Desktop"}, Secret: secret}
	}
	u := &url.URL{Scheme: "http", Host: m.run.listener.Addr().String(), User: url.UserPassword(scope.ID, scope.Secret)}
	r.OwnedEnv = map[string]string{"HTTPS_PROXY": u.String(), "NODE_EXTRA_CA_CERTS": filepath.Join(m.cfg.DataRoot, "ca.pem")}
	if exists && old.Phase == "configured" && doc.matches(setupStrings(r.OwnedEnv)) && before.info.Mode().Perm() == 0600 {
		if err = ctx.Err(); err != nil {
			return err
		}
		current, err := readSetupFile(dir, filepath.Base(path))
		if err != nil || !lease.check() || !setupSameFile(before, current) {
			return setupConflict()
		}
		return nil
	}
	r.BeforeEnv = doc.values()
	r.BeforeDigest = setupDigest(before.data, before.exists)
	after, err := doc.patch(setupStrings(r.OwnedEnv), "object")
	if err != nil {
		return err
	}
	r.AfterDigest = setupDigest(after, true)
	r.Phase = "configuring"
	r.UpdatedAt = time.Now().UTC()
	if err = ctx.Err(); err != nil {
		return err
	}
	current, err := readSetupFile(dir, filepath.Base(path))
	if err != nil || !lease.check() || !setupSameFile(before, current) {
		return setupConflict()
	}
	if m.state.Setups == nil {
		m.state.Setups = make(map[string]setupRecord)
	}
	m.state.Setups[path] = r
	m.state.Scopes[scope.ID] = scope
	if err = m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			if exists {
				m.state.Setups[path] = old
			} else {
				delete(m.state.Setups, path)
			}
			if !scopeExists {
				delete(m.state.Scopes, scope.ID)
			}
		}
		return err
	}
	if err = m.writeSetupFile(ctx, dir, filepath.Base(path), lease, before, after); err != nil {
		return err
	}
	r.Phase = "configured"
	r.UpdatedAt = time.Now().UTC()
	m.state.Setups[path] = r
	if err = m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			r.Phase = "configuring"
			m.state.Setups[path] = r
		}
		return err
	}
	return nil
}

// SetupStatus reads only the supplied settings and relay state. It does not
// acquire native locks, initialize storage, export trust or recover a journal.
func (m *Manager) SetupStatus(path string) SetupStatus {
	s := SetupStatus{Condition: "unavailable", SettingsPath: path}
	if !setupPath(path) {
		s.Message = "An explicit absolute settings path is required"
		return s
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	if m.store == nil {
		var err error
		state, err = m.readSetupState()
		if err != nil {
			s.Message = "Relay setup state cannot be read safely"
			return s
		}
	}
	r, owned := state.Setups[path]
	if owned {
		s.ScopeID = r.ScopeID
		s.BackupPath = filepath.Join(m.cfg.DataRoot, "backups", r.BackupName)
	}
	if owned && m.failed {
		s.Condition = "pending"
		s.RestartRequired = true
		s.Message = "Desktop setup durability is uncertain; reload relay state before retrying"
		return s
	}
	dir, err := openSetupDirectory(filepath.Dir(path), false)
	var current setupFile
	if err == nil {
		current, err = readSetupFile(dir, filepath.Base(path))
		dir.Close()
	}
	if errors.Is(err, errSetupMissing) {
		err = nil
	}
	if err != nil {
		s.Message = "Desktop settings cannot be read safely"
		return s
	}
	doc, err := parseSetupDocument(current.data, current.exists)
	if err != nil {
		s.Message = "Desktop settings are invalid or too large"
		return s
	}
	if !owned || r.Phase == "restored" {
		s.Condition = "not_configured"
		s.RestartRequired = owned
		return s
	}
	s.RestartRequired = true
	if r.Phase == "configuring" || r.Phase == "restoring" {
		s.Condition = "pending"
		s.Message = "Desktop setup needs an explicit retry or restore"
		return s
	}
	if !doc.matches(setupStrings(r.OwnedEnv)) {
		s.Condition = "changed"
		s.Message = "Desktop proxy settings changed after setup"
		return s
	}
	if m.failed || m.run == nil || m.run.ctx.Err() != nil {
		s.Message = "Desktop relay is not listening"
		return s
	}
	if _, ok := state.Scopes[r.ScopeID]; !ok {
		s.Message = "Desktop profile is unavailable"
		return s
	}
	u, err := url.Parse(r.OwnedEnv["HTTPS_PROXY"])
	if err != nil || u.Host != m.run.listener.Addr().String() {
		s.Condition = "changed"
		s.Message = "Desktop relay address changed; run setup again"
		return s
	}
	s.Condition = "configured"
	s.Configured = true
	s.Message = "Restart Claude Desktop, then start a new Code task"
	return s
}

// RestoreSetup restores only the original two keys and their empty env shape.
// It retains the private backup and the scope, allowing already admitted streams
// to finish. The caller can explicitly DeleteScope after restoration.
func (m *Manager) RestoreSetup(ctx context.Context, path string) (SetupStatus, error) {
	err := setupFailure(m.restoreSetup(ctx, path))
	return m.SetupStatus(path), err
}

func (m *Manager) restoreSetup(ctx context.Context, path string) error {
	if !setupPath(path) {
		return setupError("setup_invalid_path", ErrUnavailable)
	}
	if err := setupLockMutex(ctx, &m.lifecycle); err != nil {
		return err
	}
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.initializeLocked(false); err != nil {
		return err
	}
	if m.failed {
		return setupError("setup_unavailable", ErrUnavailable)
	}
	r, ok := m.state.Setups[path]
	if !ok || r.Phase == "restored" {
		return nil
	}
	baseline, err := m.loadSetupBaselineLocked(r)
	if err != nil {
		return err
	}
	// The journal records ownership and presence. The immutable backup also
	// retains original raw formatting across JSON state serialization/reload.
	r.OriginalEnv = baseline.values()
	// Even an externally deleted settings file needs the same lock while we
	// remove ownership. Recreate only private parents for that lock, never the
	// deleted file, so a cooperating native writer cannot race restoration.
	dir, err := openSetupDirectory(filepath.Dir(path), true)
	if err != nil {
		return setupFailure(err)
	}
	defer dir.Close()
	lease, err := acquireSetupLease(ctx, dir, filepath.Base(path)+".lock")
	if err != nil {
		return err
	}
	defer lease.close()
	before, err := readSetupFile(dir, filepath.Base(path))
	if err != nil {
		return setupFailure(err)
	}
	if !before.exists {
		current, err := readSetupFile(dir, filepath.Base(path))
		if err != nil || !lease.check() || !setupSameFile(before, current) {
			return setupConflict()
		}
		return m.markSetupRestoredLocked(ctx, path, r)
	}
	doc, err := parseSetupDocument(before.data, before.exists)
	if err != nil {
		return err
	}
	owned := doc.matches(setupStrings(r.OwnedEnv))
	prewrite := r.Phase == "configuring" && doc.matches(r.BeforeEnv)
	restored := r.Phase == "restoring" && doc.matches(r.OriginalEnv)
	if !owned && !prewrite && !restored {
		return setupConflict()
	}
	old := r
	r.BeforeEnv = doc.values()
	r.BeforeDigest = setupDigest(before.data, true)
	after, err := doc.patch(r.OriginalEnv, r.EnvShape)
	if err != nil {
		return err
	}
	r.AfterDigest = setupDigest(after, true)
	r.Phase = "restoring"
	r.UpdatedAt = time.Now().UTC()
	m.state.Setups[path] = r
	if err = ctx.Err(); err != nil {
		m.state.Setups[path] = old
		return err
	}
	if err = m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Setups[path] = old
		}
		return err
	}
	// A pending restore may have renamed these exact bytes without completing
	// the settings-directory fsync. Re-establish file and directory durability
	// before releasing ownership, even when recovery has no content changes.
	if old.Phase == "restoring" || !bytes.Equal(before.data, after) {
		if err = m.writeSetupFile(ctx, dir, filepath.Base(path), lease, before, after); err != nil {
			return err
		}
	} else if !lease.check() {
		return setupConflict()
	}
	return m.markSetupRestoredLocked(ctx, path, r)
}

func (m *Manager) markSetupRestoredLocked(ctx context.Context, path string, r setupRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old := r
	r.Phase = "restored"
	r.UpdatedAt = time.Now().UTC()
	m.state.Setups[path] = r
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Setups[path] = old
		}
		return err
	}
	return nil
}
