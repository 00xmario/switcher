package desktoprelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestSetupRestoreRecoverySyncsIdenticalSettingsBeforeReleasingOwnership(t *testing.T) {
	for _, failure := range []string{"none", "file", "directory"} {
		t.Run(failure, func(t *testing.T) {
			cfg := fixtureConfig(t)
			path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
			var phase, fileSyncs, directorySyncs atomic.Int32
			cfg.SyncSetupFile = func(f *os.File) error {
				if phase.Load() == 2 {
					fileSyncs.Add(1)
					if failure == "file" {
						return errors.New("fixture recovery file sync failure")
					}
				}
				return f.Sync()
			}
			cfg.SyncDirectory = func(f *os.File) error {
				if f.Name() == filepath.Dir(path) {
					switch phase.Load() {
					case 1:
						return errors.New("fixture original restore directory sync failure")
					case 2:
						directorySyncs.Add(1)
						if failure == "directory" {
							return errors.New("fixture recovery directory sync failure")
						}
					}
				}
				return f.Sync()
			}
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			original := []byte(`{"env":null,"model":"kept"}`)
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			setup, err := m.Configure(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			phase.Store(1)
			status, err := m.RestoreSetup(context.Background(), path)
			if !errors.Is(err, desktoprelay.ErrUnavailable) || status.Condition != "pending" {
				t.Fatal("fixture did not leave post-rename restore pending")
			}
			after := setupRead(t, path)
			if !bytes.Equal(after, append(append([]byte(nil), original...), '\n')) {
				t.Fatal("fixture restore did not leave identical target bytes")
			}
			if err = m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			recovery, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer recovery.Close(context.Background())
			if recovery.SetupStatus(path).Condition != "pending" || fileSyncs.Load() != 0 || directorySyncs.Load() != 0 {
				t.Fatal("read-only status recovered pending restore")
			}
			phase.Store(2)
			status, err = recovery.RestoreSetup(context.Background(), path)
			if fileSyncs.Load() != 1 {
				t.Fatal("identical pending restore skipped file fsync")
			}
			wantDirectorySyncs := int32(1)
			if failure == "file" {
				wantDirectorySyncs = 0
			}
			if directorySyncs.Load() != wantDirectorySyncs {
				t.Fatal("pending restore skipped directory fsync or synced after file failure")
			}
			if !bytes.Equal(after, setupRead(t, path)) || !bytes.Equal(original, setupRead(t, setup.BackupPath)) {
				t.Fatal("recovery changed restored bytes or baseline backup")
			}
			if failure == "none" {
				if err != nil || status.Condition != "not_configured" {
					t.Fatalf("durable recovery: %+v %v", status, err)
				}
			} else {
				if !errors.Is(err, desktoprelay.ErrUnavailable) || status.Condition != "pending" {
					t.Fatal("failed recovery released setup ownership")
				}
				for _, operation := range []func() error{
					func() error { return recovery.Stop(context.Background()) },
					func() error { return recovery.DeleteScope(setup.ScopeID) },
				} {
					err := operation()
					var own *desktoprelay.SetupError
					if !errors.Is(err, desktoprelay.ErrConflict) || !errors.As(err, &own) || own.ErrorCode() != "setup_owned" {
						t.Fatal("non-durable restore allowed stop or revocation")
					}
				}
				if err = recovery.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				cfg.SyncDirectory = nil
				cfg.SyncSetupFile = nil
				final, err := desktoprelay.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer final.Close(context.Background())
				if final.SetupStatus(path).Condition != "pending" {
					t.Fatal("failed recovery lost durable pending ownership")
				}
				if _, err = final.RestoreSetup(context.Background(), path); err != nil {
					t.Fatal(err)
				}
				if err = final.DeleteScope(setup.ScopeID); err != nil {
					t.Fatal(err)
				}
				if err = final.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSetupEquivalentEscapedOwnedStringsRetainConfigurationAndRestoreRawBaseline(t *testing.T) {
	for _, encoding := range []string{"slash", "unicode"} {
		t.Run(encoding, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.DataRoot = filepath.Join(filepath.Dir(cfg.DataRoot), "relay&fixture")
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
			priorProxy := `"https:\/\/corporate.example\/proxy?tag=\u0026"`
			priorCA := `"certs\/prior-\u0026.pem"`
			original := []byte(`{"env":{"HTTPS_PROXY":` + priorProxy + `,"NODE_EXTRA_CA_CERTS":` + priorCA + `,"KEEP":"before"},"model":"before","precision":900719925474099312345,"raw":{"num" : [ 1e+99, 1.0000000000000000001 ]}}`)
			if err = os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			setup, err := m.Configure(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			env := setupEnv(t, path)
			edited := setupRead(t, path)
			for _, key := range []string{"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS"} {
				raw, err := json.Marshal(env[key])
				if err != nil {
					t.Fatal(err)
				}
				escaped := string(raw)
				if encoding == "slash" {
					escaped = strings.ReplaceAll(escaped, "/", `\/`)
				} else {
					escaped = strings.ReplaceAll(escaped, `\u0026`, "&")
					escaped = strings.Replace(escaped, "/", `\u002f`, 1)
					if key == "HTTPS_PROXY" {
						escaped = strings.Replace(escaped, "http", `\u0068ttp`, 1)
					}
				}
				edited = bytes.Replace(edited, raw, []byte(escaped), 1)
			}
			edited = bytes.Replace(edited, []byte(`"model":"before"`), []byte(`"model":"external","future":1e+123`), 1)
			edited = bytes.Replace(edited, []byte(`"KEEP":"before"`), []byte(`"KEEP":"external","ADDED":"keep"`), 1)
			if err = os.WriteFile(path, edited, 0600); err != nil {
				t.Fatal(err)
			}
			if status := m.SetupStatus(path); status.Condition != "configured" || !status.Configured {
				t.Fatalf("equivalent env encoding reported changed: %+v", status)
			}
			if !bytes.Equal(edited, setupRead(t, path)) {
				t.Fatal("status normalized unrelated edits")
			}
			again, err := m.Configure(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if again.ScopeID != setup.ScopeID || again.BackupPath != setup.BackupPath || !bytes.Equal(edited, setupRead(t, path)) {
				t.Fatal("equivalent strings replaced scope, baseline, or external edits")
			}
			if _, err = m.RestoreSetup(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			restored := setupRead(t, path)
			for _, raw := range []string{priorProxy, priorCA, `"KEEP":"external"`, `"ADDED":"keep"`, `"model":"external"`, `"future":1e+123`, `"precision":900719925474099312345`, `{"num" : [ 1e+99, 1.0000000000000000001 ]}`} {
				if !bytes.Contains(restored, []byte(raw)) {
					t.Fatal("restore changed original raw values or unrelated external edits")
				}
			}
			if !bytes.Equal(original, setupRead(t, setup.BackupPath)) {
				t.Fatal("escape handling changed exact baseline backup")
			}
		})
	}
}

func TestSetupUnavailableListenerRetainsReadOnlyRestoreEvidence(t *testing.T) {
	cfg := fixtureConfig(t)
	var calls atomic.Int32
	cfg.Source = &setupNoSource{calls: &calls}
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
	setup, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close(context.Background())
	statePath := filepath.Join(cfg.DataRoot, "state.json")
	stateBefore, settingsBefore := setupRead(t, statePath), setupRead(t, path)
	for i := 0; i < 3; i++ {
		status := reloaded.SetupStatus(path)
		if status.Condition != "unavailable" || status.Configured || status.ScopeID != setup.ScopeID || status.BackupPath != setup.BackupPath {
			t.Fatal("unavailable listener hid UI restore evidence")
		}
	}
	if !bytes.Equal(stateBefore, setupRead(t, statePath)) || !bytes.Equal(settingsBefore, setupRead(t, path)) {
		t.Fatal("read-only restore evidence mutated settings or state")
	}
	if _, err = os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("read-only restore evidence created settings lock")
	}
	if reloaded.Status().Listening || calls.Load() != 0 {
		t.Fatal("status started listener or consulted credentials")
	}
	status, err := reloaded.RestoreSetup(context.Background(), path)
	if err != nil || status.Condition != "not_configured" || !status.RestartRequired {
		t.Fatalf("restore with unavailable listener: %+v %v", status, err)
	}
	if reloaded.Status().Listening || calls.Load() != 0 || !bytes.Equal(original, setupRead(t, setup.BackupPath)) {
		t.Fatal("restore started listener, consulted credentials or modified backup")
	}
}
