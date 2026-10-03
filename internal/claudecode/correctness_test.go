package claudecode

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"switcher/internal/store"
)

func TestIssuedInactiveSuccessorRecoveredBeforeSwitch(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		t.Run(intText(map[bool]int{false: 0, true: 1}[keychain]), func(t *testing.T) {
			h := newHarness(t, keychain)
			a, _ := h.accounts.Get("claude-b")
			h.accounts.failSave = true
			if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
				t.Fatal("fixture did not interrupt successor persistence")
			}
			h.accounts.failSave = false
			restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
			if _, err := restarted.Switch(context.Background(), a.ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			current, err := restarted.inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			oauth, _ := parseOAuth(current.credential())
			stored, _ := h.accounts.Get(a.ID)
			if oauth.RefreshToken != "rt-successor" || stored.Token.RefreshToken != "rt-successor" || h.grants != 1 {
				t.Fatal("activation installed a consumed predecessor instead of recovering the issued successor")
			}
		})
	}
}

func TestStaleInactiveCallerAdoptsPersistedSuccessorWithoutGrant(t *testing.T) {
	h := newHarness(t, false)
	stale, _ := h.accounts.Get("claude-b")
	a := stale
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	other := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if err := other.Refresh(context.Background(), &stale, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if h.grants != 1 || stale.Token.RefreshToken != "rt-successor" {
		t.Fatal("serialized native locks allowed a stale inactive caller to reuse the already-consumed grant")
	}
}

func TestLargeConfigInterruptedSwitchIsRecoverable(t *testing.T) {
	h := newHarness(t, false)
	before, _ := h.manager.inspect(context.Background())
	config, _ := object(before.Config.Data)
	config["largeProjectSetting"] = encoded(strings.Repeat("x", 900<<10))
	before.Config.Data = encoded(config)
	if err := h.manager.writeSnapshot(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	result, err := h.manager.Switch(context.Background(), "claude-b", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(result.Backup, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(journal) <= 2<<20 {
		t.Fatal("fixture did not exceed the native single-file limit")
	}
	// Recreate a process interruption before the routing commit. The journal
	// bytes came from the real switch, not a hand-built recovery structure.
	if err := os.WriteFile(h.manager.pendingPath(), journal, 0600); err != nil {
		t.Fatal(err)
	}
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Switch(context.Background(), "claude-a", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	current, err := restarted.inspect(context.Background())
	if err != nil || !reflectSnapshots(current, before) {
		t.Fatal("large accepted native config could not recover its interrupted switch")
	}
}

func TestOversizedComposedConfigRejectedBeforeNativeMutation(t *testing.T) {
	h := newHarness(t, true)
	before, _ := h.manager.inspect(context.Background())
	config, _ := object(before.Config.Data)
	config["projectFlags"] = encoded(make([]int, 400000))
	before.Config.Data = encoded(config)
	if err := h.manager.writeSnapshot(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	h.keys.writes = 0
	commits := 0
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { commits++; return nil }); err == nil {
		t.Fatal("oversized composed native config was accepted")
	}
	current, _ := h.manager.inspect(context.Background())
	if h.keys.writes != 0 || commits != 0 || !reflectSnapshots(current, before) {
		t.Fatal("deterministically oversized config changed native stores before preflight rejection")
	}
}

func TestLargeNativeRefreshPreimageRecoversAfterRestart(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	before, _ := h.manager.inspect(context.Background())
	config, _ := object(before.Config.Data)
	config["largeProjectSetting"] = encoded(strings.Repeat("p", 850<<10))
	before.Config.Data = encoded(config)
	credential, _ := object(before.credential())
	credential["mcpOAuth"] = encoded(strings.Repeat("m", 650<<10))
	before.File.Data, before.Keychain.Data = encoded(credential), encoded(credential)
	if err := h.manager.writeSnapshot(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture native write interruption")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not retain issued successor")
	}
	raw, err := os.ReadFile(h.manager.successorPath(a.ID))
	if err != nil || len(raw) <= nativeFileLimit {
		t.Fatal("fixture did not produce an aggregate successor larger than one native store")
	}
	h.keys.writeErr = nil
	a, _ = h.accounts.Get(a.ID)
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	live, _ := restarted.inspect(context.Background())
	fields, _ := object(live.credential())
	if a.Token.RefreshToken != "rt-successor" || h.grants != 1 || !bytes.Equal(fields["mcpOAuth"], credential["mcpOAuth"]) || !bytes.Equal(live.Config.Data, before.Config.Data) {
		t.Fatal("large native recovery lost the issued generation, MCP data, or config")
	}
}

func TestNativeRefreshRecoveryAcceptsVerifiedLiveReplacement(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		for _, replacement := range []string{"rotation", "relogin", "other-account", "other-organization"} {
			t.Run(intText(map[bool]int{false: 0, true: 1}[keychain])+"/"+replacement, func(t *testing.T) {
				h := newHarness(t, keychain)
				expireNativeFixture(t, h)
				before, _ := h.manager.inspect(context.Background())
				a, _ := h.accounts.Get("claude-a")
				if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
					t.Fatal(err)
				}
				pending := successor{Account: a, Predecessor: "rt-a", Native: &nativeRefresh{Paths: h.paths, Before: before}}
				if err := writePrivate(h.manager.successorPath(a.ID), fileValue{Data: encoded(pending), Exists: true}); err != nil {
					t.Fatal(err)
				}
				if err := h.manager.beginRefresh("rt-a", a.ID); err != nil {
					t.Fatal(err)
				}
				name := "a"
				if replacement == "other-account" {
					name = "b"
				}
				identity := identityFor(name)
				if replacement == "other-organization" {
					identity.OrganizationUUID = "new-organization"
					profile := h.manager.profile
					h.manager.profile = func(ctx context.Context, token string) (Identity, error) {
						if token == "at-a-other-organization" {
							return identity, nil
						}
						return profile(ctx, token)
					}
				}
				live, _ := h.manager.inspect(context.Background())
				fields, _ := object(rawCredential(name))
				oauth, _ := object(fields["claudeAiOauth"])
				oauth["accessToken"] = encoded("at-" + name + "-" + replacement)
				oauth["refreshToken"] = encoded("rt-" + name + "-" + replacement)
				fields["claudeAiOauth"] = encoded(oauth)
				fields["mcpOAuth"] = encoded(map[string]string{"generation": "new-live-integration"})
				live.File.Data = encoded(fields)
				if keychain {
					live.Keychain.Data = live.File.Data
				}
				config, _ := object(live.Config.Data)
				config["oauthAccount"] = encoded(identity)
				config["newProjectSetting"] = encoded("preserve")
				live.Config.Data = encoded(config)
				if err := h.manager.writeSnapshot(context.Background(), live); err != nil {
					t.Fatal(err)
				}
				restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
				captured, err := restarted.CaptureAndStore(context.Background(), func(a *store.Account) error {
					if a.ID == "" {
						a.ID = restarted.ExistingID(identity)
						if a.ID == "" {
							a.ID = "claude-new-organization"
						}
					}
					return h.accounts.Save(*a)
				})
				if err != nil {
					t.Fatal(err)
				}
				current, _ := restarted.inspect(context.Background())
				if !reflectSnapshots(live, current) || captured.Token.RefreshToken != "rt-"+name+"-"+replacement || h.grants != 1 {
					t.Fatal("recovery changed the verified replacement login or reused a grant")
				}
				files, err := os.ReadDir(filepath.Join(restarted.dataRoot, "generations"))
				if err != nil {
					t.Fatal(err)
				}
				archived := false
				for _, file := range files {
					value, _ := readPrivate(filepath.Join(restarted.dataRoot, "generations", file.Name()))
					archived = archived || bytes.Contains(value.Data, []byte(`"refresh_token":"rt-successor"`))
				}
				if !archived {
					t.Fatal("displaced issued successor was not archived")
				}
				if replacement == "other-organization" {
					original, _ := h.accounts.Get("claude-a")
					if accountIdentity(original).OrganizationUUID != "org-a" || original.Token.RefreshToken != "rt-successor" {
						t.Fatal("new organization replaced the original organization's issued generation")
					}
				}
			})
		}
	}
}

func TestNativeRefreshRecoveryPreservesUpdatedSharedCredentials(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	h.keys.writeErr = errors.New("fixture interrupted native write")
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
		t.Fatal("fixture did not retain native successor")
	}
	fields, _ := object(h.keys.values[h.paths.Service])
	fields["mcpOAuth"] = encoded(map[string]string{"generation": "new-live-secret"})
	delete(fields, "pluginSecrets")
	h.keys.values[h.paths.Service] = encoded(fields)
	h.keys.writeErr = nil
	a, _ = h.accounts.Get(a.ID)
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	live, _ := restarted.inspect(context.Background())
	updated, _ := object(live.credential())
	if !bytes.Contains(updated["mcpOAuth"], []byte("new-live-secret")) || len(updated["pluginSecrets"]) != 0 || a.Token.RefreshToken != "rt-successor" || h.grants != 1 {
		t.Fatal("native recovery reverted live shared credentials or their absence")
	}
}

func directoryInfo(fd int) (os.FileInfo, error) {
	copy, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(copy), "fixture-directory")
	defer f.Close()
	return f.Stat()
}

func TestRefreshIntentIsDurableBeforeGrant(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	var synced []os.FileInfo
	intentDurable := false
	original := syncDirectoryFD
	syncDirectoryFD = func(fd int) error {
		info, err := directoryInfo(fd)
		if err != nil {
			return err
		}
		synced = append(synced, info)
		if parent, err := os.Stat(filepath.Dir(h.manager.intentPath(a.ID))); err == nil && os.SameFile(info, parent) {
			var entry unix.Stat_t
			intentDurable = unix.Fstatat(fd, a.ID+".json", &entry, unix.AT_SYMLINK_NOFOLLOW) == nil
		}
		return original(fd)
	}
	t.Cleanup(func() { syncDirectoryFD = original })
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if !intentDurable {
			t.Fatal("grant started before the intent rename was directory-synced")
		}
		for _, path := range []string{filepath.Dir(h.manager.dataRoot), h.manager.dataRoot, filepath.Dir(h.manager.intentPath(a.ID))} {
			wanted, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, info := range synced {
				found = found || os.SameFile(wanted, info)
			}
			if !found {
				t.Fatalf("new recovery directory ancestry was not durable: %s", path)
			}
		}
		return h.manager.grant(ctx, a)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorySyncFailurePreventsGrant(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	original := syncDirectoryFD
	syncDirectoryFD = func(fd int) error {
		info, err := directoryInfo(fd)
		if err != nil {
			return err
		}
		parent, err := os.Stat(filepath.Dir(h.manager.intentPath(a.ID)))
		var entry unix.Stat_t
		if err == nil && os.SameFile(info, parent) && unix.Fstatat(fd, a.ID+".json", &entry, unix.AT_SYMLINK_NOFOLLOW) == nil {
			return errors.New("fixture intent directory fsync failed")
		}
		return original(fd)
	}
	t.Cleanup(func() { syncDirectoryFD = original })
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil || h.grants != 0 {
		t.Fatal("a non-durable consume fence permitted a grant")
	}
}

func TestSuccessorAccountIsDurableBeforeRecoveryCleanup(t *testing.T) {
	h := newHarness(t, false)
	a, _ := h.accounts.Get("claude-b")
	accountDir := filepath.Join(filepath.Dir(h.manager.dataRoot), "accounts")
	accountDurable := false
	intentSeen := false
	original := syncDirectoryFD
	syncDirectoryFD = func(fd int) error {
		info, err := directoryInfo(fd)
		if err != nil {
			return err
		}
		if parent, err := os.Stat(accountDir); err == nil && os.SameFile(info, parent) {
			stored, err := h.accounts.Get(a.ID)
			accountDurable = err == nil && stored.Token.RefreshToken == "rt-successor"
		}
		if parent, err := os.Stat(filepath.Dir(h.manager.intentPath(a.ID))); err == nil && os.SameFile(info, parent) {
			_, err := os.Stat(h.manager.intentPath(a.ID))
			if err == nil {
				intentSeen = true
			} else if intentSeen && os.IsNotExist(err) && !accountDurable {
				return errors.New("fixture detected cleanup before account directory durability")
			}
		}
		return original(fd)
	}
	t.Cleanup(func() { syncDirectoryFD = original })
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err != nil {
		t.Fatal(err)
	}
	if !accountDurable {
		t.Fatal("issued account generation was not directory-synced")
	}
}

type preflightKeychain struct {
	*memoryKeychain
	validate func() error
}

func (k preflightKeychain) Validate(string, []byte) error { return k.validate() }

func replaceFixtureLock(t *testing.T, path string) {
	t.Helper()
	if err := os.Rename(path, path+"-displaced"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestLockCompromiseBeforeGrantPreventsConsumption(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	lock := filepath.Join(h.paths.ConfigHome, ".oauth_refresh.lock")
	h.manager.keys = preflightKeychain{h.keys, func() error {
		replaceFixtureLock(t, lock)
		return nil
	}}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); !errors.Is(err, ErrConflict) {
		t.Fatalf("lost refresh lock was not fenced: %v", err)
	}
	if h.grants != 0 || h.keys.writes != 0 {
		t.Fatal("compromised lock permitted refresh consumption or native writes")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("release removed the replacement owner's lock")
	}
}

func TestLockCompromiseCancelsInFlightGrant(t *testing.T) {
	h := newHarness(t, false)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	canceled := false
	err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, _ *store.Account) error {
		replaceFixtureLock(t, filepath.Join(h.paths.ConfigHome, ".oauth_refresh.lock"))
		select {
		case <-ctx.Done():
			canceled = true
			return ctx.Err()
		case <-time.After(4 * time.Second):
			return errors.New("fixture grant was not canceled on lock compromise")
		}
	})
	if err == nil || !canceled {
		t.Fatalf("compromised lock did not cancel the request: %v", err)
	}
	intent, readErr := readPrivate(h.manager.intentPath(a.ID))
	if readErr != nil || !intent.Exists {
		t.Fatal("uncertain canceled grant lost its consume fence")
	}
}

func TestLockParentReplacementPreventsGrant(t *testing.T) {
	h := newHarness(t, true)
	expireNativeFixture(t, h)
	a, _ := h.accounts.Get("claude-a")
	h.manager.keys = preflightKeychain{h.keys, func() error {
		if err := os.Rename(h.paths.ConfigHome, h.paths.ConfigHome+"-displaced"); err != nil {
			return err
		}
		if err := os.Mkdir(h.paths.ConfigHome, 0700); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(h.paths.ConfigHome+"-displaced", ".credentials.json"))
		if err != nil {
			return err
		}
		return os.WriteFile(h.paths.CredentialsFile, raw, 0600)
	}}
	if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); !errors.Is(err, ErrConflict) || h.grants != 0 {
		t.Fatalf("detached lock parent permitted a grant: %v", err)
	}
}

type compromisingKeychain struct {
	*memoryKeychain
	write func()
}

func (k compromisingKeychain) Write(ctx context.Context, service string, value []byte) error {
	if err := k.memoryKeychain.Write(ctx, service, value); err != nil {
		return err
	}
	k.write()
	return nil
}

func TestLockCompromiseDuringSwitchPreventsFurtherMutation(t *testing.T) {
	h := newHarness(t, true)
	before, _ := h.manager.inspect(context.Background())
	h.manager.keys = compromisingKeychain{h.keys, func() {
		replaceFixtureLock(t, filepath.Join(h.paths.ConfigHome, ".oauth_refresh.lock"))
	}}
	commits := 0
	if _, err := h.manager.Switch(context.Background(), "claude-b", func() error { commits++; return nil }); err == nil {
		t.Fatal("switch accepted compromised lock ownership")
	}
	file, _ := readPrivate(h.paths.CredentialsFile)
	config, _ := readPrivate(h.paths.ConfigFile)
	if commits != 0 || !bytes.Equal(file.Data, before.File.Data) || !bytes.Equal(config.Data, before.Config.Data) || h.keys.writes != 1 {
		t.Fatal("switch or rollback mutated native stores after ownership was lost")
	}
	journal, err := readRecovery(h.manager.pendingPath())
	if err != nil || !journal.Exists {
		t.Fatal("compromised partial switch lost recovery journal")
	}
}

func TestLockHeartbeatFailureCancelsOwner(t *testing.T) {
	h := newHarness(t, false)
	ctx, release, err := h.manager.locks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	set := ctx.Value(lockContextKey{}).(*lockSet)
	lease := set.leases[0]
	lease.mu.Lock()
	err = lease.file.Close()
	lease.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrConflict) {
			t.Fatal("heartbeat failure did not report compromised ownership")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("failed heartbeat left protected context alive")
	}
}

func TestExpiredAcquisitionNeverClaimsLockOwnership(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	lease, err := acquireDirectory(ctx, filepath.Join(t.TempDir(), "fixture.lock"), time.Nanosecond, time.Millisecond, cancel)
	if lease != nil {
		lease.release()
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("acquisition beyond the stale interval claimed potentially replaced ownership: %v", err)
	}
}

func TestIssuedInactiveSuccessorRepairsNativePredecessorBeforeAdoption(t *testing.T) {
	for _, operation := range []string{"synchronize", "capture"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t, true)
			a, _ := h.accounts.Get("claude-b")
			h.accounts.failSave = true
			if err := h.manager.Refresh(context.Background(), &a, h.manager.grant); err == nil {
				t.Fatal("fixture did not retain successor")
			}
			h.accounts.failSave = false
			current, _ := h.manager.inspect(context.Background())
			current.Config.Data = encoded(map[string]any{"oauthAccount": identityFor("b")})
			current.Keychain.Data, current.File.Data = rawCredential("b"), rawCredential("b")
			if err := h.manager.writeSnapshot(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if operation == "synchronize" {
				a, _ = h.accounts.Get(a.ID)
				if _, err := h.manager.Synchronize(context.Background(), &a); err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				a, err = h.manager.CaptureAndStore(context.Background(), func(a *store.Account) error {
					if a.ID == "" {
						a.ID = h.manager.ExistingID(identityFor("b"))
					}
					return h.accounts.Save(*a)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			live, _ := h.manager.inspect(context.Background())
			oauth, _ := parseOAuth(live.credential())
			if a.Token.RefreshToken != "rt-successor" || oauth.RefreshToken != "rt-successor" || h.grants != 1 {
				t.Fatal("live consumed predecessor replaced the retained successor")
			}
		})
	}
}

func TestPendingInactiveAccountRepairsNativePredecessorAfterRestart(t *testing.T) {
	h := newHarness(t, true)
	a, _ := h.accounts.Get("claude-b")
	blocked := filepath.Join(h.manager.dataRoot, "successors")
	if err := h.manager.Refresh(context.Background(), &a, func(ctx context.Context, a *store.Account) error {
		if err := h.manager.grant(ctx, a); err != nil {
			return err
		}
		return os.WriteFile(blocked, []byte("fixture sidecar unavailable"), 0600)
	}); err == nil {
		t.Fatal("fixture did not leave a pending account successor")
	}
	a, _ = h.accounts.Get(a.ID)
	if !a.ClaudeCodeRefreshPending {
		t.Fatal("issued account fallback was not retained")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	live, _ := h.manager.inspect(context.Background())
	live.Config.Data = encoded(map[string]any{"oauthAccount": identityFor("b")})
	live.Keychain.Data, live.File.Data = rawCredential("b"), rawCredential("b")
	if err := h.manager.writeSnapshot(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	restarted := New(h.paths, filepath.Dir(h.manager.dataRoot), h.accounts, h.keys, h.manager.profile, h.manager.grant)
	if _, err := restarted.Synchronize(context.Background(), &a); err != nil {
		t.Fatal(err)
	}
	current, _ := restarted.inspect(context.Background())
	oauth, _ := parseOAuth(current.credential())
	if a.Token.RefreshToken != "rt-successor" || oauth.RefreshToken != "rt-successor" || h.grants != 1 {
		t.Fatal("pending account recovery adopted a consumed native predecessor")
	}
}
