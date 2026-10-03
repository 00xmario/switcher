package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type installFixture struct {
	checker       *Checker
	ops           installOps
	current       string
	stage         string
	checksum      func(string, string) *string
	version       string
	smokeCalls    int
	execCalls     int
	requestedTemp string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	f := &installFixture{checker: New("0.5.5"), current: filepath.Join(t.TempDir(), "switcher"), version: "switcher 0.5.6\n"}
	f.checker.latest = "v0.5.6"
	if err := os.WriteFile(f.current, []byte("old fixture"), 0o711); err != nil {
		t.Fatal(err)
	}
	current, err := filepath.EvalSymlinks(f.current)
	if err != nil {
		t.Fatal(err)
	}
	f.current = current
	f.checksum = func(sum, asset string) *string {
		value := sum + "  " + asset + "\n"
		return &value
	}
	f.ops = installOps{
		tempDir: func(parent, pattern string) (string, error) {
			f.requestedTemp = parent
			// Even buggy implementations remain confined to fixture storage.
			dir, err := os.MkdirTemp(filepath.Dir(f.current), pattern)
			f.stage = dir
			return dir, err
		},
		download: func(ctx context.Context, tag, asset, dir string) error {
			data := []byte("new fixture")
			if err := os.WriteFile(filepath.Join(dir, asset), data, 0o600); err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if checksum := f.checksum(hex.EncodeToString(sum[:]), asset); checksum != nil {
				return os.WriteFile(filepath.Join(dir, asset+".sha256"), []byte(*checksum), 0o600)
			}
			return nil
		},
		smoke: func(ctx context.Context, path string) ([]byte, error) {
			f.smokeCalls++
			return []byte(f.version), nil
		},
		executable: func() (string, error) { return f.current, nil },
		rename:     os.Rename,
		chmod:      os.Chmod,
		syncDir:    syncDirectory,
		exec: func(path string) error {
			f.execCalls++
			return nil // No process is executed, replaced, or restarted.
		},
	}
	return f
}

func assertFixtureFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content || info.Mode().Perm() != mode {
		t.Errorf("%s: content=%q mode=%o, want %q and %o", path, data, info.Mode().Perm(), content, mode)
	}
}

func TestChecksumRequiresExactlyOneValidEntryBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string, string) *string
	}{
		{"missing", func(string, string) *string { return nil }},
		{"empty", func(string, string) *string { s := ""; return &s }},
		{"whitespace", func(string, string) *string { s := " \n\t"; return &s }},
		{"short digest", func(string, string) *string { s := "abcd"; return &s }},
		{"nonhex digest", func(string, string) *string { s := strings.Repeat("g", 64); return &s }},
		{"mismatch", func(string, string) *string { s := strings.Repeat("0", 64); return &s }},
		{"multiple entries", func(sum, asset string) *string { s := sum + "  " + asset + "\n" + sum + "  " + asset + "\n"; return &s }},
		{"wrong asset", func(sum, asset string) *string { s := sum + "  other-binary\n"; return &s }},
		{"extra fields", func(sum, asset string) *string { s := sum + "  " + asset + " extra"; return &s }},
		{"split entry", func(sum, asset string) *string { s := sum + "\n" + asset; return &s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.checksum = tc.make
			if err := f.checker.installAndRestart(f.ops); err == nil {
				t.Error("invalid checksum was accepted")
			}
			if f.smokeCalls != 0 || f.execCalls != 0 {
				t.Errorf("unverified binary reached execution hooks: smoke=%d exec=%d", f.smokeCalls, f.execCalls)
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
		})
	}
}

func TestValidChecksumFormats(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(string, string) *string
	}{
		{"digest only", func(sum, asset string) *string { return &sum }},
		{"shasum", func(sum, asset string) *string { s := sum + "  " + asset + "\n"; return &s }},
		{"binary marker and uppercase", func(sum, asset string) *string { s := strings.ToUpper(sum) + " *" + asset + "\r\n"; return &s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFixture(t)
			f.checksum = tc.make
			if err := f.checker.installAndRestart(f.ops); err != nil {
				t.Fatal(err)
			}
			assertFixtureFile(t, f.current, "new fixture", 0o755)
			assertFixtureFile(t, f.current+".previous", "old fixture", 0o711)
		})
	}
}

func TestDownloadedVersionMustMatchRelease(t *testing.T) {
	for _, output := range []string{"switcher 0.5.5\n", "", "0.5.6", "switcher 0.5.6\nextra output"} {
		t.Run(fmt.Sprintf("output=%q", output), func(t *testing.T) {
			f := newInstallFixture(t)
			f.version = output
			if err := f.checker.installAndRestart(f.ops); err == nil {
				t.Error("wrong version was accepted")
			}
			if f.execCalls != 0 {
				t.Error("wrong version reached restart hook")
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
		})
	}
}

func TestReplacementStagesBesideCurrentAndKeepsOriginalPresent(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.rename = func(from, to string) error {
		if from == f.current {
			return errors.New("original executable must remain in place before atomic replacement")
		}
		if _, err := os.Stat(f.current); err != nil {
			t.Errorf("original executable disappeared: %v", err)
		}
		if to == f.current && filepath.Dir(filepath.Dir(from)) != filepath.Dir(f.current) {
			return syscall.EXDEV
		}
		return os.Rename(from, to)
	}
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
	if f.requestedTemp != filepath.Dir(f.current) {
		t.Errorf("staging parent=%q, want %q", f.requestedTemp, filepath.Dir(f.current))
	}
	if _, err := os.Stat(f.stage); !os.IsNotExist(err) {
		t.Errorf("staging directory not cleaned: %v", err)
	}
	assertFixtureFile(t, f.current, "new fixture", 0o755)
}

func TestExecFailureRestoresOriginal(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.exec = func(path string) error {
		assertFixtureFile(t, path, "new fixture", 0o755)
		return syscall.EACCES
	}
	if err := f.checker.installAndRestart(f.ops); !errors.Is(err, syscall.EACCES) {
		t.Errorf("error=%v, want EACCES", err)
	}
	assertFixtureFile(t, f.current, "old fixture", 0o711)
}

func TestStageIsCleanedBeforeExec(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.exec = func(string) error {
		// A successful syscall.Exec never runs deferred cleanup.
		if _, err := os.Stat(f.stage); !os.IsNotExist(err) {
			t.Errorf("stage still present at exec boundary: %v", err)
		}
		return nil
	}
	if err := f.checker.installAndRestart(f.ops); err != nil {
		t.Fatal(err)
	}
}

func TestReplacementFailuresPreserveOriginal(t *testing.T) {
	for _, point := range []string{"chmod", "backup", "replacement", "post-swap sync"} {
		t.Run(point, func(t *testing.T) {
			f := newInstallFixture(t)
			fault := errors.New(point + " fixture failure")
			switch point {
			case "chmod":
				f.ops.chmod = func(string, os.FileMode) error { return fault }
			case "backup", "replacement":
				f.ops.rename = func(from, to string) error {
					if point == "backup" && to == f.current+".previous" || point == "replacement" && to == f.current && from != f.current+".previous" {
						return fault
					}
					return os.Rename(from, to)
				}
			case "post-swap sync":
				calls := 0
				f.ops.syncDir = func(string) error {
					calls++
					if calls == 2 {
						return fault
					}
					return nil
				}
			}
			if err := f.checker.installAndRestart(f.ops); !errors.Is(err, fault) {
				t.Errorf("error=%v, want %v", err, fault)
			}
			assertFixtureFile(t, f.current, "old fixture", 0o711)
			if f.execCalls != 0 {
				t.Error("failed replacement reached restart hook")
			}
		})
	}
}

func TestRollbackFailureRetainsBackupAndReportsBothErrors(t *testing.T) {
	f := newInstallFixture(t)
	rollbackError := errors.New("rollback fixture failure")
	f.ops.exec = func(string) error { return syscall.EACCES }
	f.ops.rename = func(from, to string) error {
		if from == f.current+".previous" && to == f.current {
			return rollbackError
		}
		return os.Rename(from, to)
	}
	err := f.checker.installAndRestart(f.ops)
	if !errors.Is(err, syscall.EACCES) || !errors.Is(err, rollbackError) || !strings.Contains(err.Error(), f.current+".previous") {
		t.Errorf("missing failure/recovery details: %v", err)
	}
	assertFixtureFile(t, f.current+".previous", "old fixture", 0o711)
}

func TestSmokeTestHasBoundedContext(t *testing.T) {
	f := newInstallFixture(t)
	f.ops.smoke = func(ctx context.Context, path string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Second {
			t.Error("smoke test lacks a 15-second deadline")
		}
		return nil, context.DeadlineExceeded
	}
	if err := f.checker.installAndRestart(f.ops); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error=%v, want deadline exceeded", err)
	}
	assertFixtureFile(t, f.current, "old fixture", 0o711)
}

func TestConcurrentReplacementsAreSerialized(t *testing.T) {
	f := newInstallFixture(t)
	download := f.ops.download
	var active atomic.Int32
	f.ops.download = func(ctx context.Context, tag, asset, dir string) error {
		if active.Add(1) != 1 {
			t.Error("overlapping replacements")
		}
		defer active.Add(-1)
		time.Sleep(time.Millisecond)
		return download(ctx, tag, asset, dir)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.checker.installAndRestart(f.ops); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.execCalls != 4 {
		t.Errorf("completed fixture replacements=%d, want 4", f.execCalls)
	}
	assertFixtureFile(t, f.current, "new fixture", 0o755)
}
