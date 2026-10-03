package claudecode

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"switcher/internal/store"
)

type AccountStore interface {
	List() ([]store.Account, error)
	Get(string) (store.Account, error)
	Save(store.Account) error
}
type ProfileReader func(context.Context, string) (Identity, error)
type Grant func(context.Context, *store.Account) error

type Status struct {
	Available  bool   `json:"available"`
	Condition  string `json:"condition"`
	ActiveID   string `json:"active_id,omitempty"`
	Email      string `json:"email,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Message    string `json:"message,omitempty"`
}
type SwitchResult struct {
	Changed bool   `json:"changed"`
	Backup  string `json:"backup,omitempty"`
	Message string `json:"message"`
}

type Manager struct {
	mu                 sync.Mutex
	statusMu           sync.Mutex
	status             Status
	paths              Paths
	dataRoot           string
	store              AccountStore
	keys               Keychain
	profile            ProfileReader
	grant              Grant
	lockWait           time.Duration
	backend            string
	receiptReader      func(string) (bool, error)
	volatileSuccessors map[string]successor
}

func New(paths Paths, dataRoot string, st AccountStore, keys Keychain, profile ProfileReader, grant Grant) *Manager {
	return &Manager{paths: paths, dataRoot: filepath.Join(dataRoot, "claude-native"), store: st, keys: keys, profile: profile, grant: grant,
		lockWait: 9 * time.Second, volatileSuccessors: map[string]successor{}, status: Status{Available: true, Condition: "checking", ConfigPath: paths.ConfigFile}}
}

// Account files are file-synced by Store.Save. Recovery cleanup must also wait
// for their atomic rename to be durable, even when no sidecar could be written.
func (m *Manager) saveAccount(a store.Account) error {
	if err := m.store.Save(a); err != nil {
		return err
	}
	return syncPrivateDirectory(filepath.Join(filepath.Dir(m.dataRoot), "accounts"))
}

func (m *Manager) Status() Status { m.statusMu.Lock(); defer m.statusMu.Unlock(); return m.status }

func (m *Manager) SetReceiptReader(reader func(string) (bool, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receiptReader = reader
}

func (m *Manager) publish(identity Identity, accounts []store.Account) {
	state := Status{Available: true, Condition: "logged_out", ConfigPath: m.paths.ConfigFile, Backend: "file"}
	if m.backend != "" {
		state.Backend = m.backend
	}
	if identity.UUID != "" {
		state.Condition = "unmanaged"
		state.Email = identity.Email
		for _, a := range accounts {
			if a.Provider == "claude" && owns(identity, a) {
				state.ActiveID = a.ID
				state.Condition = "ready"
				break
			}
		}
	}
	m.statusMu.Lock()
	m.status = state
	m.statusMu.Unlock()
}

func (m *Manager) unavailable(err error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.status.Condition = "unavailable"
	m.status.ActiveID = ""
	m.status.Email = ""
	m.status.Message = err.Error()
}

type snapshot struct {
	Config     fileValue `json:"config"`
	File       fileValue `json:"file"`
	Keychain   fileValue `json:"keychain"`
	ManagedKey fileValue `json:"managed_key"`
}

func (s snapshot) credential() []byte {
	if s.Keychain.Exists {
		return s.Keychain.Data
	}
	return s.File.Data
}

func (m *Manager) readSnapshot(ctx context.Context) (snapshot, error) {
	var s snapshot
	var err error
	if err := checkLocks(ctx); err != nil {
		return s, err
	}
	if s.Config, err = readPrivate(m.paths.ConfigFile); err != nil {
		return s, err
	}
	if _, err = object(s.Config.Data); err != nil {
		return s, err
	}
	if s.File, err = readPrivate(m.paths.CredentialsFile); err != nil {
		return s, err
	}
	if m.paths.Keychain {
		s.Keychain.Data, s.Keychain.Exists, err = m.keys.Read(ctx, m.paths.Service)
		if err != nil {
			return s, err
		}
		if m.paths.Service == "Claude Code-credentials" {
			s.ManagedKey.Data, s.ManagedKey.Exists, err = m.keys.Read(ctx, "Claude Code")
			if err != nil {
				return s, err
			}
		}
	}
	if err := validateSnapshotSize(s); err != nil {
		return s, err
	}
	if len(s.credential()) > 0 {
		if _, err := parseOAuth(s.credential()); err != nil {
			return s, err
		}
	}
	m.backend = "file"
	if s.Keychain.Exists {
		m.backend = "keychain"
	}
	return s, checkLocks(ctx)
}

func validateSnapshotSize(s snapshot) error {
	for _, value := range []fileValue{s.Config, s.File, s.Keychain, s.ManagedKey} {
		if len(value.Data) > nativeFileLimit {
			return errors.New("native Claude store exceeds its bounded storage limit")
		}
	}
	return nil
}

func (m *Manager) inspect(ctx context.Context) (snapshot, error) {
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return snapshot{}, err
	}
	defer unlock()
	return m.readSnapshot(locked)
}

func (m *Manager) owner(ctx context.Context, s snapshot, accounts []store.Account) (Identity, error) {
	_, err := identityOf(s.Config.Data)
	if err != nil {
		return Identity{}, err
	}
	if len(s.credential()) == 0 {
		return Identity{}, nil
	}
	oauth, err := parseOAuth(s.credential())
	if err != nil {
		return Identity{}, err
	}
	if oauth.AccessToken == "" {
		return Identity{}, nil
	}
	for _, a := range accounts {
		if a.Provider != "claude" || !family(oauth, a) {
			continue
		}
		known := accountIdentity(a)
		if known.UUID == "" {
			continue
		}
		// A matching lineage proves token ownership; a disagreeing config
		// must not silently label it as a different account.
		if a.ClaudeCode != nil {
			return known, nil
		}
	}
	if m.profile == nil {
		return Identity{}, identityError()
	}
	resolved, err := m.profile(ctx, oauth.AccessToken)
	if err != nil || resolved.UUID == "" || resolved.Email == "" {
		return Identity{}, identityError()
	}
	return resolved, nil
}

// Capture is read-only toward Claude's profile and Keychain. Identity is
// resolved from the credential itself, then fenced against rotation before
// returning the complete account-specific snapshot to the import handler.
func (m *Manager) Capture(ctx context.Context) (store.Account, error) {
	return m.capture(ctx, nil)
}

func (m *Manager) CaptureAndStore(ctx context.Context, save func(*store.Account) error) (store.Account, error) {
	return m.capture(ctx, save)
}

func (m *Manager) capture(ctx context.Context, save func(*store.Account) error) (store.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoverActiveRefreshes(ctx); err != nil {
		return store.Account{}, err
	}
	s, err := m.inspect(ctx)
	if err != nil {
		return store.Account{}, err
	}
	oauth, err := parseOAuth(s.credential())
	if err != nil || oauth.AccessToken == "" {
		return store.Account{}, errors.New("no native Claude Code OAuth login was found")
	}
	identity, err := m.profile(ctx, oauth.AccessToken)
	if err != nil || identity.UUID == "" || identity.Email == "" {
		return store.Account{}, identityError()
	}
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return store.Account{}, err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return store.Account{}, err
	}
	if !reflect.DeepEqual(current, s) {
		return store.Account{}, ErrConflict
	}
	a := store.Account{Provider: "claude", Email: identity.Email, CreatedAt: time.Now().Unix(), Token: store.Token{AccountID: identity.UUID}}
	if accounts, err := m.store.List(); err == nil {
		matches := []string{}
		for _, stored := range accounts {
			if stored.Provider == "claude" && stored.Token.AccountID == identity.UUID && strings.EqualFold(stored.Email, identity.Email) && family(oauth, stored) {
				matches = append(matches, stored.ID)
			}
		}
		if len(matches) == 1 {
			a.ID = matches[0]
		}
	}
	retainIdentityMetadata(&a, s.Config.Data, identity)
	if err := applyNative(&a, s.credential(), identity); err != nil {
		return store.Account{}, err
	}
	if save != nil {
		if err := checkLocks(locked); err != nil {
			return store.Account{}, err
		}
		if err := save(&a); err != nil {
			return store.Account{}, err
		}
		if err := m.retireSuccessor(a); err != nil {
			return a, errors.New("native login imported but stale recovery metadata could not be archived")
		}
	}
	return a, nil
}

// Missing legacy organization metadata must be verified from that stored
// token, not inferred from whichever organization is currently in Code.
func (m *Manager) prepareLegacy(ctx context.Context, a *store.Account, owner Identity, live []byte) error {
	if a.ClaudeCode != nil {
		return nil
	}
	oauth, _ := parseOAuth(live)
	identity := owner
	if !family(oauth, *a) {
		var err error
		identity, err = m.profile(ctx, a.Token.AccessToken)
		if err != nil {
			return identityError()
		}
	}
	if identity.UUID != a.Token.AccountID || !strings.EqualFold(identity.Email, a.Email) {
		return identityError()
	}
	credential, err := credentialsFor(*a, nil)
	if err != nil {
		return err
	}
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: credential, OAuthAccount: encodedIdentity(identity)}
	return nil
}

func unownedExpiredLegacy(a store.Account, owner Identity, live []byte) bool {
	oauth, _ := parseOAuth(live)
	return a.ClaudeCode == nil && a.Token.ExpiresAt <= time.Now().Unix() && owner.UUID != a.Token.AccountID && !family(oauth, a)
}

// Synchronize adopts only a verified, still-live native generation. The
// caller holds Switcher's account lock and persists the returned account.
func (m *Manager) Synchronize(ctx context.Context, a *store.Account) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkPending(); err != nil {
		m.unavailable(err)
		return false, err
	}
	before, _ := json.Marshal(a)
	if a.ClaudeCodeRefreshPending {
		if err := m.recoverAccountSuccessor(ctx, a); err != nil {
			return false, err
		}
	}
	if _, err := m.recoverNativeRefresh(ctx, a); err != nil {
		return false, err
	}
	s, err := m.inspect(ctx)
	if err != nil {
		m.unavailable(err)
		return false, err
	}
	accounts, err := m.store.List()
	if err != nil {
		return false, err
	}
	owner, err := m.owner(ctx, s, accounts)
	if err != nil {
		m.unavailable(err)
		return false, err
	}
	m.publish(owner, accounts)
	configured, _ := identityOf(s.Config.Data)
	config, _ := object(s.Config.Data)
	if owner.UUID != "" && (!sameIdentity(configured, owner) || s.ManagedKey.Exists || len(config["primaryApiKey"]) > 0) {
		m.unavailable(ErrConflict)
		return false, ErrConflict
	}
	if err := m.prepareLegacy(ctx, a, owner, s.credential()); err != nil {
		if unownedExpiredLegacy(*a, owner, s.credential()) {
			return false, nil
		}
		return false, err
	}
	if !owns(owner, *a) {
		after, _ := json.Marshal(a)
		return !bytes.Equal(before, after), nil
	}
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(current, s) {
		return false, ErrConflict
	}
	if err := m.preserveBeforeAdoption(*a, s.credential()); err != nil {
		return false, err
	}
	retainIdentityMetadata(a, s.Config.Data, owner)
	if err := applyNative(a, s.credential(), owner); err != nil {
		return false, err
	}
	m.publish(owner, []store.Account{*a})
	after, _ := json.Marshal(a)
	return !bytes.Equal(before, after), nil
}

// Refresh is the sole refresh-grant gate once the native bridge is enabled.
// Refresh consumes only a verified current generation under Claude's locks.
// Active successors also update the native stores before those locks release.
func (m *Manager) Refresh(ctx context.Context, a *store.Account, grant Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refresh(ctx, a, grant)
}

// RefreshAfter401 can force only the access-token generation actually rejected
// by the caller. A newer locked native generation is adopted without a POST.
func (m *Manager) RefreshAfter401(ctx context.Context, a *store.Account, rejectedAccessToken string, grant Grant) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rejectedAccessToken == "" {
		return ErrConflict
	}
	return m.refreshGeneration(ctx, a, grant, rejectedAccessToken)
}

type successor struct {
	Account         store.Account  `json:"account"`
	Predecessor     string         `json:"predecessor"`
	RequireIdentity bool           `json:"require_identity,omitempty"`
	Native          *nativeRefresh `json:"native,omitempty"`
}

func (m *Manager) successorPath(id string) string {
	return filepath.Join(m.dataRoot, "successors", id+".json")
}

func (m *Manager) refresh(ctx context.Context, a *store.Account, grant Grant) error {
	return m.refreshGeneration(ctx, a, grant, "")
}

func (m *Manager) refreshGeneration(ctx context.Context, a *store.Account, grant Grant, rejectedAccessToken string) error {
	if err := m.checkPending(); err != nil {
		return err
	}
	advanced, err := m.reloadAccountGeneration(a)
	if err != nil {
		return err
	}
	if a.ClaudeCodeRefreshPending {
		return m.recoverAccountSuccessor(ctx, a)
	}
	if recovered, err := m.recoverNativeRefresh(ctx, a); err != nil || recovered {
		return err
	}
	pre, err := m.inspect(ctx)
	if err != nil {
		return err
	}
	accounts, err := m.store.List()
	if err != nil {
		return err
	}
	owner, err := m.owner(ctx, pre, accounts)
	if err != nil {
		return err
	}
	m.publish(owner, accounts)
	if err := m.prepareLegacy(ctx, a, owner, pre.credential()); err != nil {
		if !unownedExpiredLegacy(*a, owner, pre.credential()) {
			return err
		}
	}
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(pre, current) {
		return ErrConflict
	}
	changed, err := m.reloadAccountGeneration(a)
	if err != nil {
		return err
	}
	advanced = advanced || changed
	if a.ClaudeCodeRefreshPending {
		unlock()
		return m.recoverAccountSuccessor(ctx, a)
	}
	oauth, _ := parseOAuth(current.credential())
	if owns(owner, *a) || family(oauth, *a) {
		if owns(owner, *a) {
			if err := m.preserveBeforeAdoption(*a, current.credential()); err != nil {
				return err
			}
			retainIdentityMetadata(a, current.Config.Data, owner)
			if err := applyNative(a, current.credential(), owner); err != nil {
				return err
			}
			if fresh(*a, time.Minute) && (rejectedAccessToken == "" || a.Token.AccessToken != rejectedAccessToken) {
				return nil
			}
			return m.refreshNativeLocked(locked, a, current, grant)
		}
		return ErrNativeOwned
	}
	if advanced && fresh(*a, time.Minute) {
		return nil
	}
	if rejectedAccessToken != "" && a.Token.AccessToken != rejectedAccessToken {
		return nil
	}
	if saved, ok := m.volatileSuccessors[a.ID]; ok {
		if a.Token.RefreshToken != saved.Predecessor && a.Token.RefreshToken != saved.Account.Token.RefreshToken {
			if err := m.retireSuccessor(*a); err != nil {
				return err
			}
		} else {
			unlock()
			if err := m.verifySuccessor(ctx, &saved); err != nil {
				return err
			}
			saved.Account.AutoUseReset = a.AutoUseReset
			if err := m.saveAccount(saved.Account); err != nil {
				return errors.New("Claude refresh successor is retained in memory; keep Switcher running until storage is repaired")
			}
			*a = saved.Account
			if err := m.clearRefreshRecovery(a.ID); err != nil {
				return err
			}
			return nil
		}
	}
	if raw, err := readRecovery(m.successorPath(a.ID)); err != nil {
		return err
	} else if raw.Exists {
		var saved successor
		if json.Unmarshal(raw.Data, &saved) != nil || saved.Account.ID != a.ID || saved.Account.Token.AccountID != a.Token.AccountID {
			return ErrConflict
		}
		if a.Token.RefreshToken != saved.Predecessor && a.Token.RefreshToken != saved.Account.Token.RefreshToken {
			if err := m.retireSuccessor(*a); err != nil {
				return err
			}
		} else {
			unlock()
			if err := m.verifySuccessor(ctx, &saved); err != nil {
				return err
			}
			saved.Account.AutoUseReset = a.AutoUseReset
			if err := m.saveAccount(saved.Account); err != nil {
				return errors.New("refreshed Claude token is preserved but could not be saved")
			}
			*a = saved.Account
			if err := m.clearRefreshRecovery(a.ID); err != nil {
				return err
			}
			return nil
		}
	}
	// Validate everything used to rebuild the wrapper before consuming the
	// one-time grant. A crash with no successor leaves a durable consume fence.
	if _, err := credentialsFor(*a, nil); err != nil {
		return err
	}
	predecessor := a.Token.RefreshToken
	if err := m.beginRefresh(predecessor, a.ID); err != nil {
		return err
	}
	if err := checkLocks(locked); err != nil {
		return err
	}
	grantCtx, cancel := context.WithTimeout(locked, 8*time.Second)
	defer cancel()
	if err := grant(grantCtx, a); err != nil {
		if errors.Is(err, ErrGrantRejected) {
			_ = writePrivate(m.intentPath(a.ID), fileValue{})
		}
		return err
	}
	copyBytes, _ := json.Marshal(a)
	var retained store.Account
	_ = json.Unmarshal(copyBytes, &retained)
	m.volatileSuccessors[a.ID] = successor{Account: retained, Predecessor: predecessor, RequireIdentity: a.ClaudeCode == nil}
	// Build the wrapper from this account, never copy shared native keys
	// into an unrelated inactive slot during token refresh.
	if a.ClaudeCode != nil {
		credential, err := credentialsFor(*a, a.ClaudeCode.Credentials)
		if err != nil {
			return err
		}
		a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: credential, OAuthAccount: append([]byte(nil), a.ClaudeCode.OAuthAccount...)}
	}
	saved := successor{Account: *a, Predecessor: predecessor, RequireIdentity: a.ClaudeCode == nil}
	m.volatileSuccessors[a.ID] = saved
	raw, _ := json.Marshal(saved)
	if saved.RequireIdentity {
		// Preserve issued bytes before a profile request, then release native
		// mutation locks while resolving the successor's organization.
		if err := writeRecovery(m.successorPath(a.ID), fileValue{Data: raw, Exists: true}); err != nil {
			a.ClaudeCodeRefreshPending = true
			a.ClaudeCodeRefreshRecovery = inactiveRecovery(predecessor)
			if err := m.saveAccount(*a); err != nil {
				return errors.New("Claude refresh succeeded; successor retained in memory and consume fence preserved. Keep Switcher running until storage is repaired")
			}
		}
		unlock()
		if err := m.verifySuccessor(ctx, &saved); err != nil {
			return err
		}
		*a = saved.Account
		m.volatileSuccessors[a.ID] = saved
		raw, _ = json.Marshal(saved)
	}
	if err := writeRecovery(m.successorPath(a.ID), fileValue{Data: raw, Exists: true}); err != nil {
		// Preserve the successor in the existing account store if the sidecar
		// cannot be created, rather than discard a consumed refresh grant.
		a.ClaudeCodeRefreshPending = true
		unlock()
		return m.recoverAccountSuccessor(ctx, a)
	}
	if err := m.saveAccount(*a); err != nil {
		return errors.New("refreshed Claude token was preserved for recovery; account save failed")
	}
	return m.clearRefreshRecovery(a.ID)
}

func (m *Manager) reloadAccountGeneration(a *store.Account) (bool, error) {
	latest, err := m.store.Get(a.ID)
	if err != nil {
		return false, err
	}
	if latest.Provider != a.Provider || latest.Token.AccountID != a.Token.AccountID || !strings.EqualFold(latest.Email, a.Email) ||
		(latest.ClaudeCode != nil && a.ClaudeCode != nil && !sameIdentity(accountIdentity(latest), accountIdentity(*a))) {
		return false, ErrConflict
	}
	if latest.Token.AccessToken == a.Token.AccessToken && latest.Token.RefreshToken == a.Token.RefreshToken {
		return false, nil
	}
	*a = latest
	return true, nil
}

func (m *Manager) verifySuccessor(ctx context.Context, saved *successor) error {
	if !saved.RequireIdentity {
		saved.Account.ClaudeCodeRefreshPending = false
		return nil
	}
	identity, err := m.profile(ctx, saved.Account.Token.AccessToken)
	if err != nil || identity.UUID != saved.Account.Token.AccountID || !strings.EqualFold(identity.Email, saved.Account.Email) {
		return identityError()
	}
	credential, err := credentialsFor(saved.Account, nil)
	if err != nil {
		return err
	}
	saved.Account.ClaudeCode = &store.ClaudeCodeLogin{Credentials: credential, OAuthAccount: encodedIdentity(identity)}
	if identity.Plan != "" {
		saved.Account.Plan = identity.Plan
	}
	saved.RequireIdentity = false
	saved.Account.ClaudeCodeRefreshPending = false
	return nil
}

// A private account marker preserves both the issued bytes and their pending
// verification when sidecar storage failed. Keep that marker durable until
// cleanup completes so a restart never turns recovery into a second grant.
func (m *Manager) recoverAccountSuccessor(ctx context.Context, a *store.Account) error {
	saved := successor{Account: *a, RequireIdentity: a.ClaudeCode == nil}
	if len(a.ClaudeCodeRefreshRecovery) != 0 {
		var inactive inactiveRefresh
		if json.Unmarshal(a.ClaudeCodeRefreshRecovery, &inactive) != nil {
			return ErrConflict
		}
		if !inactive.Inactive {
			var native nativeRefresh
			if json.Unmarshal(a.ClaudeCodeRefreshRecovery, &native) != nil {
				return ErrConflict
			}
			saved.Native = &native
			return m.finishNativeRefresh(ctx, a, saved)
		}
		saved.Predecessor = inactive.Predecessor
	}
	if retained, ok := m.volatileSuccessors[a.ID]; ok && !retained.RequireIdentity &&
		retained.Account.ID == a.ID && retained.Account.Token.AccountID == a.Token.AccountID &&
		strings.EqualFold(retained.Account.Email, a.Email) &&
		retained.Account.Token.AccessToken == a.Token.AccessToken && retained.Account.Token.RefreshToken == a.Token.RefreshToken {
		// Verification may have completed before the durable save failed.
		// Recover that exact generation without requiring the profile again.
		saved = retained
		saved.Account.AutoUseReset = a.AutoUseReset
	}
	if err := m.verifySuccessor(ctx, &saved); err != nil {
		return err
	}
	if repaired, err := m.repairInactiveNative(ctx, a, saved); err != nil || repaired {
		return err
	}
	return m.finishAccountSuccessor(a, saved)
}

func (m *Manager) finishAccountSuccessor(a *store.Account, saved successor) error {
	saved.Account.AutoUseReset = a.AutoUseReset
	saved.Account.ClaudeCodeRefreshPending = true
	saved.Account.ClaudeCodeRefreshRecovery = inactiveRecovery(saved.Predecessor)
	if err := m.saveAccount(saved.Account); err != nil {
		return errors.New("Claude refresh successor could not be saved; keep Switcher running until storage is repaired")
	}
	*a = saved.Account
	if err := m.clearRefreshRecovery(a.ID); err != nil {
		return err
	}
	saved.Account.ClaudeCodeRefreshPending = false
	saved.Account.ClaudeCodeRefreshRecovery = nil
	if err := m.saveAccount(saved.Account); err != nil {
		return err
	}
	*a = saved.Account
	return nil
}

var ErrGrantRejected = errors.New("refresh grant was explicitly rejected before issuance")

func (m *Manager) intentPath(id string) string {
	return filepath.Join(m.dataRoot, "refresh-intents", id+".json")
}
func (m *Manager) beginRefresh(token, id string) error {
	fingerprint := sha256.Sum256([]byte(token))
	value, err := readPrivate(m.intentPath(id))
	if err != nil {
		return err
	}
	if value.Exists {
		return errors.New("a prior Claude refresh has no recovered successor; import a new native login before consuming another grant")
	}
	return writePrivate(m.intentPath(id), fileValue{Data: encodedIdentityIntent(fingerprint), Exists: true})
}
func encodedIdentityIntent(fingerprint [32]byte) []byte {
	raw, _ := json.Marshal(map[string]any{"predecessor_sha256": fmt.Sprintf("%x", fingerprint), "started_at": time.Now().Unix()})
	return raw
}
func (m *Manager) clearRefreshRecovery(id string) error {
	if err := writePrivate(m.intentPath(id), fileValue{}); err != nil {
		return err
	}
	if err := writePrivate(m.successorPath(id), fileValue{}); err != nil {
		return err
	}
	delete(m.volatileSuccessors, id)
	return nil
}

type journal struct {
	Version   int      `json:"version"`
	Paths     Paths    `json:"paths"`
	TargetID  string   `json:"target_id"`
	Before    snapshot `json:"before"`
	After     snapshot `json:"after"`
	Committed bool     `json:"committed"`
	Backup    string   `json:"backup"`
	Receipt   string   `json:"receipt"`
}

func (m *Manager) pendingPath() string { return filepath.Join(m.dataRoot, "pending.json") }
func (m *Manager) checkPending() error {
	value, err := readRecovery(m.pendingPath())
	if err != nil {
		return err
	}
	if value.Exists {
		var saved journal
		if json.Unmarshal(value.Data, &saved) != nil || saved.Version != 1 || saved.Paths != m.paths {
			return ErrConflict
		}
		committed, err := m.wasCommitted(saved)
		if err != nil {
			return err
		}
		if committed {
			return writePrivate(m.pendingPath(), fileValue{})
		}
		return errors.New("a Claude switch was interrupted; use the account switch action to recover it before refreshing tokens")
	}
	return nil
}

func (m *Manager) wasCommitted(saved journal) (bool, error) {
	if saved.Committed {
		return true, nil
	}
	if saved.Receipt != "" && m.receiptReader != nil {
		return m.receiptReader(saved.Receipt)
	}
	return false, nil
}

func validTransition(current, before, after snapshot) bool {
	for _, pair := range []struct{ now, old, next fileValue }{{current.Config, before.Config, after.Config}, {current.File, before.File, after.File}, {current.Keychain, before.Keychain, after.Keychain}, {current.ManagedKey, before.ManagedKey, after.ManagedKey}} {
		if !reflect.DeepEqual(pair.now, pair.old) && !reflect.DeepEqual(pair.now, pair.next) {
			return false
		}
	}
	return true
}

func (m *Manager) writeSnapshot(ctx context.Context, s snapshot) error {
	if err := validateSnapshotSize(s); err != nil {
		return err
	}
	if err := checkLocks(ctx); err != nil {
		return err
	}
	if m.paths.Keychain {
		if s.Keychain.Exists {
			if err := m.keys.Write(ctx, m.paths.Service, s.Keychain.Data); err != nil {
				return err
			}
		} else if err := m.keys.Delete(ctx, m.paths.Service); err != nil {
			return err
		}
		if m.paths.Service == "Claude Code-credentials" {
			if err := checkLocks(ctx); err != nil {
				return err
			}
			if s.ManagedKey.Exists {
				if err := m.keys.Write(ctx, "Claude Code", s.ManagedKey.Data); err != nil {
					return err
				}
			} else if err := m.keys.Delete(ctx, "Claude Code"); err != nil {
				return err
			}
		}
	}
	if err := writeLocked(ctx, m.paths.CredentialsFile, s.File); err != nil {
		return err
	}
	return writeLocked(ctx, m.paths.ConfigFile, s.Config)
}

func (m *Manager) recover(ctx context.Context) error {
	value, err := readRecovery(m.pendingPath())
	if err != nil || !value.Exists {
		return err
	}
	var saved journal
	if json.Unmarshal(value.Data, &saved) != nil || saved.Version != 1 || saved.Paths != m.paths {
		return ErrConflict
	}
	committed, err := m.wasCommitted(saved)
	if err != nil {
		return err
	}
	if committed {
		return writePrivate(m.pendingPath(), fileValue{})
	}
	current, err := m.readSnapshot(ctx)
	if err != nil {
		return err
	}
	if !validTransition(current, saved.Before, saved.After) {
		return errors.New("native Claude state changed after an interrupted switch; backup retained for manual recovery")
	}
	if err := m.writeSnapshot(ctx, saved.Before); err != nil {
		return errors.New("Claude rollback could not complete; backup and recovery journal retained")
	}
	return writePrivate(m.pendingPath(), fileValue{})
}

// Switch changes Claude Code's actual login, and calls commit only after
// native writes verify. Caller serializes every Claude account record with
// Switcher's token refreshes so outgoing capture cannot revert another save.
func (m *Manager) Switch(ctx context.Context, id string, commit func() error) (SwitchResult, error) {
	return m.SwitchWithReceipt(ctx, id, func(string) error { return commit() })
}

func (m *Manager) SwitchWithReceipt(ctx context.Context, id string, commit func(string) error) (SwitchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := SwitchResult{}
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return result, err
	}
	err = m.recover(locked)
	unlock()
	if err != nil {
		return result, err
	}
	if err := m.recoverActiveRefreshes(ctx); err != nil {
		return result, err
	}
	initial, err := m.inspect(ctx)
	if err != nil {
		return result, err
	}
	accounts, err := m.store.List()
	if err != nil {
		return result, err
	}
	owner, err := m.owner(ctx, initial, accounts)
	if err != nil {
		return result, err
	}
	target, err := m.store.Get(id)
	if err != nil {
		return result, err
	}
	if target.Provider != "claude" || target.Token.AccountID == "" {
		return result, identityError()
	}
	if target.ClaudeCodeRefreshPending {
		if err := m.refresh(ctx, &target, m.grant); err != nil {
			return result, err
		}
	}
	if err := m.prepareLegacy(ctx, &target, owner, initial.credential()); err != nil {
		if !unownedExpiredLegacy(target, owner, initial.credential()) {
			return result, err
		}
		if err := m.refresh(ctx, &target, m.grant); err != nil {
			return result, err
		}
	}
	configured, _ := identityOf(initial.Config.Data)
	configBefore, _ := object(initial.Config.Data)
	if owns(owner, target) && sameIdentity(configured, owner) && !initial.ManagedKey.Exists && len(configBefore["primaryApiKey"]) == 0 {
		locked, unlock, err := m.locks(ctx)
		if err != nil {
			return result, err
		}
		defer unlock()
		current, err := m.readSnapshot(locked)
		if err != nil {
			return result, err
		}
		if !reflect.DeepEqual(current, initial) {
			return result, ErrConflict
		}
		if err := m.preserveBeforeAdoption(target, current.credential()); err != nil {
			return result, err
		}
		retainIdentityMetadata(&target, current.Config.Data, owner)
		if err := applyNative(&target, current.credential(), owner); err != nil {
			return result, err
		}
		if err := checkLocks(locked); err != nil {
			return result, err
		}
		if err := m.saveAccount(target); err != nil {
			return result, err
		}
		if err := checkLocks(locked); err != nil {
			return result, err
		}
		if err := commit(""); err != nil {
			return result, err
		}
		m.publish(owner, []store.Account{target})
		result.Message = "Claude Code already uses this login. Reopen Code to bypass its credential cache."
		return result, nil
	}
	if owns(owner, target) {
		locked, unlock, err := m.locks(ctx)
		if err != nil {
			return result, err
		}
		current, err := m.readSnapshot(locked)
		if err != nil {
			unlock()
			return result, err
		}
		if !reflect.DeepEqual(current, initial) {
			unlock()
			return result, ErrConflict
		}
		if err := m.preserveBeforeAdoption(target, current.credential()); err != nil {
			unlock()
			return result, err
		}
		retainIdentityMetadata(&target, current.Config.Data, owner)
		if err := applyNative(&target, current.credential(), owner); err != nil {
			unlock()
			return result, err
		}
		if err = checkLocks(locked); err == nil {
			err = m.saveAccount(target)
		}
		unlock()
		if err != nil {
			return result, err
		}
	}
	if !fresh(target, 10*time.Minute) {
		if err := m.refresh(ctx, &target, m.grant); err != nil {
			return result, err
		}
	}
	if !fresh(target, time.Minute) {
		return result, errors.New("Claude target token is not fresh enough; relogin before switching")
	}
	// Older Switcher accounts have only OAuth tokens. Resolve full native
	// identity before taking mutation locks, then revalidate exact live bytes.
	resolved, err := m.profile(ctx, target.Token.AccessToken)
	if err != nil || resolved.UUID != target.Token.AccountID || !bytes.EqualFold([]byte(resolved.Email), []byte(target.Email)) {
		return result, identityError()
	}
	if target.ClaudeCode != nil && !sameIdentity(accountIdentity(target), resolved) {
		return result, identityError()
	}
	identityData, err := identityRecord(target, resolved)
	if err != nil {
		return result, err
	}
	credential, err := credentialsFor(target, initial.credential())
	if err != nil {
		return result, err
	}
	config, err := object(initial.Config.Data)
	if err != nil {
		return result, err
	}
	config["oauthAccount"] = identityData
	delete(config, "primaryApiKey")
	configData, _ := json.MarshalIndent(config, "", "  ")
	next := initial
	next.Config = fileValue{Data: configData, Exists: true}
	next.ManagedKey = fileValue{}
	if m.paths.Keychain {
		next.Keychain = fileValue{Data: credential, Exists: true}
		if initial.File.Exists {
			next.File = fileValue{Data: credential, Exists: true}
		}
	} else {
		next.File = fileValue{Data: credential, Exists: true}
	}
	if err := validateSnapshotSize(next); err != nil {
		return result, err
	}
	locked, unlock, err = m.locks(ctx)
	if err != nil {
		return result, err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(current, initial) {
		return result, ErrConflict
	}
	// Capture the outgoing native generation only into its verified owner.
	for _, a := range accounts {
		if a.Provider == "claude" && owns(owner, a) {
			if err := m.preserveBeforeAdoption(a, current.credential()); err != nil {
				return result, err
			}
			retainIdentityMetadata(&a, current.Config.Data, owner)
			if err := applyNative(&a, current.credential(), owner); err != nil {
				return result, err
			}
			if err := checkLocks(locked); err != nil {
				return result, err
			}
			if err := m.saveAccount(a); err != nil {
				return result, err
			}
			break
		}
	}
	backupDir := filepath.Join(m.dataRoot, "backups")
	if err := safeDirectory(backupDir, true); err != nil {
		return result, err
	}
	backup, err := os.MkdirTemp(backupDir, time.Now().Format("20060102-150405")+"-")
	if err != nil {
		return result, err
	}
	result.Backup = backup
	saved := journal{Version: 1, Paths: m.paths, TargetID: id, Before: initial, After: next, Backup: backup, Receipt: rand.Text()}
	raw, _ := json.Marshal(saved)
	if err := writeRecovery(filepath.Join(backup, "snapshot.json"), fileValue{Data: raw, Exists: true}); err != nil {
		return result, err
	}
	if err := writeRecovery(m.pendingPath(), fileValue{Data: raw, Exists: true}); err != nil {
		return result, err
	}
	if err := m.writeSnapshot(locked, next); err != nil {
		return result, m.rollback(locked, saved, err)
	}
	verified, err := m.readSnapshot(locked)
	if err != nil || !reflect.DeepEqual(verified, next) {
		return result, m.rollback(locked, saved, errors.New("native Claude writes did not verify"))
	}
	// Save account-owned target wrapper separately from live shared state.
	ownCredential, err := credentialsFor(target, nil)
	if err != nil {
		return result, m.rollback(locked, saved, err)
	}
	target.ClaudeCode = &store.ClaudeCodeLogin{Credentials: ownCredential, OAuthAccount: identityData}
	if err := checkLocks(locked); err != nil {
		return result, err
	}
	if err := m.saveAccount(target); err != nil {
		return result, m.rollback(locked, saved, err)
	}
	if err := checkLocks(locked); err != nil {
		return result, err
	}
	if err := commit(saved.Receipt); err != nil {
		return result, m.rollback(locked, saved, err)
	}
	saved.Committed = true
	raw, _ = json.Marshal(saved)
	if err := writeRecovery(m.pendingPath(), fileValue{Data: raw, Exists: true}); err != nil {
		result.Message = "Claude Code login switched; recovery metadata cleanup is pending"
	} else {
		_ = writePrivate(m.pendingPath(), fileValue{})
	}
	m.publish(resolved, []store.Account{target})
	result.Changed = true
	if result.Message == "" {
		result.Message = "Claude Code login switched. Running sessions may take about 30 seconds to notice; reopen Code to apply immediately."
	}
	return result, nil
}

// Explicit re-login supersedes this recovery record. Archive the old bytes
// after account persistence, without allowing them to block the new login.
func (m *Manager) RetireSuccessor(a store.Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.retireSuccessor(a)
}
func (m *Manager) retireSuccessor(a store.Account) error {
	current, err := m.store.Get(a.ID)
	if err != nil {
		return err
	}
	if current.Token.RefreshToken != a.Token.RefreshToken {
		return ErrConflict
	}
	if err := m.verifyRetirementGeneration(a); err != nil {
		return err
	}
	if saved, ok := m.volatileSuccessors[a.ID]; ok {
		raw, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		archive := filepath.Join(m.dataRoot, "generations", a.ID+"-retired-volatile-refresh-"+rand.Text()+".json")
		if err := writeRecovery(archive, fileValue{Data: raw, Exists: true}); err != nil {
			return err
		}
	}
	for _, path := range []string{m.successorPath(a.ID), m.intentPath(a.ID)} {
		value, err := readRecovery(path)
		if err != nil {
			return err
		}
		if !value.Exists {
			continue
		}
		archive := filepath.Join(m.dataRoot, "generations", a.ID+"-retired-refresh-"+rand.Text()+".json")
		if err := writeRecovery(archive, value); err != nil {
			return err
		}
		if err := writePrivate(path, fileValue{}); err != nil {
			return err
		}
	}
	delete(m.volatileSuccessors, a.ID)
	return nil
}

// A verified identity is not proof that two refresh generations have the
// same freshness. Keep the stored generation before adopting different live
// bytes, particularly when migrating from Switcher's old independent copies.
func (m *Manager) preserveBeforeAdoption(a store.Account, live []byte) error {
	oauth, err := parseOAuth(live)
	if err != nil {
		return err
	}
	if oauth.RefreshToken == a.Token.RefreshToken {
		return nil
	}
	return m.BackupAccount(a)
}

func (m *Manager) BackupAccount(a store.Account) error {
	if a.ID == "" || len(a.ID) > 64 || strings.ContainsAny(a.ID, "/\\.") {
		return errors.New("invalid Claude account snapshot ID")
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	path := filepath.Join(m.dataRoot, "generations", a.ID+"-"+time.Now().Format("20060102-150405")+"-"+rand.Text()+".json")
	return writeRecovery(path, fileValue{Data: raw, Exists: true})
}

// Keep legacy IDs during metadata backfill without conflating organizations.
func (m *Manager) ExistingID(identity Identity) string {
	accounts, err := m.store.List()
	if err != nil {
		return ""
	}
	legacy := []string{}
	for _, a := range accounts {
		if a.Provider != "claude" || !owns(identity, a) {
			continue
		}
		if a.ClaudeCode != nil {
			return a.ID
		}
		legacy = append(legacy, a.ID)
	}
	if len(legacy) == 1 {
		return legacy[0]
	}
	return ""
}

func (m *Manager) rollback(locked context.Context, saved journal, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(locked), 20*time.Second)
	defer cancel()
	current, err := m.readSnapshot(ctx)
	if err == nil && reflect.DeepEqual(current, saved.Before) {
		_ = writePrivate(m.pendingPath(), fileValue{})
		return errors.Join(errors.New("Claude switch aborted before changing the native login"), cause)
	}
	if err != nil || !validTransition(current, saved.Before, saved.After) {
		return errors.New("Claude switch failed and live state changed; backup and recovery journal retained")
	}
	if err := m.writeSnapshot(ctx, saved.Before); err != nil {
		return errors.New("Claude switch failed and rollback was incomplete; backup and journal retained")
	}
	_ = writePrivate(m.pendingPath(), fileValue{})
	return errors.Join(errors.New("Claude switch failed; native login was rolled back"), cause)
}
