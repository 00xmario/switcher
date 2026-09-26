package codexcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestTrackedCodexRestoreKeepsOtherEditsAndPriorSelection(t *testing.T) {
	original := "model_provider = \"openai\" # keep this\r\nmodel = \"gpt-5\"\r\n\r\n[projects.\"/tmp\"]\r\ntrust_level = \"trusted\"\r\n"
	path := writeFile(t, original)
	if err := InstallAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 9123); got.Condition != "ready" || got.Ownership != "tracked" || got.RestoreAction != "restore" {
		t.Fatalf("tracked install: %+v", got)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(read(t, path), `trust_level = "trusted"`, `trust_level = "very-trusted"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(original, `trust_level = "trusted"`, `trust_level = "very-trusted"`, 1)
	if got := read(t, path); got != want {
		t.Fatalf("restore changed unrelated config:\nwant: %q\ngot: %q", want, got)
	}
	if got := read(t, path+".switcher-backup"); got != original {
		t.Fatal("first backup was replaced")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), journalName)); !os.IsNotExist(err) {
		t.Fatal("journal remains after restore")
	}
}

func TestPortUpdateKeepsOwnedLineEndingAfterUnrelatedCRLFEdit(t *testing.T) {
	path := writeFile(t, "model_provider = \"openai\"\nmodel = \"gpt-5\"\n")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# user note\r\n"+read(t, path)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 9123); got.Condition != "ready" || got.Ownership != "tracked" {
		t.Fatalf("CRLF edit broke ownership: %+v", got)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "# user note\r\nmodel_provider = \"openai\"\nmodel = \"gpt-5\"\n" {
		t.Fatalf("restore changed unrelated CRLF comment: %q", got)
	}
}

func TestReselectDoesNotReplaceRecordedPriorWithSwitcher(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	modified := strings.Replace(read(t, path), providerLine, providerLine+" # user note", 1)
	if err := os.WriteFile(path, []byte(modified), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 8787); got.InstallAction == "reselect" || got.RestoreAction == "restore" {
		t.Fatalf("altered owned selection offered destructive action: %+v", got)
	}
	if err := ReselectAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("reselect accepted altered owned line: %v", err)
	}
	if got := read(t, path); got != modified {
		t.Fatal("reselect changed user comment")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(modified, providerLine+" # user note", providerLine, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != `model_provider = "openai"`+"\n" {
		t.Fatalf("original provider lost: %q", got)
	}
}

func TestRestoreInsertedSelectionAfterPrependedComment(t *testing.T) {
	path := writeFile(t, "model = \"gpt-5\"\n")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# user's new note\n"+read(t, path)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "# user's new note\nmodel = \"gpt-5\"\n" {
		t.Fatalf("prepended comment was not preserved: %q", got)
	}
}

func TestTrackedCodexRestoreCreatedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.toml")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Switcher-only generated config should be removed")
	}
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(read(t, path), blockHeader,
		"model = \"gpt-5\"\n\n"+blockHeader, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "model = \"gpt-5\"\n\n" {
		t.Fatalf("user's later setting disappeared: %q", got)
	}
}

func TestTrackedCodexSelectionChangeAndExplicitReselect(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(read(t, path), providerLine, `model_provider = "other"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, port := range []int{8787, 9123} {
		if err := InstallAt(path, port); !errors.Is(err, ErrSelectionChanged) {
			t.Fatalf("port %d silently reselected Switcher: %v", port, err)
		}
		if read(t, path) != changed {
			t.Fatal("failed install changed user's provider")
		}
	}
	if got := Check(path, 8787); got.InstallAction != "reselect" || got.RestoreAction != "restore" {
		t.Fatalf("status after user selection: %+v", got)
	}
	if err := ReselectAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 9123).Condition; got != "ready" {
		t.Fatalf("explicit reselect status: %s", got)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != `model_provider = "other"`+"\n" {
		t.Fatalf("reselected provider was not restored: %q", got)
	}
}

func TestTrackedCodexUninstallPreservesUserSelection(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(read(t, path), providerLine, `model_provider = "other"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != `model_provider = "other"`+"\n" {
		t.Fatalf("uninstall overwrote chosen provider: %q", got)
	}
}

func TestLegacyCodexRequiresExplicitRemoval(t *testing.T) {
	path := writeFile(t, providerLine+"\n\n"+block)
	if got := Check(path, 8787); got.Ownership != "legacy_candidate" || got.RestoreAction != "remove_legacy" {
		t.Fatalf("legacy status: %+v", got)
	}
	if err := Uninstall(path); !errors.Is(err, ErrLegacyOwnershipUnknown) {
		t.Fatalf("untracked block silently removed: %v", err)
	}
	if err := UninstallLegacy(path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, path), "switcher") {
		t.Fatal("explicit legacy removal left Switcher fields")
	}
	conflictPath := writeFile(t, providerLine+"\n\n"+strings.Replace(block, `name = "Switcher"`, `name = "Private"`, 1))
	if err := UninstallLegacy(conflictPath); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("user-owned block removed: %v", err)
	}
}

func TestLegacyCommentedSelectionDoesNotOfferRemoval(t *testing.T) {
	path := writeFile(t, providerLine+" # user's note\n\n"+block)
	if got := Check(path, 8787); got.RestoreAction == "remove_legacy" {
		t.Fatalf("legacy action offered for edited selection: %+v", got)
	}
	if err := UninstallLegacy(path); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("legacy removal accepted edited selection: %v", err)
	}
}

func TestLegacyRemovalAcceptsOldMixedLineEndings(t *testing.T) {
	// The v0.5.0 line editor replaced the top-level line with LF and
	// appended an LF block without rewriting unrelated CRLF settings.
	path := writeFile(t, providerLine+"\nmodel = \"gpt-5\"\r\n\n"+block)
	if got := Check(path, 8787); got.Ownership != "legacy_candidate" || got.RestoreAction != "remove_legacy" {
		t.Fatalf("old mixed-EOL Switcher entry was not recognized: %+v", got)
	}
	if err := UninstallLegacy(path); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); strings.Contains(got, "switcher") || !strings.Contains(got, "model = \"gpt-5\"\r\n") {
		t.Fatalf("legacy removal changed unrelated CRLF setting: %q", got)
	}
}

func TestCodexHomeOutsideDefaultRootsIsSupportedWhenOwned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "config.toml")
	t.Setenv("HOME", filepath.Join(t.TempDir(), "different-home"))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "different-temp"))
	if err := InstallAt(path, 8787); err != nil {
		t.Fatalf("absolute user-owned CODEX_HOME refused: %v", err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
}

func TestCodexJournalRecoversInterruptedOperations(t *testing.T) {
	for _, tc := range []struct{ stage, operation string }{
		{"journal_prepared", "install"}, {"config_replaced", "install"},
		{"journal_prepared", "update"}, {"config_replaced", "update"},
		{"journal_prepared", "restore"}, {"config_replaced", "restore"},
	} {
		t.Run(tc.operation+"/"+tc.stage, func(t *testing.T) {
			path := writeFile(t, `model_provider = "openai"`+"\n")
			if tc.operation != "install" {
				if err := InstallAt(path, 8787); err != nil {
					t.Fatal(err)
				}
			}
			codexFault = func(stage string) error {
				if stage == tc.stage {
					return errors.New("simulated process exit")
				}
				return nil
			}
			var err error
			switch tc.operation {
			case "install":
				err = InstallAt(path, 8787)
			case "update":
				err = InstallAt(path, 9123)
			case "restore":
				err = Uninstall(path)
			}
			codexFault = func(string) error { return nil }
			if err == nil || Check(path, 8787).Condition != "pending" {
				t.Fatalf("interruption not observable: %v, %+v", err, Check(path, 8787))
			}
			switch tc.operation {
			case "install":
				err = InstallAt(path, 8787)
			case "update":
				err = InstallAt(path, 9123)
			case "restore":
				err = Uninstall(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.operation == "restore" {
				if got := read(t, path); got != `model_provider = "openai"`+"\n" {
					t.Fatalf("restore recovery: %q", got)
				}
			} else if got := Check(path, map[bool]int{true: 9123, false: 8787}[tc.operation == "update"]); got.Condition != "ready" || got.Ownership != "tracked" {
				t.Fatalf("install recovery: %+v", got)
			}
		})
	}
}

func TestCodexConfigRejectsUnsafeSymlinksAndMetadata(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	if err := os.WriteFile(target, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := InstallAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("symlink target modified: %v", err)
	}
	if got := read(t, target); got != "model = \"gpt-5\"\n" {
		t.Fatal("symlink target changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, journalName), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 8787).Condition; got != "conflict" {
		t.Fatalf("corrupt journal status: %s", got)
	}
	if err := InstallAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("corrupt journal ignored: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, journalName)); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(dir, "real")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := InstallAt(filepath.Join(dir, "alias", "config.toml"), 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("parent directory symlink was followed: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ".switcher.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".switcher.lock")); err != nil {
		t.Fatal(err)
	}
	if err := InstallAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("symlinked lock accepted: %v", err)
	}
}

func TestCodexCheckDoesNotCreateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if got := Check(filepath.Join(dir, "config.toml"), 8787); got.Condition != "missing" {
		t.Fatalf("missing config: %+v", got)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("read-only Check created a directory or lock")
	}
}

func TestCodexDetectsExternalEditBeforeRename(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	want := `model_provider = "openai"` + "\nmodel = \"gpt-5\"\n"
	codexFault = func(stage string) error {
		if stage == "before_config_replace" {
			return os.WriteFile(path, []byte(want), 0o600)
		}
		return nil
	}
	defer func() { codexFault = func(string) error { return nil } }()
	if err := InstallAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("concurrent edit was overwritten: %v", err)
	}
	if got := read(t, path); got != want {
		t.Fatalf("external edit was overwritten: %q", got)
	}
	if got := Check(path, 8787).Condition; got != "pending" {
		t.Fatalf("interrupted transaction was hidden: %s", got)
	}
}

func TestCodexDetectsEditAtFinalRename(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	want := `model_provider = "openai"` + "\nmodel = \"gpt-5\"\n"
	codexFault = func(stage string) error {
		if stage == "before_rename" {
			return os.WriteFile(path, []byte(want), 0o600)
		}
		return nil
	}
	defer func() { codexFault = func(string) error { return nil } }()
	if err := InstallAt(path, 8787); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("edit immediately before rename was overwritten: %v", err)
	}
	if got := read(t, path); got != want {
		t.Fatalf("concurrent edit lost: %q", got)
	}
}

func TestCodexDetectsEditBeforeRemovingGeneratedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	var changed string
	codexFault = func(stage string) error {
		if stage == "before_unlink" {
			changed = "# new note\n" + read(t, path)
			return os.WriteFile(path, []byte(changed), 0o600)
		}
		return nil
	}
	defer func() { codexFault = func(string) error { return nil } }()
	if err := Uninstall(path); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("modified file was removed: %v", err)
	}
	if got := read(t, path); got != changed {
		t.Fatal("concurrent change was lost")
	}
}

func TestCodexTrackedFilesArePrivateAndLockSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "config.toml")
	var wg sync.WaitGroup
	for _, port := range []int{8787, 9123} {
		wg.Add(1)
		go func(port int) { defer wg.Done(); _ = InstallAt(path, port) }(port)
	}
	wg.Wait()
	for _, name := range []string{"config.toml", "config.toml.switcher-backup", journalName, ".switcher.lock"} {
		info, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if name == "config.toml.switcher-backup" && os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s is not user-only: %v", name, err)
		}
	}
	if got := Check(path, 8787); got.Condition != "ready" && Check(path, 9123).Condition != "ready" {
		t.Fatalf("concurrent installs left inconsistent state: %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), journalName))
	if err != nil {
		t.Fatal(err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil || j.Phase != "active" {
		t.Fatalf("journal after concurrent writers: %v, %+v", err, j)
	}
}
