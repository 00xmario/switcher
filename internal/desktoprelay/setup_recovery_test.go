package desktoprelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestSetupRestartAndPortChangePreserveScopeAndBaseline(t *testing.T) {
	m, cfg, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"env":{"HTTPS_PROXY":"prior","KEEP":"yes"},"precision":1.0000000000000000001}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	oldEnv := setupEnv(t, path)
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Occupy the old ephemeral address so the new listener must use another.
	u, _ := url.Parse(oldEnv["HTTPS_PROXY"])
	l, err := net.Listen("tcp4", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	before := setupRead(t, path)
	if other.SetupStatus(path).Condition != "unavailable" || !bytes.Equal(before, setupRead(t, path)) {
		t.Fatal("lazy status recovered or ignored inactive relay")
	}
	if err = other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if other.SetupStatus(path).Condition != "changed" || !bytes.Equal(before, setupRead(t, path)) {
		t.Fatal("resume silently rewrote native settings")
	}
	next, err := other.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if next.ScopeID != s.ScopeID || next.BackupPath != s.BackupPath || len(other.Scopes()) != 1 || setupEnv(t, path)["HTTPS_PROXY"] == oldEnv["HTTPS_PROXY"] {
		t.Fatal("port repair replaced ownership")
	}
	if _, err = other.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if setupEnv(t, path)["HTTPS_PROXY"] != "prior" || !bytes.Equal(original, setupRead(t, s.BackupPath)) {
		t.Fatal("restart lost baseline")
	}
}

func TestSetupReloadRestoresExactOriginalOwnedRawValues(t *testing.T) {
	m, cfg, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := `{"nested" : [ 900719925474099312345, 1e+99 ]}`
	if err := os.WriteFile(path, []byte(`{"env":{"HTTPS_PROXY":`+raw+`,"NODE_EXTRA_CA_CERTS":null}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background())
	if _, err = next.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(setupRead(t, path), []byte(raw)) {
		t.Fatal("journal serialization changed original raw env value")
	}
}

func TestSetupCorruptOwnedCapabilityFailsClosedWithoutSettingsWrites(t *testing.T) {
	m, cfg, path := setupFixture(t)
	if _, err := m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	before := setupRead(t, path)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(cfg.DataRoot, "state.json")
	var state struct {
		Version     int                                   `json:"version"`
		Enabled     bool                                  `json:"enabled"`
		Certificate string                                `json:"certificate"`
		PrivateKey  string                                `json:"private_key"`
		Scopes      map[string]json.RawMessage            `json:"scopes"`
		Sessions    map[string]json.RawMessage            `json:"sessions"`
		Setups      map[string]map[string]json.RawMessage `json:"setups"`
	}
	if err := json.Unmarshal(setupRead(t, statePath), &state); err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(state.Setups[path]["owned_env"], &env); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(env["HTTPS_PROXY"])
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(u.User.Username(), "wrong-capability")
	env["HTTPS_PROXY"] = u.String()
	state.Setups[path]["owned_env"], _ = json.Marshal(env)
	b, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, b, 0600); err != nil {
		t.Fatal(err)
	}
	next, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background())
	if got := next.SetupStatus(path); got.Condition != "unavailable" {
		t.Fatal("corrupt journal accepted")
	}
	if _, err = next.Configure(context.Background(), path); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatal("corrupt journal did not fail closed")
	}
	if !bytes.Equal(before, setupRead(t, path)) {
		t.Fatal("corrupt journal overwrote config")
	}
}

func TestSetupFaultBeforeSettingsSyncLeavesRecoverableJournal(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "configure", true: "restore"}[restore], func(t *testing.T) {
			cfg := fixtureConfig(t)
			var fail atomic.Bool
			cfg.SyncSetupFile = func(f *os.File) error {
				if fail.Load() {
					return errors.New("private-fixture-token")
				}
				return f.Sync()
			}
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
			original := []byte(`{"env":null,"model":"kept"}`)
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			operation := m.Configure
			if restore {
				if _, err = m.Configure(context.Background(), path); err != nil {
					t.Fatal(err)
				}
				operation = m.RestoreSetup
			}
			before := setupRead(t, path)
			fail.Store(true)
			s, err := operation(context.Background(), path)
			if !errors.Is(err, desktoprelay.ErrUnavailable) || s.Condition != "pending" || s.Configured || !bytes.Equal(before, setupRead(t, path)) {
				t.Fatal("pre-write failure lost pending intent")
			}
			state := setupRead(t, filepath.Join(cfg.DataRoot, "state.json"))
			for i := 0; i < 3; i++ {
				if m.SetupStatus(path).Condition != "pending" {
					t.Fatal("status repaired a pending setup")
				}
			}
			if !bytes.Equal(state, setupRead(t, filepath.Join(cfg.DataRoot, "state.json"))) {
				t.Fatal("status wrote journal")
			}
			if err = m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			cfg.SyncSetupFile = nil
			reloaded, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close(context.Background())
			if reloaded.SetupStatus(path).Condition != "pending" {
				t.Fatal("read-only reload lost pending intent")
			}
			if restore {
				if _, err = reloaded.RestoreSetup(context.Background(), path); err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(setupRead(t, path), []byte(`"env":null`)) {
					t.Fatal("restore retry lost null baseline")
				}
			} else {
				next, err := reloaded.Configure(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				if next.ScopeID != s.ScopeID || next.BackupPath != s.BackupPath || len(reloaded.Scopes()) != 1 {
					t.Fatal("retry leaked scope/baseline")
				}
				if _, err = reloaded.RestoreSetup(context.Background(), path); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(original, setupRead(t, s.BackupPath)) {
				t.Fatal("retry overwrote original backup")
			}
		})
	}
}

func TestSetupCancellationDuringBackupDoesNotPublishOwnership(t *testing.T) {
	cfg := fixtureConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.SyncDirectory = func(f *os.File) error {
		if filepath.Base(f.Name()) == "backups" {
			cancel()
		}
		return f.Sync()
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
	if _, err = m.Configure(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("backup cancellation ignored")
	}
	if len(m.Scopes()) != 0 || m.SetupStatus(path).Condition != "not_configured" {
		t.Fatal("canceled configure published ownership before final journal fence")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("canceled configure wrote settings")
	}
}

func TestSetupJournalCommitFailureBeforeConfigRecoversOneScope(t *testing.T) {
	cfg := fixtureConfig(t)
	var armed atomic.Bool
	cfg.SyncDirectory = func(f *os.File) error {
		if filepath.Base(f.Name()) == "relay" && armed.Swap(false) {
			return errors.New("fixture intent fsync failure")
		}
		return f.Sync()
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
	original := []byte(`{"env":null,"model":"original"}`)
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	s, err := m.Configure(context.Background(), path)
	if !errors.Is(err, desktoprelay.ErrUnavailable) || s.Condition != "pending" || !bytes.Equal(original, setupRead(t, path)) || m.Status().Listening {
		t.Fatal("uncertain intent wrote config or acknowledged")
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.SyncDirectory = nil
	next, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background())
	again, err := next.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ScopeID != s.ScopeID || again.BackupPath != s.BackupPath || len(next.Scopes()) != 1 {
		t.Fatal("intent retry lost original scope or baseline")
	}
	if _, err = next.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, setupRead(t, s.BackupPath)) {
		t.Fatal("intent recovery lost exact original bytes")
	}
}

func TestSetupPostRenameAndFinalMarkFailuresNeverAcknowledge(t *testing.T) {
	for _, stage := range []string{"settings", "mark"} {
		for _, restore := range []bool{false, true} {
			t.Run(stage+map[bool]string{false: "Configure", true: "Restore"}[restore], func(t *testing.T) {
				cfg := fixtureConfig(t)
				var armed atomic.Bool
				var writes atomic.Int32
				cfg.SyncDirectory = func(f *os.File) error {
					if armed.Load() {
						isSettings := f.Name() == filepath.Dir(cfg.DataRoot)
						if (stage == "settings" && isSettings) || (stage == "mark" && !isSettings && filepath.Base(f.Name()) == "relay" && writes.Add(1) == 2) {
							return errors.New("sensitive-fixture-error")
						}
					}
					return f.Sync()
				}
				m, err := desktoprelay.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer m.Close(context.Background())
				path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
				if err = os.WriteFile(path, []byte(`{"env":{"HTTPS_PROXY":"baseline"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = m.Configure(context.Background(), path); err != nil {
					t.Fatal(err)
				}
				operation := m.Configure
				if restore {
					operation = m.RestoreSetup
				} else {
					// Start from a pending pre-write intent, then fail the retry after
					// rename or while marking the canonical journal.
					if _, err = m.RestoreSetup(context.Background(), path); err != nil {
						t.Fatal(err)
					}
				}
				armed.Store(true)
				s, err := operation(context.Background(), path)
				if !errors.Is(err, desktoprelay.ErrUnavailable) || s.Condition != "pending" || s.Configured || m.Status().Listening {
					t.Fatalf("uncertain write acknowledged or lost pending recovery: %+v %v", s, err)
				}
				var typed *desktoprelay.SetupError
				if !errors.As(err, &typed) || typed.ErrorCode() != "setup_unavailable" {
					t.Fatal("durability failure has no typed setup code")
				}
				public, _ := json.Marshal(s)
				if bytes.Contains(public, []byte("sensitive-fixture-error")) || bytes.Contains([]byte(err.Error()), []byte("sensitive-fixture-error")) {
					t.Fatal("fault error leaked")
				}
				if err = m.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				cfg.SyncDirectory = nil
				next, err := desktoprelay.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer next.Close(context.Background())
				if restore {
					if _, err = next.RestoreSetup(context.Background(), path); err != nil {
						t.Fatal(err)
					}
					if setupEnv(t, path)["HTTPS_PROXY"] != "baseline" {
						t.Fatal("post-rename restore rolled back")
					}
				} else {
					again, err := next.Configure(context.Background(), path)
					if err != nil {
						t.Fatal(err)
					}
					if again.ScopeID != s.ScopeID || len(next.Scopes()) != 1 {
						t.Fatal("recovery leaked scope")
					}
					if _, err = next.RestoreSetup(context.Background(), path); err != nil {
						t.Fatal(err)
					}
					if setupEnv(t, path)["HTTPS_PROXY"] != "baseline" {
						t.Fatal("recovery replaced baseline")
					}
				}
			})
		}
	}
}
