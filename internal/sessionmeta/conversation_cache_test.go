package sessionmeta_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"switcher/internal/sessionmeta"
)

// This is the relay's structural contract, without a dependency on its package.
var _ interface {
	Resolve(context.Context, []string) (map[string]string, error)
} = (*sessionmeta.Index)(nil)

func TestResolveUnknownAliasRefreshesSharedCacheAfterAtomicRename(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	if info := index.Lookup(context.Background(), []string{requestA}); info[requestA].ConversationID != conversationID {
		t.Fatal("Lookup did not construct shared membership")
	}
	path := conversationRecord(t, root, "b", conversationID, requestB, "")
	pending := filepath.Join(filepath.Dir(path), ".metadata-update.tmp")
	if err := os.Rename(path, pending); err != nil {
		t.Fatal(err)
	}
	if got, err := index.Resolve(context.Background(), []string{requestB}); err != nil || len(got) != 0 {
		t.Fatalf("temporary metadata file created membership: %#v, %v", got, err)
	}
	if err := os.Rename(pending, path); err != nil {
		t.Fatal(err)
	}
	// This UUID already had a negative refresh. Its new file becomes visible
	// at TTL expiry rather than causing every request to rescan the index.
	time.Sleep(3100 * time.Millisecond)
	var wg sync.WaitGroup
	for j := 0; j < 24; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := index.Resolve(context.Background(), []string{requestA, requestB})
			if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID}) {
				t.Errorf("new alias stayed stale or concurrent snapshot tore: %#v, %v", got, err)
			}
			info := index.Lookup(context.Background(), []string{requestB})
			if info[requestB].ConversationID != conversationID {
				t.Errorf("Lookup and Resolve used different snapshots: %#v", info)
			}
			info[requestB] = sessionmeta.Info{ConversationID: "caller-mutation"}
		}()
	}
	wg.Wait()
	if got, err := index.Resolve(context.Background(), []string{requestB}); err != nil || got[requestB] != conversationID {
		t.Fatalf("display map mutation changed membership: %#v, %v", got, err)
	}
}

func TestResolveTTLRefreshRemovesDeletedAndRelabeledAliases(t *testing.T) {
	root := fixtureRoot(t)
	a := conversationRecord(t, root, "a", conversationID, requestA, "")
	b := conversationRecord(t, root, "b", conversationID, requestB, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	if got, err := index.Resolve(context.Background(), []string{requestA, requestB}); err != nil || len(got) != 2 {
		t.Fatalf("initial metadata snapshot = %#v, %v", got, err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	if got, err := index.Resolve(context.Background(), []string{requestB}); err != nil || got[requestB] != conversationID {
		t.Fatalf("known membership was not cached within TTL: %#v, %v", got, err)
	}
	time.Sleep(3100 * time.Millisecond)
	got, err := index.Resolve(context.Background(), []string{requestA, requestB})
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID}) {
		t.Fatalf("deleted source remained authority after TTL: %#v, %v", got, err)
	}
	if info := index.Lookup(context.Background(), []string{requestB}); len(info) != 0 {
		t.Fatalf("deleted display metadata survived resolver refresh: %#v", info)
	}
	conversationRecord(t, root, "b", firstID, requestB, "")
	time.Sleep(3100 * time.Millisecond)
	got, err = index.Resolve(context.Background(), []string{requestB})
	if err != nil || got[requestB] != firstID {
		t.Fatalf("new native identity reused the old conversation: %#v, %v", got, err)
	}
	reviseConversation(t, a, map[string]any{"sessionId": "local_" + firstID})
	// An unknown ID forces a current scan for the whole source, exposing the
	// mismatched filename and invalidating both claimed native identities.
	got, err = index.Resolve(context.Background(), []string{requestA, requestB, requestC})
	if err != nil || len(got) != 0 {
		t.Fatalf("relabel conflict retained cached authority: %#v, %v", got, err)
	}
	if err := os.Rename(a, filepath.Join(filepath.Dir(a), "local_"+firstID+".json")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3100 * time.Millisecond)
	got, err = index.Resolve(context.Background(), []string{requestA, requestB})
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: firstID, requestB: firstID}) {
		t.Fatalf("correct native rename did not refresh missing memberships: %#v, %v", got, err)
	}
}

func TestResolveCancellationReturnsNoAuthorityAndCannotPoisonCache(t *testing.T) {
	root := fixtureRoot(t)
	path := conversationRecord(t, root, "a", conversationID, requestA, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := index.Resolve(ctx, []string{requestA}); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("canceled membership lookup = %#v, %v", got, err)
	}
	reviseConversation(t, path, map[string]any{"createdAt": nil})
	if got, err := index.Resolve(context.Background(), []string{requestA}); err != nil || len(got) != 0 {
		t.Fatalf("canceled scan cached the previous record: %#v, %v", got, err)
	}
	reviseConversation(t, path, map[string]any{"createdAt": int64(1788432998079)})
	time.Sleep(3100 * time.Millisecond)
	if got, err := index.Resolve(context.Background(), []string{requestA}); err != nil || got[requestA] != conversationID {
		t.Fatalf("recovery after cancellation = %#v, %v", got, err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if got, err := index.Resolve(deadline, []string{requestA}); !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Fatalf("expired context returned cached authority: %#v, %v", got, err)
	}
}

func TestResolveMissingRootsNeverConstructHOMEFallback(t *testing.T) {
	root := fixtureRoot(t)
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	conversationRecord(t, filepath.Join(root, "Library/Application Support/Claude/claude-code-sessions"), "a", conversationID, requestA, "")
	missing := filepath.Join(root, "missing")
	for _, index := range []*sessionmeta.Index{nil, sessionmeta.New(sessionmeta.Config{}), sessionmeta.New(sessionmeta.Config{DesktopRoot: missing})} {
		if got, err := index.Resolve(context.Background(), []string{requestA}); err != nil || len(got) != 0 {
			t.Fatalf("unset or missing root resolved implicit metadata: %#v, %v", got, err)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing metadata root was created: %v", err)
	}
}

func TestResolveUnsafeNativeFilesAndRootsNeverCreateAuthority(t *testing.T) {
	for _, kind := range []string{"file symlink", "root symlink", "hardlink", "writable file"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			desktop := filepath.Join(root, "desktop")
			path := conversationRecord(t, desktop, "a", conversationID, requestA, "")
			switch kind {
			case "file symlink":
				outside := filepath.Join(root, "outside.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "root symlink":
				outside := desktop + "-real"
				if err := os.Rename(desktop, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, desktop); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(root, "outside.json")); err != nil {
					t.Fatal(err)
				}
			case "writable file":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop}).Resolve(context.Background(), []string{requestA}); err != nil || len(got) != 0 {
				t.Fatalf("unsafe source created conversation authority: %#v, %v", got, err)
			}
		})
	}
}

func TestResolveTruncatedScanRevokesCachedAuthority(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	if got, err := index.Resolve(context.Background(), []string{requestA}); err != nil || got[requestA] != conversationID {
		t.Fatalf("initial complete source = %#v, %v", got, err)
	}
	for j := 0; j < 4096; j++ {
		saved(t, root, fmt.Sprintf("a/workspace/ignored_%04d.tmp", j), `{}`)
	}
	got, err := index.Resolve(context.Background(), []string{requestA, requestB})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Fatalf("incomplete source exposed previous or partial authority: %#v, %v", got, err)
	}
	if info := index.Lookup(context.Background(), []string{requestA}); info[requestA].ConversationID != "" {
		t.Fatalf("truncated source retained display membership: %#v", info)
	}
}
