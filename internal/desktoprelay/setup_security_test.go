package desktoprelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestSetupStatusCreatesNoFilesAndNeverConsultsSource(t *testing.T) {
	m, cfg, path := setupFixture(t)
	for _, p := range []string{"", "relative/settings.json", path} {
		got := m.SetupStatus(p)
		want := "unavailable"
		if p == path {
			want = "not_configured"
		}
		if got.Condition != want || got.Configured || got.SettingsPath != p {
			t.Fatalf("read-only status: %+v", got)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(cfg.DataRoot))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("status created a store, settings, backup or lock")
	}
	if _, err := m.Configure(context.Background(), ""); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatal("empty configure path resolved a default")
	}
	if _, err := m.RestoreSetup(context.Background(), ""); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatal("empty restore path resolved a default")
	}
	entries, _ = os.ReadDir(filepath.Dir(cfg.DataRoot))
	if len(entries) != 0 {
		t.Fatal("invalid paths wrote files")
	}
}

func TestSetupInvalidSettingsNeverOverwriteOrStart(t *testing.T) {
	for name, input := range map[string]string{
		"malformed": `{"env":`, "null": `null`, "array": `[]`,
		"duplicate": `{"env":{},"env":{}}`, "nestedDuplicate": `{"permissions":{"x":1,"x":2}}`,
		"envArray": `{"env":[]}`, "envScalar": `{"env":7}`, "trailing": `{} {}`,
		"oversized": `{"large":"` + strings.Repeat("x", 1<<20) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			m, cfg, path := setupFixture(t)
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			if got := m.SetupStatus(path); got.Condition != "unavailable" {
				t.Fatal("invalid settings reported usable")
			}
			if _, err := m.Configure(context.Background(), path); !errors.Is(err, desktoprelay.ErrUnavailable) {
				t.Fatal("invalid settings accepted")
			}
			if !bytes.Equal([]byte(input), setupRead(t, path)) || m.Status().Listening {
				t.Fatal("invalid file overwritten or listener started")
			}
			if _, err := os.Stat(cfg.DataRoot); !os.IsNotExist(err) {
				t.Fatal("invalid config created relay state")
			}
			if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatal("lock leaked")
			}
		})
	}
}

func TestSetupRejectsSymlinksHardlinksAndUnsafeModes(t *testing.T) {
	for _, kind := range []string{"fileSymlink", "parentSymlink", "hardlink", "writableFile", "writableParent", "lockSymlink"} {
		t.Run(kind, func(t *testing.T) {
			m, cfg, path := setupFixture(t)
			base := filepath.Dir(cfg.DataRoot)
			outside := filepath.Join(base, "untouched.json")
			original := []byte(`{"env":{"HTTPS_PROXY":"do-not-touch"}}`)
			if err := os.WriteFile(outside, original, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "parentSymlink" {
				real := filepath.Join(base, "real")
				if err := os.Mkdir(real, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(real, "settings.json"), original, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "fileSymlink":
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(outside, path); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.WriteFile(path, original, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "writableFile" {
					if err := os.Chmod(path, 0666); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "writableParent" {
					if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "lockSymlink" {
					if err := os.Symlink(base, path+".lock"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := m.Configure(context.Background(), path); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if !bytes.Equal(original, setupRead(t, outside)) || m.Status().Listening {
				t.Fatal("unsafe traversal mutated target or started listener")
			}
		})
	}
}

func TestSetupSettingsLockHonorsCancellationAndExternalWriter(t *testing.T) {
	m, _, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"before"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.Configure(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored context: %v", err)
	}
	if m.Status().Listening {
		t.Fatal("locked settings started relay")
	}
	if err := os.WriteFile(path, []byte(`{"model":"external","precision":9007199254740993}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(setupRead(t, path), []byte(`"model":"external"`)) {
		t.Fatal("ignored external writer's update")
	}
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := m.RestoreSetup(ctx2, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("restore ignored lock cancellation")
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestSetupStaleLockCanRecoverAndStatusDoesNotRepairCA(t *testing.T) {
	m, cfg, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path+".lock", stale, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(cfg.DataRoot, "ca.pem")
	if err := os.Remove(ca); err != nil {
		t.Fatal(err)
	}
	before := setupRead(t, filepath.Join(cfg.DataRoot, "state.json"))
	next, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close(context.Background())
	_ = next.SetupStatus(path)
	if _, err = os.Stat(ca); !os.IsNotExist(err) {
		t.Fatal("status repaired CA export")
	}
	if _, err = os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("status created settings lock")
	}
	if !bytes.Equal(before, setupRead(t, filepath.Join(cfg.DataRoot, "state.json"))) {
		t.Fatal("status wrote persisted state")
	}
}

func TestSetupRestoreDeletedParentStillSerializesWithSettingsWriter(t *testing.T) {
	cfg := fixtureConfig(t)
	path := filepath.Join(filepath.Dir(cfg.DataRoot), "native", "settings.json")
	var armed atomic.Bool
	cfg.SyncDirectory = func(f *os.File) error {
		if armed.Load() && filepath.Base(f.Name()) == "relay" {
			info, err := os.Lstat(path + ".lock")
			if err != nil || !info.IsDir() {
				return errors.New("restore must hold settings lock while dropping ownership")
			}
		}
		return f.Sync()
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	if _, err = m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatalf("deleted-parent restore lacked config serialization: %v", err)
	}
	armed.Store(false)
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("restore resurrected deleted settings")
	}
}

func TestSetupRereadFenceAndFinalCancellation(t *testing.T) {
	for _, kind := range []string{"writer", "cancel", "parentSwap"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixtureConfig(t)
			path := filepath.Join(filepath.Dir(cfg.DataRoot), "native", "settings.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte(`{"model":"before"}`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg.SyncSetupFile = func(f *os.File) error {
				if err := f.Sync(); err != nil {
					return err
				}
				switch kind {
				case "writer":
					return os.WriteFile(path, []byte(`{"model":"external"}`), 0600)
				case "cancel":
					cancel()
				case "parentSwap":
					if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-old"); err != nil {
						return err
					}
					if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
						return err
					}
					return os.WriteFile(path, []byte(`{"model":"external"}`), 0600)
				}
				return nil
			}
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			_, err = m.Configure(ctx, path)
			want := desktoprelay.ErrConflict
			if kind == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("final fence: %v", err)
			}
			b := setupRead(t, path)
			if bytes.Contains(b, []byte("HTTPS_PROXY")) {
				t.Fatal("final fence overwrote settings")
			}
			if kind != "cancel" && !bytes.Contains(b, []byte(`"model":"external"`)) {
				t.Fatal("lost external edits")
			}
		})
	}
}

func TestSetupTempReplacementCannotPublishSymlinkOrHardlink(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixtureConfig(t)
			parent := filepath.Dir(cfg.DataRoot)
			path := filepath.Join(parent, "settings.json")
			original := []byte(`{"model":"original"}`)
			outside := filepath.Join(parent, "outside.json")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outside, []byte(`{"model":"outside"}`), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.SyncSetupFile = func(f *os.File) error {
				if err := f.Sync(); err != nil {
					return err
				}
				tmp := filepath.Join(parent, f.Name())
				if err := os.Remove(tmp); err != nil {
					return err
				}
				if kind == "symlink" {
					return os.Symlink(outside, tmp)
				}
				return os.Link(outside, tmp)
			}
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close(context.Background())
			if _, err = m.Configure(context.Background(), path); !errors.Is(err, desktoprelay.ErrConflict) {
				t.Fatalf("temp replacement accepted: %v", err)
			}
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || !bytes.Equal(original, setupRead(t, path)) {
				t.Fatal("published attacker-controlled temp inode")
			}
		})
	}
}

func TestSetupConcurrentClicksReuseOneScopeWithoutCredentials(t *testing.T) {
	cfg := fixtureConfig(t)
	var calls atomic.Int32
	cfg.Source = &setupNoSource{calls: &calls}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := m.Configure(context.Background(), path); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(m.Scopes()) != 1 || calls.Load() != 0 {
		t.Fatal("clicks leaked scopes or requested credentials")
	}
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

type setupNoSource struct{ calls *atomic.Int32 }

func (s *setupNoSource) Prepare(context.Context, string) (desktoprelay.Credential, error) {
	s.calls.Add(1)
	return desktoprelay.Credential{}, errors.New("forbidden fixture source")
}
func (s *setupNoSource) RefreshRejected(context.Context, string, string) (desktoprelay.Credential, error) {
	s.calls.Add(1)
	return desktoprelay.Credential{}, errors.New("forbidden fixture source")
}

func TestSetupManualProfileBecomesPreservedBaseline(t *testing.T) {
	m, _, path := setupFixture(t)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	manual, err := m.CreateScope("manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"env": manual.Env, "model": "native"})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ScopeID == manual.ID || len(m.Scopes()) != 2 {
		t.Fatal("manual profile ownership confused")
	}
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	env := setupEnv(t, path)
	if env["HTTPS_PROXY"] != manual.ProxyURL || env["NODE_EXTRA_CA_CERTS"] != manual.CAPath {
		t.Fatal("manual baseline lost")
	}
	if err = m.DeleteScope(s.ScopeID); err != nil {
		t.Fatal(err)
	}
}
