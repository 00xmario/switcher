package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/store"
)

type memoryKeychain struct {
	mu                     sync.Mutex
	values                 map[string][]byte
	reads, writes, deletes int
	readErr                error
	failDelete             string
	writeErr               error
}

func (k *memoryKeychain) Read(ctx context.Context, key string) ([]byte, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reads++
	value, exists := k.values[key]
	return append([]byte(nil), value...), exists, k.readErr
}
func (k *memoryKeychain) Write(ctx context.Context, key string, value []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writes++
	if k.writeErr != nil {
		return k.writeErr
	}
	k.values[key] = append([]byte(nil), value...)
	return nil
}
func (k *memoryKeychain) Delete(ctx context.Context, key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deletes++
	if k.failDelete == key {
		k.failDelete = ""
		return errors.New("fixture delete failed")
	}
	delete(k.values, key)
	return nil
}

type failingStore struct {
	*store.Store
	failSave bool
}

func (s *failingStore) Save(a store.Account) error {
	if s.failSave {
		return errors.New("fixture account save failed")
	}
	return s.Store.Save(a)
}

type harness struct {
	manager  *Manager
	paths    Paths
	accounts *failingStore
	keys     *memoryKeychain
	grants   int
}

func encoded(value any) []byte { data, _ := json.Marshal(value); return data }
func identityFor(name string) Identity {
	return Identity{UUID: "uuid-" + name, Email: name + "@example.test", OrganizationUUID: "org-" + name, OrganizationName: "Fixture " + name}
}
func rawCredential(name string) []byte {
	return encoded(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "at-" + name, "refreshToken": "rt-" + name, "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:profile", "user:inference"}}, "trustedDeviceToken": "device-" + name, "unknownAccountField": "account-" + name})
}
func nativeAccount(name string) store.Account {
	identity := identityFor(name)
	return store.Account{ID: "claude-" + name, Provider: "claude", Email: identity.Email, CreatedAt: 1, Token: store.Token{AccessToken: "at-" + name, RefreshToken: "rt-" + name, ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: identity.UUID}, ClaudeCode: &store.ClaudeCodeLogin{Credentials: rawCredential(name), OAuthAccount: encoded(identity)}}
}
func newHarness(t *testing.T, keychain bool) *harness {
	t.Helper()
	home := t.TempDir()
	data := t.TempDir()
	paths := Paths{Home: home, ConfigHome: filepath.Join(home, ".claude"), ConfigFile: filepath.Join(home, ".claude.json"), CredentialsFile: filepath.Join(home, ".claude", ".credentials.json"), Service: "Claude Code-credentials", Keychain: keychain}
	accounts := &failingStore{Store: store.New(data)}
	for _, name := range []string{"a", "b"} {
		if err := accounts.Save(nativeAccount(name)); err != nil {
			t.Fatal(err)
		}
	}
	config := encoded(map[string]any{"oauthAccount": identityFor("a"), "projects": map[string]any{"/fixture/project": map[string]any{"trusted": true}}, "mcpServers": map[string]any{"local": map[string]any{"command": "fixture-command"}}, "userID": "machine-id", "unknownMachineSetting": true})
	if err := writePrivate(paths.ConfigFile, fileValue{Data: config, Exists: true}); err != nil {
		t.Fatal(err)
	}
	live := map[string]json.RawMessage{}
	_ = json.Unmarshal(rawCredential("a"), &live)
	for _, key := range sharedKeys {
		live[key] = encoded(map[string]any{"generation": "live-current"})
	}
	credential := encoded(live)
	if err := writePrivate(paths.CredentialsFile, fileValue{Data: credential, Exists: true}); err != nil {
		t.Fatal(err)
	}
	keys := &memoryKeychain{values: map[string][]byte{}}
	if keychain {
		keys.values[paths.Service] = credential
	}
	h := &harness{paths: paths, accounts: accounts, keys: keys}
	profile := func(ctx context.Context, token string) (Identity, error) {
		for _, name := range []string{"a", "b"} {
			if strings.HasPrefix(token, "at-"+name) {
				return identityFor(name), nil
			}
		}
		return Identity{}, errors.New("fixture profile unavailable")
	}
	grant := func(ctx context.Context, a *store.Account) error {
		h.grants++
		a.Token.AccessToken = "at-" + strings.TrimPrefix(a.ID, "claude-") + "-refreshed"
		a.Token.RefreshToken = "rt-successor"
		a.Token.ExpiresAt = time.Now().Add(time.Hour).Unix()
		return nil
	}
	h.manager = New(paths, data, accounts, keys, profile, grant)
	h.manager.lockWait = 100 * time.Millisecond
	return h
}

func TestSwitchNativeStoresPreservesMachineAndAccountFields(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		t.Run(intText(map[bool]int{false: 0, true: 1}[keychain]), func(t *testing.T) {
			h := newHarness(t, keychain)
			ctx := context.Background()
			original, err := h.manager.inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			target, _ := h.accounts.Get("claude-b")
			targetData, _ := object(target.ClaudeCode.Credentials)
			targetData["mcpOAuth"] = encoded(map[string]any{"generation": "stale-slot"})
			target.ClaudeCode.Credentials = encoded(targetData)
			identityData, _ := object(target.ClaudeCode.OAuthAccount)
			identityData["organizationRole"] = encoded("fixture-role")
			target.ClaudeCode.OAuthAccount = encoded(identityData)
			no := false
			target.AutoUseReset = &no
			if err := h.accounts.Save(target); err != nil {
				t.Fatal(err)
			}
			commits := 0
			result, err := h.manager.Switch(ctx, target.ID, func() error { commits++; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !result.Changed || result.Backup == "" || commits != 1 {
				t.Fatalf("missing switch result: %+v", result)
			}
			current, err := h.manager.inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			config, _ := object(current.Config.Data)
			beforeConfig, _ := object(original.Config.Data)
			for _, key := range []string{"projects", "mcpServers", "userID", "unknownMachineSetting"} {
				var currentValue, previousValue any
				_ = json.Unmarshal(config[key], &currentValue)
				_ = json.Unmarshal(beforeConfig[key], &previousValue)
				if !reflect.DeepEqual(currentValue, previousValue) {
					t.Fatalf("machine config %s changed", key)
				}
			}
			identity, _ := identityOf(current.Config.Data)
			if identity.UUID != "uuid-b" {
				t.Fatal("native oauthAccount was not switched")
			}
			nativeIdentity, _ := object(config["oauthAccount"])
			if string(nativeIdentity["organizationRole"]) != `"fixture-role"` {
				t.Fatal("target native identity metadata was dropped")
			}
			credentials, _ := object(current.credential())
			beforeCreds, _ := object(original.credential())
			for _, key := range sharedKeys {
				if !bytes.Equal(credentials[key], beforeCreds[key]) {
					t.Fatalf("shared credential %s was replaced by stale slot state", key)
				}
			}
			if string(credentials["trustedDeviceToken"]) != `"device-b"` || string(credentials["unknownAccountField"]) != `"account-b"` {
				t.Fatal("account-bound fields leaked across switch")
			}
			oauth, _ := parseOAuth(current.credential())
			if oauth.AccessToken != "at-b" {
				t.Fatal("actual native credential was not switched")
			}
			backup, err := readPrivate(filepath.Join(result.Backup, "snapshot.json"))
			if err != nil || !backup.Exists {
				t.Fatal("pre-switch backup missing")
			}
			var saved journal
			if json.Unmarshal(backup.Data, &saved) != nil || !reflectSnapshots(saved.Before, original) {
				t.Fatal("backup did not preserve exact original state")
			}
			info, err := os.Stat(filepath.Join(result.Backup, "snapshot.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("backup must be user-only")
			}
			stored, _ := h.accounts.Get(target.ID)
			if stored.AutoUseReset == nil || *stored.AutoUseReset {
				t.Fatal("native switch dropped account preference")
			}
			if h.manager.Status().ActiveID != target.ID {
				t.Fatal("native active status not updated")
			}
			if _, err := os.Stat(filepath.Join(h.paths.ConfigHome, "projects")); !os.IsNotExist(err) {
				t.Fatal("switch touched transcript directory")
			}
		})
	}
}

func reflectSnapshots(a, b snapshot) bool { return bytes.Equal(encoded(a), encoded(b)) }

func TestKeychainOnlySwitchNeverCreatesPlaintextCredentials(t *testing.T) {
	h := newHarness(t, true)
	if err := writePrivate(h.paths.CredentialsFile, fileValue{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.paths.CredentialsFile); !os.IsNotExist(err) {
		t.Fatal("keychain-only user gained a plaintext credential file")
	}
}

func TestSwitchCapturesVerifiedOutgoingRotation(t *testing.T) {
	h := newHarness(t, false)
	live, _ := object(rawCredential("a"))
	oauth, _ := object(live["claudeAiOauth"])
	oauth["accessToken"] = encoded("at-a-rotated")
	oauth["refreshToken"] = encoded("rt-a-rotated")
	live["claudeAiOauth"] = encoded(oauth)
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: encoded(live), Exists: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	saved, _ := h.accounts.Get("claude-a")
	if saved.Token.RefreshToken != "rt-a-rotated" || saved.Token.AccessToken != "at-a-rotated" {
		t.Fatal("outgoing slot retained a consumed token generation")
	}
}

func TestSwitchDoesNotPoisonWrongOutgoingSlot(t *testing.T) {
	h := newHarness(t, false)
	// Config claims A, credential actually belongs to B. The token's identity
	// wins; only B can receive it, and the full original state is backed up.
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: rawCredential("b"), Exists: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	saved, _ := h.accounts.Get("claude-a")
	if saved.Token.RefreshToken != "rt-a" {
		t.Fatal("foreign credentials poisoned A's saved login")
	}
	current, _ := h.manager.inspect(context.Background())
	identity, _ := identityOf(current.Config.Data)
	if identity.UUID != "uuid-b" {
		t.Fatal("config/credential mismatch not repaired")
	}
}

func TestAlreadyNativeActiveAdoptsRotationWithoutWritingStores(t *testing.T) {
	h := newHarness(t, true)
	before, _ := h.manager.inspect(context.Background())
	result, err := h.manager.Switch(context.Background(), "claude-a", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	after, _ := h.manager.inspect(context.Background())
	if result.Changed || h.keys.writes != 0 || !reflectSnapshots(before, after) {
		t.Fatal("self-switch rewrote the native login")
	}
}

func TestNativeKeychainReadErrorNeverConsumesFileFallback(t *testing.T) {
	h := newHarness(t, true)
	h.keys.readErr = errors.New("fixture locked Keychain")
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("locked keychain was ignored")
	}
	if h.grants != 0 {
		t.Fatal("possibly stale fallback refresh token was consumed")
	}
}

func expireNativeFixture(t *testing.T, h *harness) {
	t.Helper()
	live, err := h.manager.inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := object(live.credential())
	oauth, _ := object(credential["claudeAiOauth"])
	oauth["expiresAt"] = encoded(int64(1000))
	credential["claudeAiOauth"] = encoded(oauth)
	raw := encoded(credential)
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: raw, Exists: true}); err != nil {
		t.Fatal(err)
	}
	if h.paths.Keychain {
		h.keys.values[h.paths.Service] = raw
	}
}

func TestOwnedExpiredCredentialRefreshesLiveGenerationUnderNativeLocks(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		t.Run(intText(map[bool]int{false: 0, true: 1}[keychain]), func(t *testing.T) {
			h := newHarness(t, keychain)
			expireNativeFixture(t, h)
			before, _ := h.manager.inspect(context.Background())
			a, _ := h.accounts.Get("claude-a")
			a.Token.RefreshToken = "rt-stale-backup"
			grant := h.manager.grant
			if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
				if a.Token.RefreshToken != "rt-a" {
					t.Fatal("native refresh used the stale stored copy")
				}
				for _, path := range []string{filepath.Join(h.paths.ConfigHome, ".oauth_refresh.lock"), h.paths.ConfigHome + ".lock", h.paths.ConfigFile + ".lock"} {
					if info, err := os.Stat(path); err != nil || !info.IsDir() {
						t.Fatal("native locks were not held across the grant")
					}
				}
				return grant(ctx, a)
			}); err != nil {
				t.Fatal(err)
			}
			after, _ := h.manager.inspect(context.Background())
			oauth, _ := parseOAuth(after.credential())
			if oauth.RefreshToken != "rt-successor" || h.grants != 1 || !bytes.Equal(before.Config.Data, after.Config.Data) {
				t.Fatal("native refresh did not advance credentials without changing config")
			}
			oldFields, _ := object(before.credential())
			newFields, _ := object(after.credential())
			for _, field := range sharedKeys {
				if !bytes.Equal(oldFields[field], newFields[field]) {
					t.Fatal("native refresh lost shared credential state")
				}
			}
			if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil || h.grants != 1 {
				t.Fatal("fresh locked native generation was refreshed twice")
			}
		})
	}
}

func TestNativeRefreshWriteFailureRecoversAfterRestartWithoutRepost(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture Keychain write failed")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("native write failure hidden")
	}
	if h.grants != 1 {
		t.Fatal("fixture grant not consumed")
	}
	h.keys.writeErr = nil
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	a, _ = h.accounts.Get(a.ID)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	current, _ := restarted.inspect(context.Background())
	oauth, _ := parseOAuth(current.credential())
	if h.grants != 1 || oauth.RefreshToken != "rt-successor" || a.Token.RefreshToken != oauth.RefreshToken {
		t.Fatal("restart recovery reposted or lost the native successor")
	}
}

func TestNativeRefreshDoesNotOverwriteForeignUpdateDuringGrant(t *testing.T) {
	h := newHarness(t, false)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	grant := h.manager.grant
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		return writePrivate(h.paths.CredentialsFile, fileValue{Data: rawCredential("b"), Exists: true})
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign update during grant: %v", err)
	}
	current, _ := h.manager.inspect(context.Background())
	oauth, _ := parseOAuth(current.credential())
	if oauth.RefreshToken != "rt-b" || h.grants != 1 || h.manager.volatileSuccessors[a.ID].Account.Token.RefreshToken != "rt-successor" {
		t.Fatal("foreign update or issued successor was lost")
	}
}

func TestNativeRefreshFallbackPreservesNativeRepairAcrossRestart(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	grant := h.manager.grant
	blocked := filepath.Join(h.manager.dataRoot, "successors")
	h.keys.writeErr = errors.New("fixture native write unavailable")
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		return os.WriteFile(blocked, []byte("fixture sidecar unavailable"), 0600)
	}); err == nil {
		t.Fatal("pending native repair hidden")
	}
	a, _ = h.accounts.Get(a.ID)
	if !a.ClaudeCodeRefreshPending || len(a.ClaudeCodeRefreshRecovery) == 0 || a.Token.RefreshToken != "rt-successor" {
		t.Fatal("durable account fallback lost native repair metadata")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	h.keys.writeErr = nil
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	current, _ := restarted.inspect(context.Background())
	oauth, _ := parseOAuth(current.credential())
	if h.grants != 1 || oauth.RefreshToken != "rt-successor" || a.ClaudeCodeRefreshPending || len(a.ClaudeCodeRefreshRecovery) != 0 {
		t.Fatal("native fallback did not finalize without another grant")
	}
}

func TestCaptureRepairsPendingNativeRefreshBeforeRetiringRecovery(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture native write failed")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not retain successor")
	}
	if _, err := h.manager.CaptureAndStore(context.Background(), func(a *store.Account) error { return h.accounts.Save(*a) }); err == nil {
		t.Fatal("import accepted a consumed native predecessor")
	}
	if len(h.manager.volatileSuccessors) != 1 {
		t.Fatal("failed import retired issued bytes")
	}
	h.keys.writeErr = nil
	imported, err := h.manager.CaptureAndStore(context.Background(), func(a *store.Account) error { return h.accounts.Save(*a) })
	if err != nil {
		t.Fatal(err)
	}
	if imported.Token.RefreshToken != "rt-successor" || h.grants != 1 {
		t.Fatal("import replaced or reposted the issued native generation")
	}
}

func TestSwitchAwayRepairsOutgoingNativeRefreshFirst(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture native write failed")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not retain native recovery")
	}
	h.keys.writeErr = nil
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	stored, _ := h.accounts.Get(a.ID)
	current, _ := h.manager.inspect(context.Background())
	oauth, _ := parseOAuth(current.credential())
	if stored.Token.RefreshToken != "rt-successor" || oauth.RefreshToken != "rt-b" || h.grants != 1 {
		t.Fatal("switch away stranded or replaced outgoing native successor")
	}
}

type boundedMemoryKeychain struct{ *memoryKeychain }

func (k boundedMemoryKeychain) Validate(service string, value []byte) error {
	return (SystemKeychain{}).Validate(service, value)
}

func TestNativeRefreshPreflightsDeterministicallyUnwritableKeychainPayload(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	fields, _ := object(h.keys.values[h.paths.Service])
	fields["mcpOAuth"] = encoded(strings.Repeat("fixture-large-shared-field", 200))
	h.keys.values[h.paths.Service] = encoded(fields)
	h.manager.keys = boundedMemoryKeychain{h.keys}
	a, _ := h.accounts.Get("claude-a")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("guaranteed native writer failure was not preflighted")
	}
	if h.grants != 0 || h.keys.writes != 0 {
		t.Fatal("unwritable wrapper consumed the native grant")
	}
}

func TestNativeRefreshRecoveryPreservesUnrelatedConfigChanges(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	before, _ := h.manager.inspect(context.Background())
	a, _ := h.accounts.Get("claude-a")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	// Represent interruption after the successor was saved but before cleanup.
	saved := successor{Account: a, Predecessor: "rt-a", Native: &nativeRefresh{Paths: h.paths, Before: before}}
	if err := writePrivate(h.manager.successorPath(a.ID), fileValue{Data: encoded(saved), Exists: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.beginRefresh("rt-a", a.ID); err != nil {
		t.Fatal(err)
	}
	config, _ := object(before.Config.Data)
	config["projects"] = encoded(map[string]any{"new-project": map[string]any{"trusted": true}})
	updated := encoded(config)
	if err := writePrivate(h.paths.ConfigFile, fileValue{Data: updated, Exists: true}); err != nil {
		t.Fatal(err)
	}
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	current, _ := restarted.inspect(context.Background())
	if !bytes.Equal(current.Config.Data, updated) || h.grants != 1 {
		t.Fatal("config update was reverted or triggered another grant")
	}
}

func TestAmbiguousNativeGrantImportCannotClearConsumeFence(t *testing.T) {
	h := newHarness(t, false)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	posts := 0
	grant := func(context.Context, *store.Account) error {
		posts++
		return errors.New("fixture lost refresh response")
	}
	if err := h.manager.Refresh(context.Background(), &a, grant); err == nil {
		t.Fatal("ambiguous grant was accepted")
	}
	if _, err := h.manager.CaptureAndStore(context.Background(), func(a *store.Account) error { return h.accounts.Save(*a) }); err == nil {
		t.Fatal("unchanged-native import retired an uncertain consume fence")
	}
	intent, err := readPrivate(h.manager.intentPath(a.ID))
	if err != nil || !intent.Exists {
		t.Fatal("import removed the uncertain consumption intent")
	}
	a, _ = h.accounts.Get(a.ID)
	if err := h.manager.Refresh(context.Background(), &a, grant); err == nil || posts != 1 {
		t.Fatal("import allowed the possibly consumed predecessor to be posted again")
	}
}

func TestInactiveRefreshPersistsSuccessorAndKeepsNativeLogin(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	before, _ := h.manager.inspect(ctx)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	if err := h.manager.Refresh(ctx, &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	after, _ := h.manager.inspect(ctx)
	if !reflectSnapshots(before, after) {
		t.Fatal("inactive refresh modified the active native login")
	}
	saved, _ := h.accounts.Get(a.ID)
	if saved.Token.RefreshToken != "rt-successor" {
		t.Fatal("refresh successor was not durably saved")
	}
	if h.grants != 1 {
		t.Fatal("unexpected refresh count")
	}
}

func TestFailedAccountSaveRecoversSuccessorWithoutReusingGrant(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	original := a
	h.accounts.failSave = true
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("save failure was hidden")
	}
	if h.grants != 1 {
		t.Fatal("grant was not exercised")
	}
	h.accounts.failSave = false
	a = original
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if h.grants != 1 || a.Token.RefreshToken != "rt-successor" {
		t.Fatal("spent refresh grant was retried instead of recovering successor")
	}
}

func TestNativeMutationFailureRollsBackBothStores(t *testing.T) {
	for _, kind := range []string{"commit", "keychain-clear"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, true)
			ctx := context.Background()
			h.keys.values["Claude Code"] = []byte("fixture-managed-key")
			before, _ := h.manager.inspect(ctx)
			commit := func() error { return errors.New("fixture routing save failed") }
			if kind == "keychain-clear" {
				h.keys.failDelete = "Claude Code"
				commit = func() error { return nil }
			}
			result, err := h.manager.Switch(ctx, "claude-b", commit)
			if err == nil || result.Backup == "" {
				t.Fatal("partial failure did not report retained backup")
			}
			after, e := h.manager.inspect(ctx)
			if e != nil || !reflectSnapshots(before, after) {
				t.Fatal("partial native write was not rolled back")
			}
			pending, _ := readPrivate(h.manager.pendingPath())
			if pending.Exists {
				t.Fatal("completed rollback left a pending operation")
			}
		})
	}
}

func TestMalformedConfigAndSymlinksAreNotOverwritten(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, false)
			original := []byte(`{"projects":`)
			if kind == "malformed" {
				if err := os.WriteFile(h.paths.ConfigFile, original, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				outside := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(outside, original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(h.paths.ConfigFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, h.paths.ConfigFile); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err == nil {
				t.Fatal("unsafe config accepted")
			}
			data, _ := os.ReadFile(h.paths.ConfigFile)
			if !bytes.Equal(data, original) {
				t.Fatal("unreadable config destroyed")
			}
		})
	}
}

func TestLiveChangesDuringIdentityProbeAreFenced(t *testing.T) {
	h := newHarness(t, false)
	original := h.manager.profile
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-b" {
			if err := os.WriteFile(h.paths.ConfigFile, encoded(map[string]any{"oauthAccount": identityFor("a"), "newSetting": true}), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return original(ctx, token)
	}
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("racing config: %v", err)
	}
	current, _ := readPrivate(h.paths.ConfigFile)
	if !bytes.Contains(current.Data, []byte("newSetting")) {
		t.Fatal("racing setting update lost")
	}
}

func TestInterruptedSwitchRecoveryDoesNotOverwriteForeignChanges(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	before, _ := h.manager.inspect(ctx)
	next := before
	next.File = fileValue{Data: rawCredential("b"), Exists: true}
	next.Config = fileValue{Data: encoded(map[string]any{"oauthAccount": identityFor("b")}), Exists: true}
	saved := journal{Version: 1, Paths: h.paths, TargetID: "claude-b", Before: before, After: next}
	if err := writePrivate(h.manager.pendingPath(), fileValue{Data: encoded(saved), Exists: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.writeSnapshot(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(ctx, "claude-a", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, _ := h.manager.inspect(ctx)
	if !reflectSnapshots(before, current) {
		t.Fatal("interrupted operation not rolled back before switch")
	}
	if err := writePrivate(h.manager.pendingPath(), fileValue{Data: encoded(saved), Exists: true}); err != nil {
		t.Fatal(err)
	}
	foreign := encoded(map[string]any{"oauthAccount": identityFor("a"), "externalChange": "keep"})
	if err := writePrivate(h.paths.ConfigFile, fileValue{Data: foreign, Exists: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(ctx, "claude-b", func() error { return nil }); err == nil {
		t.Fatal("foreign update silently rolled back")
	}
	kept, _ := readPrivate(h.paths.ConfigFile)
	if !bytes.Equal(kept.Data, foreign) {
		t.Fatal("foreign config update overwritten during recovery")
	}
}

func TestNativeLockContentionAndStaleness(t *testing.T) {
	h := newHarness(t, false)
	primary := filepath.Join(h.paths.ConfigHome, ".oauth_refresh.lock")
	if err := os.Mkdir(primary, 0700); err != nil {
		t.Fatal(err)
	}
	_, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil })
	if err == nil {
		t.Fatal("live refresh lock was stolen")
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(primary, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{primary, h.paths.ConfigHome + ".lock", h.paths.ConfigFile + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("native lock left behind")
		}
	}
}

func TestCaptureAndSynchronizeOnlyVerifiedNativeIdentity(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	a, err := h.manager.Capture(ctx)
	if err != nil || a.Token.AccountID != "uuid-a" || a.ClaudeCode == nil {
		t.Fatalf("capture: %v", err)
	}
	stored, _ := h.accounts.Get("claude-a")
	changed, err := h.manager.Synchronize(ctx, &stored)
	if err != nil || !changed || stored.Token.AccessToken != "at-a" {
		t.Fatalf("synchronize: %v %v", changed, err)
	}
	if h.manager.Status().ActiveID != "claude-a" {
		t.Fatal("native active identity not reported")
	}
	b, _ := h.accounts.Get("claude-b")
	changed, err = h.manager.Synchronize(ctx, &b)
	if err != nil || changed || b.Token.RefreshToken != "rt-b" {
		t.Fatal("native A contaminated inactive B")
	}
}

func TestConfigRepairUsesCurrentTargetGeneration(t *testing.T) {
	h := newHarness(t, true)
	live, _ := object(rawCredential("b"))
	oauth, _ := object(live["claudeAiOauth"])
	oauth["accessToken"] = encoded("at-b-rotated")
	oauth["refreshToken"] = encoded("rt-b-current")
	live["claudeAiOauth"] = encoded(oauth)
	h.keys.values[h.paths.Service] = encoded(live)
	h.keys.values["Claude Code"] = []byte("fixture-conflicting-key")
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, _ := h.manager.inspect(context.Background())
	native, _ := parseOAuth(current.credential())
	saved, _ := h.accounts.Get("claude-b")
	if native.RefreshToken != "rt-b-current" || saved.Token.RefreshToken != native.RefreshToken {
		t.Fatal("repair restored the old target refresh generation")
	}
}

func TestCommitReceiptPreventsRollbackAfterMarkerFailure(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	committed := ""
	h.manager.SetReceiptReader(func(receipt string) (bool, error) { return receipt == committed, nil })
	// A successful durable routing write can outlive a missing final marker.
	// Recreate that crash window using the exact transaction backup.
	result, err := h.manager.SwitchWithReceipt(ctx, "claude-b", func(receipt string) error { committed = receipt; return nil })
	if err != nil || committed == "" {
		t.Fatalf("receipt was not committed: %v", err)
	}
	raw, _ := readPrivate(filepath.Join(result.Backup, "snapshot.json"))
	var pending journal
	if err := json.Unmarshal(raw.Data, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Committed || pending.Receipt != committed {
		t.Fatal("fixture does not represent pre-marker crash window")
	}
	if err := writePrivate(h.manager.pendingPath(), raw); err != nil {
		t.Fatal(err)
	}
	a, _ := h.accounts.Get("claude-a")
	if _, err := h.manager.Synchronize(ctx, &a); err != nil {
		t.Fatal(err)
	}
	current, _ := h.manager.inspect(ctx)
	identity, _ := identityOf(current.Config.Data)
	if identity.UUID != "uuid-b" {
		t.Fatal("committed native login rolled back after marker crash")
	}
	left, _ := readPrivate(h.manager.pendingPath())
	if left.Exists {
		t.Fatal("confirmed committed journal was not finalized")
	}
}

func TestFreshReloginRetiresSupersededSuccessor(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	h.accounts.failSave = true
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not create recovery record")
	}
	h.accounts.failSave = false
	newLogin := nativeAccount("b")
	newLogin.Token.RefreshToken = "rt-new-login"
	newLogin.Token.AccessToken = "at-b-new-login"
	if err := h.accounts.Save(newLogin); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.RetireSuccessor(newLogin); err != nil {
		t.Fatal(err)
	}
	newLogin.Token.ExpiresAt = 1
	if err := h.manager.Refresh(context.Background(), &newLogin, h.manager.grant); err != nil {
		t.Fatal("obsolete successor blocked new login")
	}
	if h.grants != 2 {
		t.Fatal("new login was not refreshable")
	}
	archives, err := os.ReadDir(filepath.Join(h.manager.dataRoot, "generations"))
	if err != nil || len(archives) == 0 {
		t.Fatal("superseded token was discarded rather than archived")
	}
}

func TestDoublePersistenceFailureRetainsSuccessorAndDoesNotRepost(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	original := a
	grant := h.manager.grant
	h.manager.grant = func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		h.accounts.failSave = true
		return os.WriteFile(filepath.Join(h.manager.dataRoot, "successors"), []byte("block sidecar directory"), 0600)
	}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("double persistence failure hidden")
	}
	if h.grants != 1 || len(h.manager.volatileSuccessors) != 1 {
		t.Fatal("successful successor was discarded")
	}
	h.accounts.failSave = false
	if err := os.Remove(filepath.Join(h.manager.dataRoot, "successors")); err != nil {
		t.Fatal(err)
	}
	a = original
	h.manager.grant = grant
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if h.grants != 1 || a.Token.RefreshToken != "rt-successor" {
		t.Fatal("predecessor was consumed again instead of recovering memory successor")
	}
}

func TestConsumeFenceSurvivesLossOfInMemorySuccessor(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	if err := h.manager.beginRefresh(a.Token.RefreshToken, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("unrecovered consume intent was ignored")
	}
	if h.grants != 0 {
		t.Fatal("possibly consumed predecessor was posted after restart")
	}
}

func TestWrapperIsValidatedBeforeConsumingRefreshGrant(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	a.ClaudeCode = &store.ClaudeCodeLogin{Credentials: json.RawMessage(`{"malformed":`), OAuthAccount: encoded(identityFor("b"))}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("malformed wrapper accepted")
	}
	if h.grants != 0 {
		t.Fatal("grant consumed before wrapper validation")
	}
}

func TestMCPOnlyNativeStoreCanActivateWithoutLosingSharedState(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		t.Run(intText(map[bool]int{false: 0, true: 1}[keychain]), func(t *testing.T) {
			h := newHarness(t, keychain)
			ctx := context.Background()
			shared := encoded(map[string]any{"mcpOAuth": map[string]any{"fixture-current": "keep"}, "pluginSecrets": map[string]any{"fixture": "keep"}})
			if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: shared, Exists: true}); err != nil {
				t.Fatal(err)
			}
			if keychain {
				h.keys.values[h.paths.Service] = shared
			}
			if _, err := h.manager.Switch(ctx, "claude-b", func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			current, _ := h.manager.inspect(ctx)
			object, _ := object(current.credential())
			if !bytes.Contains(object["mcpOAuth"], []byte("fixture-current")) || !bytes.Contains(object["pluginSecrets"], []byte("keep")) {
				t.Fatal("MCP-only state lost during activation")
			}
		})
	}
}

func TestLegacySlotIDIsNotReusedForAnotherOrganization(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-a")
	a.ClaudeCode = nil
	if err := h.accounts.Save(a); err != nil {
		t.Fatal(err)
	}
	other := identityFor("a")
	other.OrganizationUUID = "other-organization"
	if id := h.manager.ExistingID(other); id != "" {
		t.Fatal("legacy slot reused solely by email/UUID")
	}
	// Native owner B with the same user but another organization must not
	// overwrite the old A slot's credentials during background synchronization.
	live := rawCredential("b")
	liveObject, _ := object(live)
	liveOAuth, _ := object(liveObject["claudeAiOauth"])
	liveOAuth["accessToken"] = encoded("at-foreign-organization")
	liveOAuth["refreshToken"] = encoded("rt-foreign-organization")
	liveObject["claudeAiOauth"] = encoded(liveOAuth)
	live = encoded(liveObject)
	if err := writePrivate(h.paths.CredentialsFile, fileValue{Data: live, Exists: true}); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(h.paths.ConfigFile, fileValue{Data: encoded(map[string]any{"oauthAccount": other}), Exists: true}); err != nil {
		t.Fatal(err)
	}
	originalProfile := h.manager.profile
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-foreign-organization" {
			return other, nil
		}
		return originalProfile(ctx, token)
	}
	_, err := h.manager.Synchronize(context.Background(), &a)
	if err != nil {
		t.Fatal(err)
	}
	if a.Token.RefreshToken != "rt-a" || accountIdentity(a).OrganizationUUID != "org-a" {
		t.Fatal("native foreign organization contaminated legacy account")
	}
}

func TestAmbiguousGrantFailureDoesNotPermitASecondPost(t *testing.T) {
	for _, status := range []string{"502", "504", "timeout"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, false)
			a, _ := h.accounts.Get("claude-b")
			a.Token.ExpiresAt = 1
			calls := 0
			grant := func(context.Context, *store.Account) error { calls++; return errors.New("fixture ambiguous " + status) }
			if err := h.manager.Refresh(context.Background(), &a, grant); err == nil {
				t.Fatal("ambiguous error hidden")
			}
			if err := h.manager.Refresh(context.Background(), &a, grant); err == nil {
				t.Fatal("unrecovered grant fence ignored")
			}
			if calls != 1 {
				t.Fatal("possibly consumed predecessor was posted twice")
			}
		})
	}
}

func TestExpiredInactiveLegacyAccountRefreshesThenVerifiesOrganization(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.ClaudeCode = nil
	a.Token.ExpiresAt = 1
	if err := h.accounts.Save(a); err != nil {
		t.Fatal(err)
	}
	originalProfile := h.manager.profile
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-b" {
			return Identity{}, errors.New("fixture expired access token")
		}
		return originalProfile(ctx, token)
	}
	if changed, err := h.manager.Synchronize(context.Background(), &a); err != nil || changed {
		t.Fatalf("legacy synchronization should defer inactive refresh: %v %v", changed, err)
	}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if h.grants != 1 || a.ClaudeCode == nil || accountIdentity(a).OrganizationUUID != "org-b" {
		t.Fatal("successor was not used to verify legacy organization")
	}
}

func TestRefreshCleanupRetainsSidecarUntilIntentCanBeRemoved(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	if err := h.manager.beginRefresh(a.Token.RefreshToken, a.ID); err != nil {
		t.Fatal(err)
	}
	saved := successor{Account: a, Predecessor: a.Token.RefreshToken}
	saved.Account.Token.RefreshToken = "rt-fixture-successor"
	if err := writePrivate(h.manager.successorPath(a.ID), fileValue{Data: encoded(saved), Exists: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.accounts.Save(saved.Account); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.manager.intentPath(a.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.manager.intentPath(a.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.clearRefreshRecovery(a.ID); err == nil {
		t.Fatal("blocked intent cleanup hidden")
	}
	sidecar, err := readPrivate(h.manager.successorPath(a.ID))
	if err != nil || !sidecar.Exists {
		t.Fatal("sidecar deleted before intent removal succeeded")
	}
	if err := os.Remove(h.manager.intentPath(a.ID)); err != nil {
		t.Fatal(err)
	}
	a = saved.Account
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if h.grants != 0 {
		t.Fatal("already saved successor was consumed rather than recovered")
	}
}

func TestLegacySuccessorSurvivesSidecarAndProfileFailureAcrossRestart(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.ClaudeCode = nil
	a.Token.ExpiresAt = 1
	if err := h.accounts.Save(a); err != nil {
		t.Fatal(err)
	}
	profile := h.manager.profile
	profileOffline := true
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-b" || profileOffline && token == "at-b-refreshed" {
			return Identity{}, errors.New("fixture profile unavailable")
		}
		return profile(ctx, token)
	}
	grant := h.manager.grant
	blocked := filepath.Join(h.manager.dataRoot, "successors")
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		return os.WriteFile(blocked, []byte("fixture sidecar directory unavailable"), 0600)
	}); err == nil {
		t.Fatal("profile failure hidden")
	}
	root := filepath.Dir(h.manager.dataRoot)
	restartedStore := store.New(root)
	a, err := restartedStore.Get(a.ID)
	if err != nil || a.Token.RefreshToken != "rt-successor" || !a.ClaudeCodeRefreshPending || a.ClaudeCode != nil {
		t.Fatal("unverified successor did not survive in account store")
	}
	restarted := New(h.paths, root, restartedStore, h.keys, h.manager.profile, grant)
	if err := restarted.Refresh(context.Background(), &a, grant); err == nil {
		t.Fatal("unverified successor was accepted while profile was offline")
	}
	if h.grants != 1 {
		t.Fatal("restart consumed another grant before verifying issued bytes")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	profileOffline = false
	if changed, err := restarted.Synchronize(context.Background(), &a); err != nil || !changed {
		t.Fatalf("successor recovery after profile/storage repair: changed=%v err=%v", changed, err)
	}
	a, err = restartedStore.Get(a.ID)
	if err != nil || a.ClaudeCodeRefreshPending || accountIdentity(a).OrganizationUUID != "org-b" || h.grants != 1 {
		t.Fatal("restart did not verify and finalize the issued successor")
	}
	intent, err := readPrivate(restarted.intentPath(a.ID))
	if err != nil || intent.Exists {
		t.Fatal("recovered account still has a consume fence")
	}
}

func TestPendingAccountReusesVerifiedVolatileSuccessorAfterSaveFailure(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	a.ClaudeCode = nil
	a.Token.ExpiresAt = 1
	if err := h.accounts.Save(a); err != nil {
		t.Fatal(err)
	}
	profile := h.manager.profile
	profileCalls := 0
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-b" {
			return Identity{}, errors.New("fixture expired access")
		}
		if token == "at-b-refreshed" {
			profileCalls++
			h.accounts.failSave = true
		}
		return profile(ctx, token)
	}
	grant := h.manager.grant
	blocked := filepath.Join(h.manager.dataRoot, "successors")
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		return os.WriteFile(blocked, []byte("fixture sidecar unavailable"), 0600)
	}); err == nil {
		t.Fatal("verified account save failure hidden")
	}
	h.accounts.failSave = false
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	a, _ = h.accounts.Get(a.ID)
	if !a.ClaudeCodeRefreshPending || a.ClaudeCode != nil {
		t.Fatal("fixture did not leave unverified durable fallback")
	}
	yes := true
	a.AutoUseReset = &yes
	if err := h.accounts.Save(a); err != nil {
		t.Fatal(err)
	}
	h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
		if token == "at-b-refreshed" {
			profileCalls++
			return Identity{}, errors.New("fixture profile offline after verification")
		}
		return profile(ctx, token)
	}
	if changed, err := h.manager.Synchronize(context.Background(), &a); err != nil || !changed {
		t.Fatalf("verified in-memory recovery: changed=%v err=%v", changed, err)
	}
	if profileCalls != 1 || h.grants != 1 {
		t.Fatal("recovery repeated a completed profile lookup or consumed another grant")
	}
	if a.ClaudeCodeRefreshPending || accountIdentity(a).OrganizationUUID != "org-b" || a.AutoUseReset == nil || !*a.AutoUseReset {
		t.Fatal("recovery lost verified identity or current account preference")
	}
}

func retainVolatileOnlySuccessor(t *testing.T, h *harness) {
	t.Helper()
	a, _ := h.accounts.Get("claude-b")
	a.Token.ExpiresAt = 1
	grant := h.manager.grant
	blocked := filepath.Join(h.manager.dataRoot, "successors")
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := grant(ctx, a); err != nil {
			return err
		}
		h.accounts.failSave = true
		return os.WriteFile(blocked, []byte("fixture sidecar unavailable"), 0600)
	}); err == nil {
		t.Fatal("fixture did not retain a volatile-only successor")
	}
	h.accounts.failSave = false
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
}

func assertVolatileSuccessorArchived(t *testing.T, m *Manager) {
	t.Helper()
	dir := filepath.Join(m.dataRoot, "generations")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !strings.Contains(file.Name(), "-retired-volatile-refresh-") {
			continue
		}
		raw, err := readPrivate(filepath.Join(dir, file.Name()))
		var saved successor
		if err == nil && json.Unmarshal(raw.Data, &saved) == nil && saved.Account.Token.RefreshToken == "rt-successor" && saved.Predecessor == "rt-b" {
			return
		}
	}
	t.Fatal("retirement discarded volatile credentials without archiving their bytes")
}

func TestReloginArchivesVolatileOnlySuccessor(t *testing.T) {
	h := newHarness(t, false)
	retainVolatileOnlySuccessor(t, h)
	newLogin := nativeAccount("b")
	newLogin.Token.RefreshToken = "rt-new-login"
	if err := h.accounts.Save(newLogin); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.RetireSuccessor(newLogin); err != nil {
		t.Fatal(err)
	}
	assertVolatileSuccessorArchived(t, h.manager)
	if len(h.manager.volatileSuccessors) != 0 {
		t.Fatal("archived successor was not retired")
	}
}

func TestNewLoginRefreshRetriesFailedVolatileRetirementAfterStorageRepair(t *testing.T) {
	h := newHarness(t, false)
	retainVolatileOnlySuccessor(t, h)
	newLogin := nativeAccount("b")
	newLogin.Token.AccessToken = "at-b-new-login"
	newLogin.Token.RefreshToken = "rt-new-login"
	newLogin.Token.ExpiresAt = 1
	if err := h.accounts.Save(newLogin); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(h.manager.dataRoot, "generations")
	if err := os.WriteFile(blocked, []byte("fixture archive unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.RetireSuccessor(newLogin); err == nil || len(h.manager.volatileSuccessors) != 1 {
		t.Fatal("failed archival discarded volatile successor")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := h.manager.Refresh(context.Background(), &newLogin, func(ctx context.Context, a *store.Account) error {
		if a.Token.RefreshToken != "rt-new-login" {
			t.Fatal("retirement replaced new login with stale recovery")
		}
		return h.manager.grant(ctx, a)
	}); err != nil {
		t.Fatal(err)
	}
	if h.grants != 2 {
		t.Fatal("repaired retirement did not permit the new login's grant")
	}
	assertVolatileSuccessorArchived(t, h.manager)
}
