package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"switcher/internal/store"
)

// The preimage and resolved profile keep recovery tied to the exact native
// stores that held the grant. They are secret-bearing private recovery data.
type nativeRefresh struct {
	Paths  Paths    `json:"paths"`
	Before snapshot `json:"before"`
}

type inactiveRefresh struct {
	Inactive    bool   `json:"inactive"`
	Predecessor string `json:"predecessor"`
}

func inactiveRecovery(predecessor string) []byte {
	raw, _ := json.Marshal(inactiveRefresh{Inactive: true, Predecessor: predecessor})
	return raw
}

// Caller holds both the account mutex and Claude's credential/config locks.
// Never consume the saved backup here: a was adopted from the locked live read.
func (m *Manager) refreshNativeLocked(ctx context.Context, a *store.Account, current snapshot, grant Grant) error {
	configured, err := identityOf(current.Config.Data)
	config, _ := object(current.Config.Data)
	if err != nil || !sameIdentity(configured, accountIdentity(*a)) || current.ManagedKey.Exists || len(config["primaryApiKey"]) != 0 {
		return ErrConflict
	}
	prepared, err := credentialsFor(*a, current.credential())
	if err != nil {
		return err
	}
	if m.paths.Keychain {
		if validator, ok := m.keys.(interface{ Validate(string, []byte) error }); ok {
			if err := validator.Validate(m.paths.Service, prepared); err != nil {
				return err
			}
		}
	}
	predecessor := a.Token.RefreshToken
	if err := checkLocks(ctx); err != nil {
		return err
	}
	if err := m.beginRefresh(predecessor, a.ID); err != nil {
		return err
	}
	if err := checkLocks(ctx); err != nil {
		return err
	}
	grantCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := grant(grantCtx, a); err != nil {
		if errors.Is(err, ErrGrantRejected) {
			_ = writePrivate(m.intentPath(a.ID), fileValue{})
		}
		return err
	}
	saved := successor{Account: *a, Predecessor: predecessor, Native: &nativeRefresh{Paths: m.paths, Before: current}}
	m.volatileSuccessors[a.ID] = saved
	credential, err := credentialsFor(*a, current.credential())
	if err != nil {
		return err
	}
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: credential, OAuthAccount: append([]byte(nil), a.ClaudeCode.OAuthAccount...)}
	saved.Account = *a
	m.volatileSuccessors[a.ID] = saved
	raw, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	if err := writeRecovery(m.successorPath(a.ID), fileValue{Data: raw, Exists: true}); err != nil {
		// The account file is a second durable store. Its marker and preimage
		// survive restart even when the sidecar directory cannot be written.
		pending := *a
		pending.ClaudeCodeRefreshPending = true
		pending.ClaudeCodeRefreshRecovery, _ = json.Marshal(saved.Native)
		if err := m.saveAccount(pending); err != nil {
			return errors.New("Claude native refresh succeeded; successor retained in memory and consume fence preserved. Keep Switcher running until storage is repaired")
		}
		*a = pending
	}
	observed, err := m.readSnapshot(ctx)
	if err != nil {
		return err
	}
	return m.finishNativeRefreshLocked(ctx, a, saved, observed)
}

// Recovery must precede owner detection and live adoption: the native store
// may still hold a consumed predecessor while the account/sidecar holds its
// successor. Copying that preimage back into the account would lose the grant.
func (m *Manager) recoverNativeRefresh(ctx context.Context, a *store.Account) (bool, error) {
	saved, ok := m.volatileSuccessors[a.ID]
	if !ok {
		raw, err := readRecovery(m.successorPath(a.ID))
		if err != nil {
			return false, err
		}
		if !raw.Exists {
			return false, nil
		}
		if json.Unmarshal(raw.Data, &saved) != nil {
			return false, ErrConflict
		}
	}
	if saved.Account.ID != a.ID || saved.Account.Token.AccountID != a.Token.AccountID || !strings.EqualFold(saved.Account.Email, a.Email) ||
		(saved.Account.ClaudeCode != nil && a.ClaudeCode != nil && !sameIdentity(accountIdentity(saved.Account), accountIdentity(*a))) {
		return false, ErrConflict
	}
	if a.Token.RefreshToken != saved.Predecessor && a.Token.RefreshToken != saved.Account.Token.RefreshToken {
		// A persisted explicit re-login supersedes recovery, but its issued
		// bytes still need archival before this slot may consume another grant.
		return false, m.retireSuccessor(*a)
	}
	if saved.Native == nil {
		if err := m.verifySuccessor(ctx, &saved); err != nil {
			return true, err
		}
		saved.Account.AutoUseReset = a.AutoUseReset
		if repaired, err := m.repairInactiveNative(ctx, a, saved); err != nil || repaired {
			return true, err
		}
		m.volatileSuccessors[a.ID] = saved
		return true, m.finishAccountSuccessor(a, saved)
	}
	return true, m.finishNativeRefresh(ctx, a, saved)
}

func (m *Manager) repairInactiveNative(ctx context.Context, a *store.Account, saved successor) (bool, error) {
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return false, err
	}
	oauth, _ := parseOAuth(current.credential())
	if oauth.RefreshToken == "" {
		return false, nil
	}
	if saved.Predecessor == "" {
		// Account-only fallback retains the predecessor's fingerprint in the
		// durable intent, even when no sidecar or process memory survives.
		value, err := readPrivate(m.intentPath(a.ID))
		if err != nil {
			return false, err
		}
		var intent struct {
			Predecessor string `json:"predecessor_sha256"`
		}
		if !value.Exists {
			return false, nil
		}
		if json.Unmarshal(value.Data, &intent) != nil || intent.Predecessor == "" {
			return false, ErrConflict
		}
		fingerprint := sha256.Sum256([]byte(oauth.RefreshToken))
		if intent.Predecessor != fmt.Sprintf("%x", fingerprint) {
			return false, nil
		}
		saved.Predecessor = oauth.RefreshToken
	}
	if oauth.RefreshToken != saved.Predecessor {
		return false, nil
	}
	// An external activation can put an inactive consumed predecessor back
	// in Code. Retain its native preimage before repairing that generation.
	saved.Native = &nativeRefresh{Paths: m.paths, Before: current}
	m.volatileSuccessors[a.ID] = saved
	raw, err := json.Marshal(saved)
	if err != nil {
		return true, err
	}
	if err := writeRecovery(m.successorPath(a.ID), fileValue{Data: raw, Exists: true}); err != nil {
		pending := saved.Account
		pending.ClaudeCodeRefreshPending = true
		pending.ClaudeCodeRefreshRecovery, _ = json.Marshal(saved.Native)
		if err := m.saveAccount(pending); err != nil {
			return true, err
		}
		*a = pending
	}
	return true, m.finishNativeRefreshLocked(locked, a, saved, current)
}

// Import and manual switching can otherwise capture a consumed native
// predecessor and retire its valid successor. Repair before either capture.
func (m *Manager) recoverActiveRefreshes(ctx context.Context) error {
	accounts, err := m.store.List()
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if a.Provider != "claude" {
			continue
		}
		if a.ClaudeCodeRefreshPending || len(a.ClaudeCodeRefreshRecovery) != 0 {
			if err := m.recoverAccountSuccessor(ctx, &a); err != nil {
				return err
			}
		} else if _, err := m.recoverNativeRefresh(ctx, &a); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) verifyRetirementGeneration(a store.Account) error {
	raw, err := readPrivate(m.intentPath(a.ID))
	if err != nil || !raw.Exists {
		return err
	}
	var intent struct {
		Predecessor string `json:"predecessor_sha256"`
	}
	if json.Unmarshal(raw.Data, &intent) != nil || intent.Predecessor == "" {
		return ErrConflict
	}
	fingerprint := sha256.Sum256([]byte(a.Token.RefreshToken))
	if intent.Predecessor != fmt.Sprintf("%x", fingerprint) {
		return nil
	}
	// A repeated import is not a new login. Only received successor bytes
	// prove this same refresh-token value can supersede an unresolved fence.
	matches := func(saved successor) bool {
		return saved.Account.ID == a.ID && saved.Account.Token.AccountID == a.Token.AccountID &&
			saved.Account.Token.RefreshToken == a.Token.RefreshToken && saved.Account.Token.AccessToken == a.Token.AccessToken
	}
	if saved, ok := m.volatileSuccessors[a.ID]; ok && matches(saved) {
		return nil
	}
	value, err := readRecovery(m.successorPath(a.ID))
	if err != nil {
		return err
	}
	var saved successor
	if value.Exists && json.Unmarshal(value.Data, &saved) == nil && matches(saved) {
		return nil
	}
	return errors.New("the imported native credential is a possibly consumed refresh predecessor; use a genuinely new Claude Code login before replacing its recovery fence")
}

func (m *Manager) finishNativeRefresh(ctx context.Context, a *store.Account, saved successor) error {
	locked, unlock, err := m.locks(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := m.readSnapshot(locked)
	if err != nil {
		return err
	}
	return m.finishNativeRefreshLocked(locked, a, saved, current)
}

func (m *Manager) finishNativeRefreshLocked(ctx context.Context, a *store.Account, saved successor, current snapshot) error {
	if err := checkLocks(ctx); err != nil {
		return err
	}
	if saved.Native == nil || saved.Native.Paths != m.paths || saved.Account.ID != a.ID || saved.Account.Token.AccountID != a.Token.AccountID {
		return ErrConflict
	}
	before := saved.Native.Before
	configured, err := identityOf(current.Config.Data)
	config, _ := object(current.Config.Data)
	if err != nil || !sameIdentity(configured, accountIdentity(saved.Account)) || len(config["primaryApiKey"]) != 0 || !reflect.DeepEqual(current.ManagedKey, before.ManagedKey) {
		return m.recoverDisplacedNative(ctx, a, saved, current)
	}
	credential, err := credentialsFor(saved.Account, current.credential())
	if err != nil {
		return err
	}
	next := before
	if m.paths.Keychain {
		next.Keychain = fileValue{Data: credential, Exists: true}
		if before.File.Exists {
			next.File = fileValue{Data: credential, Exists: true}
		}
	} else {
		next.File = fileValue{Data: credential, Exists: true}
	}
	// Project settings can change after an interrupted cleanup. Authentication
	// identity and credential generations are fenced; unrelated config is live.
	fenced := current
	fenced.Config = before.Config
	if !validRefreshTransition(fenced, before, next) {
		return m.recoverDisplacedNative(ctx, a, saved, current)
	}
	next.Config = current.Config
	if err := validateSnapshotSize(next); err != nil {
		return err
	}
	if !reflect.DeepEqual(current, next) {
		if m.paths.Keychain {
			if err := m.keys.Write(ctx, m.paths.Service, credential); err != nil {
				return errors.New("Claude native refresh successor is retained; native Keychain write is pending")
			}
		}
		if next.File.Exists {
			if err := writeLocked(ctx, m.paths.CredentialsFile, next.File); err != nil {
				return errors.New("Claude native refresh successor is retained; native credential file write is pending")
			}
		}
	}
	verified, err := m.readSnapshot(ctx)
	if err != nil || !reflect.DeepEqual(verified, next) {
		return errors.New("Claude native refresh writes did not verify; successor retained for recovery")
	}
	saved.Account.AutoUseReset = a.AutoUseReset
	saved.Account.ClaudeCodeRefreshPending = true
	saved.Account.ClaudeCodeRefreshRecovery, _ = json.Marshal(saved.Native)
	if err := m.saveAccount(saved.Account); err != nil {
		return errors.New("Claude native generation was refreshed; account successor is retained for recovery")
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
	m.publish(accountIdentity(*a), []store.Account{*a})
	return nil
}

func sameAccountCredential(a, b fileValue) bool {
	if a.Exists != b.Exists {
		return false
	}
	left, err := object(a.Data)
	if err != nil {
		return false
	}
	right, err := object(b.Data)
	if err != nil {
		return false
	}
	for _, key := range sharedKeys {
		delete(left, key)
		delete(right, key)
	}
	return reflect.DeepEqual(left, right)
}

func validRefreshTransition(current, before, next snapshot) bool {
	if !reflect.DeepEqual(current.Config, before.Config) || !reflect.DeepEqual(current.ManagedKey, before.ManagedKey) {
		return false
	}
	for _, pair := range []struct{ now, old, next fileValue }{
		{current.File, before.File, next.File}, {current.Keychain, before.Keychain, next.Keychain},
	} {
		if !sameAccountCredential(pair.now, pair.old) && !sameAccountCredential(pair.now, pair.next) {
			return false
		}
	}
	return true
}

// Code may have advanced the installed successor, or explicitly logged into
// another account, before cleanup recovered. Never restore the old preimage
// over that verified login. Archive the issued bytes before retiring its fence.
func (m *Manager) recoverDisplacedNative(ctx context.Context, a *store.Account, saved successor, current snapshot) error {
	oauth, err := parseOAuth(current.credential())
	before, _ := parseOAuth(saved.Native.Before.credential())
	configured, configErr := identityOf(current.Config.Data)
	config, _ := object(current.Config.Data)
	if err != nil || configErr != nil || oauth.AccessToken == "" || oauth.RefreshToken == "" ||
		oauth.ExpiresAt <= time.Now().Add(time.Minute).UnixMilli() ||
		oauth.RefreshToken == before.RefreshToken || current.ManagedKey.Exists || len(config["primaryApiKey"]) != 0 {
		return ErrConflict
	}
	identity, err := m.profile(ctx, oauth.AccessToken)
	if err != nil || identity.UUID == "" || identity.Email == "" || !sameIdentity(identity, configured) {
		return ErrConflict
	}
	observed, err := m.readSnapshot(ctx)
	if err != nil || !reflect.DeepEqual(observed, current) {
		return ErrConflict
	}
	// Keep the original received successor available if the account save or
	// archival fails. Same-user organization changes remain separate slots.
	m.volatileSuccessors[a.ID] = saved
	next := saved.Account
	next.AutoUseReset = a.AutoUseReset
	if sameIdentity(identity, accountIdentity(saved.Account)) {
		retainIdentityMetadata(&next, current.Config.Data, identity)
		if err := applyNative(&next, current.credential(), identity); err != nil {
			return err
		}
	}
	next.ClaudeCodeRefreshPending = true
	next.ClaudeCodeRefreshRecovery, _ = json.Marshal(saved.Native)
	if err := checkLocks(ctx); err != nil {
		return err
	}
	if err := m.saveAccount(next); err != nil {
		return err
	}
	*a = next
	if err := m.retireSuccessor(next); err != nil {
		return err
	}
	next.ClaudeCodeRefreshPending = false
	next.ClaudeCodeRefreshRecovery = nil
	if err := m.saveAccount(next); err != nil {
		return err
	}
	*a = next
	accounts, err := m.store.List()
	if err != nil {
		return err
	}
	m.publish(identity, accounts)
	return nil
}
