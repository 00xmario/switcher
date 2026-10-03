package sessionmeta_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"switcher/internal/sessionmeta"
)

func TestResolveRejectedNativeFileVetoesOtherCopiesOfKnownFilename(t *testing.T) {
	for _, kind := range []string{"oversize", "hardlink", "symlink", "broken symlink", "writable file", "unreadable file", "directory", "invalid header"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "unreadable file" && os.Getuid() == 0 {
				t.Skip("root can read mode-000 fixture files")
			}
			root := fixtureRoot(t)
			conversationRecord(t, root, "a", conversationID, requestA, "")
			conversationRecord(t, root, "unrelated", firstID, requestC, "")
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			if got, err := index.Resolve(context.Background(), []string{requestA}); err != nil || got[requestA] != conversationID {
				t.Fatalf("initial safe copy = %#v, %v", got, err)
			}
			path := conversationRecord(t, root, "b", conversationID, requestB, "")
			reviseConversation(t, path, map[string]any{"createdAt": int64(1788432998080)})
			switch kind {
			case "oversize":
				reviseConversation(t, path, map[string]any{"ignored": strings.Repeat("x", 1<<20)})
			case "hardlink":
				if err := os.Link(path, filepath.Join(root, "outside.json")); err != nil {
					t.Fatal(err)
				}
			case "symlink", "broken symlink":
				outside := filepath.Join(root, "outside.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if kind == "broken symlink" {
					outside += "-missing"
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "writable file", "unreadable file":
				mode := os.FileMode(0666)
				if kind == "unreadable file" {
					mode = 0000
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "invalid header":
				reviseConversation(t, path, map[string]any{"cliSessionId": nil})
			}
			// The new unknown alias forces a current scan rather than using the
			// previously validated copy in the three-second cache.
			got, err := index.Resolve(context.Background(), []string{requestA, requestB, requestC})
			if err != nil || !reflect.DeepEqual(got, map[string]string{requestC: firstID}) {
				t.Fatalf("rejected native filename failed to veto its group: %#v, %v", got, err)
			}
			info := index.Lookup(context.Background(), []string{requestA, requestC})
			if info[requestA].ConversationID != "" || info[requestA].Title != "Saved title" || info[requestC].ConversationID != firstID {
				t.Fatalf("rejection lost safe titles or published unsafe membership: %#v", info)
			}
		})
	}
}

func TestResolveIncompleteNativeDirectoryScanDisablesAllMembership(t *testing.T) {
	for _, kind := range []string{"account symlink", "workspace symlink", "broken account symlink", "writable workspace", "unreadable workspace"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "unreadable workspace" && os.Getuid() == 0 {
				t.Skip("root can enumerate mode-000 fixture directories")
			}
			root := fixtureRoot(t)
			conversationRecord(t, root, "a", conversationID, requestA, "")
			conversationRecord(t, root, "unrelated", firstID, requestC, "")
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			if got, err := index.Resolve(context.Background(), []string{requestA, requestC}); err != nil || len(got) != 2 {
				t.Fatalf("initial complete source = %#v, %v", got, err)
			}
			path := conversationRecord(t, root, "b", conversationID, requestB, "")
			dir := filepath.Dir(path)
			switch kind {
			case "account symlink", "workspace symlink", "broken account symlink":
				if kind != "workspace symlink" {
					dir = filepath.Dir(dir)
				}
				outside := filepath.Join(fixtureRoot(t), "hidden-directory")
				if err := os.Rename(dir, outside); err != nil {
					t.Fatal(err)
				}
				if kind == "broken account symlink" {
					outside += "-missing"
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			case "writable workspace", "unreadable workspace":
				mode := os.FileMode(0777)
				if kind == "unreadable workspace" {
					mode = 0000
				}
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			}
			got, err := index.Resolve(context.Background(), []string{requestA, requestB, requestC})
			if err != nil || len(got) != 0 {
				t.Fatalf("uninspected account/workspace published partial authority: %#v, %v", got, err)
			}
			info := index.Lookup(context.Background(), []string{requestA, requestC})
			if info[requestA].Title != "Saved title" || info[requestC].Title != "Saved title" || info[requestA].ConversationID != "" || info[requestC].ConversationID != "" {
				t.Fatalf("partial scan did not separate titles from membership: %#v", info)
			}
		})
	}
}

func TestLookupVerifiedMembershipSurvivesAmbiguousDisplayTitle(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, "")
	copy := conversationRecord(t, root, "copy", conversationID, requestA, "")
	reviseConversation(t, copy, map[string]any{"title": "Different saved title"})
	conversationRecord(t, root, "b", conversationID, requestB, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	want := map[string]string{requestA: conversationID, requestB: conversationID}
	if got, err := index.Resolve(context.Background(), []string{requestA, requestB}); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("display disagreement changed verified membership: %#v, %v", got, err)
	}
	info := index.Lookup(context.Background(), []string{requestA, requestB})
	for _, id := range []string{requestA, requestB} {
		if info[id].ConversationID != conversationID || info[id].ClientKind != "desktop" {
			t.Fatalf("Lookup lost a verified member needed by revision checks: %#v", info)
		}
	}
	if info[requestA].Title != "" || info[requestA].TitleSource != "" || info[requestB].Title != "Saved title" {
		t.Fatalf("ambiguous display title was guessed: %#v", info)
	}
	raw, err := json.Marshal(info[requestA])
	if err != nil || strings.Contains(string(raw), conversationID) || strings.Contains(string(raw), "conversation_id") {
		t.Fatalf("internal membership was injected into JSON: %s, %v", raw, err)
	}
	// A grouped caller can now construct a complete observed-member revision
	// map from Lookup without dropping the title-unknown member.
	revisions := map[string]uint64{requestA: 7, requestB: 11}
	members := make(map[string]uint64)
	for id, revision := range revisions {
		if info[id].ConversationID == conversationID {
			members[id] = revision
		}
	}
	if !reflect.DeepEqual(members, revisions) {
		t.Fatalf("title disagreement omitted a member revision: %#v", members)
	}
}

func TestResolveOrdinaryRootFilesDoNotMakeEnumerationIncomplete(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, "")
	saved(t, root, "archived-sessions.idx", `fixture ignored root file`)
	ignored := saved(t, root, "a/settings.json", `fixture ignored account file`)
	if err := os.Chmod(ignored, 0000); err != nil {
		t.Fatal(err)
	}
	got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA})
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID}) {
		t.Fatalf("ordinary non-directory entries invalidated a complete scan: %#v, %v", got, err)
	}
}
