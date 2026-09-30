package claudesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree builds root/<account>/<workspace>/ and returns the workspace path.
func tree(t *testing.T, root, account, workspace string) string {
	t.Helper()
	dir := filepath.Join(root, account, workspace)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// pointer writes one session pointer and fixes its modification time so the
// copy can be checked for timestamp preservation.
func pointer(t *testing.T, dir, name, body string, mod time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}

// stubOptions wires two accounts' indexes under root and a backup folder,
// with inert desktop lifecycle hooks.
func stubOptions(t *testing.T) (Options, string, string) {
	t.Helper()
	root := t.TempDir()
	backup := t.TempDir()
	return Options{
		Root:      root,
		BackupDir: backup,
		Wait:      func(time.Duration) {},
		Quit:      func() error { return nil },
		Reopen:    func() error { return nil },
	}, root, backup
}

func TestSyncCopiesMissingPointersBothWays(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)

	pointer(t, a, "local_shared.json", `{"sessionId":"local_shared","cliSessionId":"shared"}`, past)
	pointer(t, a, "local_only-a.json", `{"sessionId":"local_only-a"}`, past)
	pointer(t, b, "local_shared.json", `{"sessionId":"local_shared","cliSessionId":"shared"}`, past)
	pointer(t, b, "local_only-b.json", `{"sessionId":"local_only-b"}`, past)

	result, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 2 {
		t.Fatalf("added %d pointers, want 2", result.Added)
	}
	for _, dir := range []string{a, b} {
		got := strings.Join(names(t, dir), ",")
		for _, want := range []string{"local_shared.json", "local_only-a.json", "local_only-b.json"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s is missing %s, have %s", dir, want, got)
			}
		}
	}
	byID := map[string]AccountResult{}
	for _, account := range result.Accounts {
		byID[account.ID] = account
	}
	if byID["acct-a"].Added != 1 || byID["acct-b"].Added != 1 {
		t.Fatalf("per-account added = %d and %d, want 1 and 1", byID["acct-a"].Added, byID["acct-b"].Added)
	}
	if byID["acct-a"].Total != 3 || byID["acct-b"].Total != 3 {
		t.Fatalf("per-account totals = %d and %d, want 3 and 3", byID["acct-a"].Total, byID["acct-b"].Total)
	}
}

func TestSyncNeverOverwritesAPointerThatExistsOnBothSides(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	past := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	// The same pointer name with different local content: whichever side is
	// older, a sync must not replace either.
	pointer(t, a, "local_same.json", `{"sessionId":"local_same","side":"a"}`, past)
	pointer(t, b, "local_same.json", `{"sessionId":"local_same","side":"b"}`, time.Now())

	result, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 0 {
		t.Fatalf("added %d pointers, want 0", result.Added)
	}
	if got := read(t, filepath.Join(a, "local_same.json")); !strings.Contains(got, `"side":"a"`) {
		t.Fatalf("account a pointer was overwritten: %s", got)
	}
	if got := read(t, filepath.Join(b, "local_same.json")); !strings.Contains(got, `"side":"b"`) {
		t.Fatalf("account b pointer was overwritten: %s", got)
	}
}

func TestSyncDeletesNothing(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")

	pointer(t, a, "local_keep-a.json", `{}`, time.Now())
	pointer(t, b, "local_keep-b.json", `{}`, time.Now())
	// Claude Desktop's own bookkeeping, which a sync must leave alone.
	for _, extra := range []string{
		"deleted_07b5e0bb-c4f8-4a9f-967d-49f896b46d04",
		"archived-sessions.idx",
	} {
		if err := os.WriteFile(filepath.Join(a, extra), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(a, "backlog"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(t, a), ",")
	for _, want := range []string{"local_keep-a.json", "local_keep-b.json", "deleted_07b5e0bb-c4f8-4a9f-967d-49f896b46d04", "archived-sessions.idx", "backlog"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sync removed %s from %s", want, got)
		}
	}
}

func TestSyncBacksUpBeforeWritingAnything(t *testing.T) {
	opts, root, backup := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")

	pointer(t, a, "local_only-a.json", `{"from":"a"}`, time.Now())
	pointer(t, b, "local_only-b.json", `{"from":"b"}`, time.Now())

	result, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Backup == "" {
		t.Fatal("sync reported no backup directory")
	}
	if !strings.HasPrefix(result.Backup, backup) {
		t.Fatalf("backup %q is outside the backup dir %q", result.Backup, backup)
	}
	// The backup is taken before the merge, so each side holds only what it
	// had, and both sides are represented.
	for account, want := range map[string][]string{
		"acct-a": {"local_only-a.json"},
		"acct-b": {"local_only-b.json"},
	} {
		dir := filepath.Join(result.Backup, account)
		got := strings.Join(names(t, dir), ",")
		for _, name := range want {
			if !strings.Contains(got, name) {
				t.Fatalf("backup %s is missing %s, have %s", dir, name, got)
			}
		}
		if strings.Contains(got, "local_only-a.json") && strings.Contains(got, "local_only-b.json") {
			t.Fatalf("backup %s looks post-merge: %s", dir, got)
		}
	}
}

func TestSyncWorksAcrossThreeOrMoreAccounts(t *testing.T) {
	opts, root, _ := stubOptions(t)
	dirs := map[string]string{
		"acct-a": tree(t, root, "acct-a", "ws"),
		"acct-b": tree(t, root, "acct-b", "ws"),
		"acct-c": tree(t, root, "acct-c", "ws"),
	}
	pointer(t, dirs["acct-a"], "local_1.json", `{}`, time.Now())
	pointer(t, dirs["acct-b"], "local_2.json", `{}`, time.Now())
	pointer(t, dirs["acct-c"], "local_3.json", `{}`, time.Now())

	result, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.Added != 6 {
		t.Fatalf("added %d pointers across three accounts, want 6", result.Added)
	}
	for id, dir := range dirs {
		if got := len(names(t, dir)); got != 3 {
			t.Fatalf("%s has %d pointers, want 3", id, got)
		}
	}
	if len(result.Accounts) != 3 {
		t.Fatalf("reported %d accounts, want 3", len(result.Accounts))
	}
}

func TestSyncNeedsAtLeastTwoIndexes(t *testing.T) {
	opts, root, _ := stubOptions(t)
	single := tree(t, root, "acct-a", "ws")
	pointer(t, single, "local_1.json", `{}`, time.Now())

	if _, err := Sync(context.Background(), opts); !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("sync with one index: %v, want ErrNoAccounts", err)
	}

	// A workspace with no pointers is not an index either.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	tree(t, root, "acct-a", "empty")
	if _, err := Sync(context.Background(), opts); !errors.Is(err, ErrNoAccounts) {
		t.Fatalf("sync with no pointers: %v, want ErrNoAccounts", err)
	}
}

func TestSyncPreservesPointerModeAndTimestamps(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	mod := time.Now().Add(-3 * time.Hour).Truncate(time.Second)

	src := filepath.Join(a, "local_1.json")
	pointer(t, a, "local_1.json", `{"keep":"mtime"}`, mod)
	pointer(t, b, "local_other.json", `{}`, mod)
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(b, "local_1.json")
	info, err := os.Stat(copied)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copied pointer mode %v, want 0600", info.Mode().Perm())
	}
	if !info.ModTime().Equal(mod) {
		t.Fatalf("copied pointer mtime %v, want %v", info.ModTime(), mod)
	}
	if read(t, copied) != `{"keep":"mtime"}` {
		t.Fatalf("copied pointer content changed: %s", read(t, copied))
	}
}

func TestSyncQuitsThenReopensClaudeDesktop(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	pointer(t, a, "local_1.json", `{}`, time.Now())
	pointer(t, b, "local_other.json", `{}`, time.Now())

	var events []string
	opts.Quit = func() error {
		events = append(events, "quit")
		return nil
	}
	opts.Reopen = func() error {
		events = append(events, "reopen")
		return nil
	}
	opts.Wait = func(time.Duration) { events = append(events, "wait") }

	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "quit,wait,reopen" {
		t.Fatalf("desktop lifecycle order %v, want quit then wait then reopen", events)
	}
	// Refusing to close must abort before writes rather than race Desktop.
	opts.Quit = func() error { return errors.New("Claude is busy") }
	opts.Reopen = func() error { return nil }
	if _, err := Sync(context.Background(), opts); err == nil {
		t.Fatal("a failed quit must abort the sync")
	}
}

func TestSyncWritesOnlyInsideItsDirectories(t *testing.T) {
	opts, root, backup := stubOptions(t)
	transcripts := t.TempDir()
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	pointer(t, a, "local_1.json", `{}`, time.Now())
	pointer(t, b, "local_2.json", `{}`, time.Now())

	if _, err := Sync(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// The transcripts directory must stay untouched: it holds the actual
	// conversations, not the Desktop discovery index.
	if got := names(t, transcripts); len(got) != 0 {
		t.Fatalf("sync wrote into the transcripts dir: %v", got)
	}
	for _, dir := range []string{root, backup} {
		if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.HasPrefix(info.Name(), ".switcher-sync-") {
				t.Fatalf("sync left a temporary file behind: %s", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefaultBackupDirStaysOutOfTranscripts(t *testing.T) {
	dir, err := DefaultBackupDir()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dir, filepath.Join(".claude", "projects")) {
		t.Fatalf("backup dir %q points into the transcripts directory", dir)
	}
	if !strings.Contains(dir, "desktop-session-sync-backups") {
		t.Fatalf("backup dir %q is not the claude-sync backup location", dir)
	}
	root, err := SessionIndexRoot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(root, filepath.Join("Claude", "claude-code-sessions")) {
		t.Fatalf("session index root %q is wrong", root)
	}
}

func TestCopyPointerRefusesToReplaceAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "local_src.json")
	dst := filepath.Join(dir, "local_dst.json")
	pointer(t, dir, "local_src.json", `{"from":"src"}`, time.Now())
	pointer(t, dir, "local_dst.json", `{"from":"dst"}`, time.Now())

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	copied, err := copyPointer(root, filepath.Base(src), root, filepath.Base(dst))
	if err != nil {
		t.Fatal(err)
	}
	if copied {
		t.Fatal("copyPointer reported a copy over an existing file")
	}
	if got := read(t, dst); got != `{"from":"dst"}` {
		t.Fatalf("destination was modified: %s", got)
	}
	if got := names(t, dir); len(got) != 2 {
		t.Fatalf("copyPointer left extra files behind: %v", got)
	}
}

func TestDescribeSummarisesPerAccount(t *testing.T) {
	result := Result{
		Added: 3,
		Accounts: []AccountResult{
			{Label: "a@example.com", Added: 3},
			{Label: "b@example.com", Added: 0},
		},
	}
	got := Describe(result)
	for _, want := range []string{"3 new sessions added", "a@example.com: 3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Describe = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "b@example.com") {
		t.Fatalf("Describe mentioned an account that gained nothing: %q", got)
	}
	if Describe(Result{}) != "No new Claude Code sessions to share" {
		t.Fatalf("Describe with nothing added = %q", Describe(Result{}))
	}
}

func TestLabelsFallBackToAShortID(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "11111111-1111-4111-8111-111111111111", "ws")
	b := tree(t, root, "22222222-2222-4222-8222-222222222222", "ws")
	pointer(t, a, "local_1.json", `{}`, time.Now())
	pointer(t, b, "local_2.json", `{}`, time.Now())
	opts.Labels = map[string]string{"11111111-1111-4111-8111-111111111111": "account-a@example.test"}

	result, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = b
	byID := map[string]AccountResult{}
	for _, account := range result.Accounts {
		byID[account.ID] = account
	}
	if byID["11111111-1111-4111-8111-111111111111"].Label != "account-a@example.test" {
		t.Fatalf("mapped label = %q", byID["11111111-1111-4111-8111-111111111111"].Label)
	}
	if got := byID["22222222-2222-4222-8222-222222222222"].Label; got != "22222222" {
		t.Fatalf("fallback label = %q, want the short id", got)
	}
}

func TestSyncHonoursCancellation(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "acct-a", "ws-a")
	b := tree(t, root, "acct-b", "ws-b")
	pointer(t, a, "local_1.json", `{}`, time.Now())
	pointer(t, b, "local_other.json", `{}`, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Sync(ctx, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sync: %v, want context.Canceled", err)
	}
	if got := names(t, b); len(got) != 1 {
		t.Fatalf("a cancelled sync still wrote files: %v", got)
	}
}

func TestEmptyIndexReceivesSessionsAndBackupsAreUnique(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "account-a", "workspace")
	b := tree(t, root, "account-b", "workspace")
	pointer(t, a, "local_a.json", `{"cliSessionId":"shared-transcript"}`, time.Now())
	opts.Now = func() time.Time { return time.Unix(12345, 0) }
	first, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Added != 1 || !strings.Contains(read(t, filepath.Join(b, "local_a.json")), "shared-transcript") {
		t.Fatal("empty index was not populated")
	}
	second, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Backup == first.Backup || second.Added != 0 {
		t.Fatal("backup collision or non-idempotent sync")
	}
}

func TestFailureReopensDesktopAndReportsBackup(t *testing.T) {
	opts, root, backup := stubOptions(t)
	a := tree(t, root, "a", "workspace")
	tree(t, root, "b", "workspace")
	pointer(t, a, "local_a.json", `{}`, time.Now())
	opts.BackupDir = filepath.Join(backup, "file")
	if err := os.WriteFile(opts.BackupDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened := false
	opts.Reopen = func() error { reopened = true; return nil }
	if _, err := Sync(context.Background(), opts); err == nil {
		t.Fatal("backup failure hidden")
	}
	if reopened {
		t.Fatal("preflight failure must not restart Claude")
	}
	if len(names(t, a)) != 1 {
		t.Fatal("backup failure modified sessions")
	}
}

func TestSymlinkPointerCannotReadTranscript(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "a", "workspace")
	tree(t, root, "b", "workspace")
	transcripts := t.TempDir()
	path := filepath.Join(transcripts, "real-transcript.json")
	if err := os.WriteFile(path, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(a, "local_link.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(context.Background(), opts); err == nil {
		t.Fatal("symlink pointer accepted")
	}
	if read(t, path) != "untouched" {
		t.Fatal("transcript modified")
	}
}

func TestQuitFlushAddsNamespaceBeforeSnapshot(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "a", "workspace")
	tree(t, root, "b", "workspace")
	pointer(t, a, "local_a.json", `{}`, time.Now())
	opts.Quit = func() error {
		c := tree(t, root, "c", "workspace")
		pointer(t, c, "local_c.json", `{}`, time.Now())
		return nil
	}
	r, err := Sync(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Accounts) != 3 || r.Added != 4 {
		t.Fatalf("flushed namespace omitted: %+v", r)
	}
}

func TestNamespaceSwapDuringQuitCannotTouchTranscripts(t *testing.T) {
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "a", "workspace")
	b := tree(t, root, "b", "workspace")
	pointer(t, a, "local_a.json", `{}`, time.Now())
	transcripts := t.TempDir()
	pointer(t, transcripts, "local_private.json", `{"transcript":"untouched"}`, time.Now())
	reopened := false
	opts.Quit = func() error {
		if err := os.Remove(b); err != nil {
			return err
		}
		return os.Symlink(transcripts, b)
	}
	opts.Reopen = func() error { reopened = true; return nil }
	if _, err := Sync(context.Background(), opts); err == nil {
		t.Fatal("redirected workspace was accepted")
	}
	if !reopened {
		t.Fatal("failed sync left Desktop closed")
	}
	if len(names(t, transcripts)) != 1 || read(t, filepath.Join(transcripts, "local_private.json")) != `{"transcript":"untouched"}` {
		t.Fatal("transcripts changed")
	}
}

func TestPartialQuitErrorStillReopens(t *testing.T) {
	opts, root, _ := stubOptions(t)
	tree(t, root, "a", "workspace")
	tree(t, root, "b", "workspace")
	reopened := false
	opts.Quit = func() error { return context.Canceled }
	opts.Reopen = func() error { reopened = true; return nil }
	if _, err := Sync(context.Background(), opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("quit error: %v", err)
	}
	if !reopened {
		t.Fatal("cancelled shutdown did not reopen")
	}
}

func TestPartialCopyFailureKeepsCountsAndBackup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission test needs an unprivileged user")
	}
	opts, root, _ := stubOptions(t)
	a := tree(t, root, "a", "workspace")
	b := tree(t, root, "b", "workspace")
	pointer(t, a, "local_a.json", `{}`, time.Now())
	pointer(t, b, "local_b.json", `{}`, time.Now())
	if err := os.Chmod(b, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(b, 0700)
	r, err := Sync(context.Background(), opts)
	if err == nil {
		t.Fatal("expected destination permission failure")
	}
	if r.Backup == "" || r.Added != 1 {
		t.Fatalf("partial result lost: %+v, %v", r, err)
	}
	if len(names(t, filepath.Join(r.Backup, "b"))) != 1 {
		t.Fatal("pre-sync backup missing")
	}
}
