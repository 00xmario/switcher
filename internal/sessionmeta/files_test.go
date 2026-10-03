package sessionmeta_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"switcher/internal/sessionmeta"
)

func TestLookupSkipsUnsafeMetadataAndRoots(t *testing.T) {
	for _, kind := range []string{"root symlink", "ancestor symlink", "account symlink", "workspace symlink", "file symlink", "hardlink", "writable file", "writable root", "writable workspace", "FIFO"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			desktop := filepath.Join(root, "desktop")
			path := saved(t, desktop, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Unsafe title"}`)
			link := func(old, new string) {
				t.Helper()
				if err := os.Rename(old, new); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(new, old); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "root symlink":
				link(desktop, desktop+"-real")
			case "ancestor symlink":
				alias := filepath.Join(root, "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				desktop = filepath.Join(alias, "desktop")
			case "account symlink":
				link(filepath.Join(desktop, "account"), filepath.Join(root, "outside-account"))
			case "workspace symlink":
				link(filepath.Join(desktop, "account/workspace"), filepath.Join(root, "outside-workspace"))
			case "file symlink":
				link(path, filepath.Join(root, "outside.json"))
			case "hardlink":
				if err := os.Link(path, filepath.Join(root, "outside.json")); err != nil {
					t.Fatal(err)
				}
			case "writable file":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "writable root", "writable workspace":
				dir := desktop
				if kind == "writable workspace" {
					dir = filepath.Dir(path)
				}
				if err := os.Chmod(dir, 0777); err != nil {
					t.Fatal(err)
				}
			case "FIFO":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
				t.Fatalf("unsafe metadata selected: %#v", got)
			}
			if time.Since(start) > time.Second {
				t.Fatal("unsafe metadata blocked lookup")
			}
		})
	}
	if os.Getuid() == 0 {
		t.Run("foreign owned file", func(t *testing.T) {
			root := fixtureRoot(t)
			path := saved(t, root, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Foreign title"}`)
			if err := os.Chown(path, 65534, -1); err != nil {
				t.Fatal(err)
			}
			if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
				t.Fatal("foreign metadata selected")
			}
		})
	}
}

func TestLookupIgnoresTranscriptsAndMalformedMetadata(t *testing.T) {
	root := fixtureRoot(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	saved(t, desktop, "account/workspace/local_partial.json", `{"cliSessionId":"`+firstID+`","title":"Partial`)
	saved(t, desktop, "account/workspace/local_wrong-type.json", `{"cliSessionId":"`+firstID+`","title":123}`)
	saved(t, desktop, "account/workspace/local_array.json", `[{"cliSessionId":"`+firstID+`","title":"Wrong schema"}]`)
	saved(t, desktop, "account/workspace/.local_pending.json", `{"cliSessionId":"`+firstID+`","title":"Pending title"}`)
	saved(t, desktop, "account/workspace/nested/local_hidden.json", `{"cliSessionId":"`+firstID+`","title":"Wrong depth"}`)
	saved(t, projects, "a/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","summary":"CLI fallback","projectPath":"/work/fallback"}]}`)
	saved(t, projects, "b/sessions-index.json", `{"entries":[`)
	for _, dir := range []string{filepath.Join(desktop, "account/workspace"), filepath.Join(projects, "a")} {
		for _, name := range []string{firstID + ".jsonl", "local_transcript.jsonl", "history.jsonl", "settings.json", "auth.json", "profiles.json", "credentials.json", "sessions-index.json.tmp"} {
			if err := unix.Mkfifo(filepath.Join(dir, name), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}).Lookup(context.Background(), []string{firstID})
	if len(got) != 1 || got[firstID] != (sessionmeta.Info{Title: "CLI fallback", Project: "fallback", TitleSource: "cli", ClientKind: "cli"}) {
		t.Fatalf("malformed fallback = %#v", got)
	}
}

func TestLookupMissingRootsAndUnsetConfigNeverUseHOME(t *testing.T) {
	root := fixtureRoot(t)
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	saved(t, root, "Library/Application Support/Claude/claude-code-sessions/account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Implicit HOME title"}`)
	saved(t, root, ".claude/projects/a/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","summary":"Implicit CLI title"}]}`)
	missing := filepath.Join(root, "missing")
	for _, cfg := range []sessionmeta.Config{{}, {DesktopRoot: missing, ProjectsRoot: missing + "-projects"}, {DesktopRoot: "relative", ProjectsRoot: "../projects"}} {
		index := sessionmeta.New(cfg)
		if got := index.Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("implicit or missing root produced metadata: %#v", got)
		}
	}
	for _, path := range []string{missing, missing + "-projects"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("lookup created missing root: %v", err)
		}
	}
}

func TestLookupBudgetsRejectOversizedAndIncompleteScans(t *testing.T) {
	t.Run("per file falls through", func(t *testing.T) {
		root := fixtureRoot(t)
		desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
		saved(t, desktop, "account/workspace/local_large.json", `{"cliSessionId":"`+firstID+`","title":"Too large","padding":"`+strings.Repeat("x", 1<<20)+`"}`)
		saved(t, projects, "a/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","summary":"Bounded fallback"}]}`)
		got := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}).Lookup(context.Background(), []string{firstID})
		if got[firstID].Title != "Bounded fallback" {
			t.Fatalf("oversized file prevented fallback: %#v", got)
		}
	})
	t.Run("record budget", func(t *testing.T) {
		root := fixtureRoot(t)
		row := `{"sessionId":"` + firstID + `","summary":"Unproven title"}`
		saved(t, root, "a/sessions-index.json", `{"entries":[`+strings.Repeat(row+",", 4096)+row+`]}`)
		if got := sessionmeta.New(sessionmeta.Config{ProjectsRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("incomplete scan exposed an unproven match: %#v", got)
		}
	})
	t.Run("directory entry budget", func(t *testing.T) {
		root := fixtureRoot(t)
		saved(t, root, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Unproven title"}`)
		for j := 0; j < 4096; j++ {
			saved(t, root, fmt.Sprintf("account/workspace/ignored_%04d.tmp", j), `{}`)
		}
		if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("entry-limited scan exposed an unproven match: %#v", got)
		}
	})
	t.Run("total byte budget", func(t *testing.T) {
		root := fixtureRoot(t)
		prefix := `{"cliSessionId":"` + firstID + `","title":"Unproven title","padding":"`
		data := prefix + strings.Repeat("x", (1<<20)-len(prefix)-2) + `"}`
		for j := 0; j < 33; j++ {
			saved(t, root, fmt.Sprintf("account/workspace/local_%02d.json", j), data)
		}
		if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("incomplete byte-limited scan exposed a match: %#v", got)
		}
	})
}

func TestLookupCacheRefreshCancellationAndConcurrentPolling(t *testing.T) {
	root := fixtureRoot(t)
	path := saved(t, root, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Old title"}`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := index.Lookup(ctx, []string{firstID}); len(got) != 0 {
		t.Fatal("canceled lookup returned metadata")
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if got := index.Lookup(deadline, []string{firstID}); len(got) != 0 {
		t.Fatal("expired lookup returned metadata")
	}
	newPath := saved(t, root, "account/workspace/.rename.tmp", `{"cliSessionId":"`+firstID+`","title":"New title"}`)
	if err := os.Rename(newPath, path); err != nil {
		t.Fatal(err)
	}
	got := index.Lookup(context.Background(), []string{firstID})
	if got[firstID].Title != "New title" {
		t.Fatal("canceled lookup populated the cache")
	}
	got[firstID] = sessionmeta.Info{Title: "Caller mutation"}
	newPath = saved(t, root, "account/workspace/.rename.tmp", `{"cliSessionId":"`+firstID+`","title":"Renamed title"}`)
	if err := os.Rename(newPath, path); err != nil {
		t.Fatal(err)
	}
	if got := index.Lookup(context.Background(), []string{firstID}); got[firstID].Title != "New title" {
		t.Fatal("cache was mutated or refreshed before its TTL")
	}
	time.Sleep(3100 * time.Millisecond)
	var wg sync.WaitGroup
	for j := 0; j < 24; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := index.Lookup(context.Background(), []string{firstID}); got[firstID].Title != "Renamed title" {
				t.Errorf("rename not visible after TTL: %#v", got)
			}
		}()
	}
	wg.Wait()
	if raw, err := os.ReadFile(path); err != nil || string(raw) != `{"cliSessionId":"`+firstID+`","title":"Renamed title"}` {
		t.Fatalf("lookup modified fixture metadata: %v", err)
	}
}
