package sessionmeta_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/sessionmeta"
)

var _ interface {
	ResolveHistorical(context.Context, []string, map[string]string) (map[string]string, error)
} = (*sessionmeta.Index)(nil)

func TestResolveHistoricalRetainsOnlyPreviouslyVerifiedRotatedAlias(t *testing.T) {
	root := fixtureRoot(t)
	rotating := conversationRecord(t, root, "a", conversationID, requestA, `,"bridgeSessionIds":["bridge-shared"]`)
	conversationRecord(t, root, "c", conversationID, requestC, `,"bridgeSessionIds":["bridge-shared","bridge-next"]`)
	previous, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA})
	if err != nil || !reflect.DeepEqual(previous, map[string]string{requestA: conversationID}) {
		t.Fatalf("initial verified evidence = %#v, %v", previous, err)
	}
	reviseConversation(t, rotating, map[string]any{"cliSessionId": requestB})
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	ids := []string{requestA, requestB, requestC, firstID}
	current := map[string]string{requestB: conversationID, requestC: conversationID}
	if got, err := index.Resolve(context.Background(), ids); err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("default resolver invented historical membership: %#v, %v", got, err)
	}
	want := map[string]string{requestA: conversationID, requestB: conversationID, requestC: conversationID}
	if got, err := index.ResolveHistorical(context.Background(), ids, previous); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("verified rotation history = %#v, %v; want %#v", got, err, want)
	}
	if !reflect.DeepEqual(previous, map[string]string{requestA: conversationID}) {
		t.Fatal("resolver mutated the core's verified history")
	}
	if got, err := index.ResolveHistorical(context.Background(), ids, nil); err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("missing evidence inherited a conversation: %#v, %v", got, err)
	}
	if got, err := index.Resolve(context.Background(), ids); err != nil || !reflect.DeepEqual(got, current) {
		t.Fatalf("historical output contaminated current cache: %#v, %v", got, err)
	}
	if info := index.Lookup(context.Background(), []string{requestA}); len(info) != 0 {
		t.Fatalf("historical membership invented display metadata: %#v", info)
	}
}

func TestResolveHistoricalCurrentClaimsBlockFallback(t *testing.T) {
	for _, kind := range []string{"current different group", "invalid creation", "missing native identity", "wrong type native identity", "invalid title", "duplicate CLI identity", "conflicting duplicate CLI identity", "partial JSON", "hardlinked claim", "symlink claim", "disputed groups"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			conversationRecord(t, root, "current", conversationID, requestB, "")
			path := conversationRecord(t, root, "claim", firstID, requestA, "")
			var expected string
			switch kind {
			case "current different group":
				expected = firstID
			case "invalid creation":
				reviseConversation(t, path, map[string]any{"createdAt": nil})
			case "missing native identity":
				reviseConversation(t, path, map[string]any{"sessionId": nil})
			case "wrong type native identity":
				reviseConversation(t, path, map[string]any{"sessionId": false})
			case "invalid title":
				reviseConversation(t, path, map[string]any{"title": false})
			case "duplicate CLI identity", "conflicting duplicate CLI identity":
				other := requestA
				if kind == "conflicting duplicate CLI identity" {
					other = requestC
				}
				conversationRecord(t, root, "claim", firstID, requestA, `,"CLISESSIONID":"`+other+`"`)
			case "partial JSON":
				if err := os.WriteFile(path, []byte(`{"cliSessionId":"`+requestA+`","title":"partial`), 0600); err != nil {
					t.Fatal(err)
				}
			case "hardlinked claim":
				if err := os.Link(path, filepath.Join(root, "outside.json")); err != nil {
					t.Fatal(err)
				}
			case "symlink claim":
				outside := filepath.Join(root, "outside.json")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			case "disputed groups":
				conversationRecord(t, root, "other", secondID, requestA, "")
			}
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			known := map[string]string{requestA: conversationID}
			got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, known)
			want := map[string]string{requestB: conversationID}
			if expected != "" {
				want[requestA] = expected
			}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("current claim was replaced by stale history: %#v, %v", got, err)
			}
			if current, err := index.Resolve(context.Background(), []string{requestB}); err != nil || current[requestB] != conversationID {
				t.Fatalf("historical claim checks changed current resolution: %#v, %v", current, err)
			}
		})
	}
}

func TestResolveHistoricalRequiresCurrentCompleteUnvetoedGroup(t *testing.T) {
	for _, kind := range []string{"deleted group", "missing root", "creation conflict", "cwd conflict", "bridge conflict", "parent relationship", "fork relationship", "oversized copy", "hardlinked copy", "unsafe workspace", "symlink workspace", "truncated scan"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			b := conversationRecord(t, root, "b", conversationID, requestB, `,"bridgeSessionIds":["bridge-shared"]`)
			c := conversationRecord(t, root, "c", conversationID, requestC, `,"bridgeSessionIds":["bridge-shared"]`)
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			if got, err := index.Resolve(context.Background(), []string{requestB, requestC}); err != nil || len(got) != 2 {
				t.Fatalf("initial group proof = %#v, %v", got, err)
			}
			switch kind {
			case "deleted group":
				for _, path := range []string{b, c} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			case "missing root":
				if err := os.Rename(root, root+"-renamed"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Rename(root+"-renamed", root) })
			case "creation conflict":
				reviseConversation(t, c, map[string]any{"createdAt": int64(1788432998080)})
			case "cwd conflict":
				reviseConversation(t, c, map[string]any{"cwd": "/fixture/other/Salenza"})
			case "bridge conflict":
				reviseConversation(t, c, map[string]any{"bridgeSessionIds": []string{"disjoint"}})
			case "parent relationship":
				reviseConversation(t, c, map[string]any{"parentSessionId": requestA})
			case "fork relationship":
				reviseConversation(t, c, map[string]any{"forkedFromSessionId": "local_" + firstID})
			case "oversized copy":
				reviseConversation(t, c, map[string]any{"ignored": strings.Repeat("x", 1<<20)})
			case "hardlinked copy":
				if err := os.Link(c, filepath.Join(root, "outside.json")); err != nil {
					t.Fatal(err)
				}
			case "unsafe workspace":
				if err := os.Chmod(filepath.Dir(c), 0777); err != nil {
					t.Fatal(err)
				}
			case "symlink workspace":
				dir, outside := filepath.Dir(c), filepath.Join(fixtureRoot(t), "outside")
				if err := os.Rename(dir, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			case "truncated scan":
				for j := 0; j < 4096; j++ {
					saved(t, root, fmt.Sprintf("b/workspace/ignored_%04d.tmp", j), `{}`)
				}
			}
			got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB, requestC}, map[string]string{requestA: conversationID})
			if err != nil && !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
				t.Fatalf("invalid current proof retained historical authority: %#v, %v", got, err)
			}
		})
	}
}

func TestResolveHistoricalIgnoresDisplayAmbiguityAndClaimsInBothDuplicateValues(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "current", conversationID, requestB, "")
	copy := conversationRecord(t, root, "copy", conversationID, requestB, "")
	reviseConversation(t, copy, map[string]any{"title": "Different title"})
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	known := map[string]string{requestA: conversationID, requestC: conversationID}
	if got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB, requestC}, known); err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID, requestC: conversationID}) {
		t.Fatalf("title ambiguity affected current group proof: %#v, %v", got, err)
	}
	conversationRecord(t, root, "bad", firstID, requestA, `,"cli\u0053essionId":"`+requestC+`"`)
	// Validate the new source snapshot. Repeated missing aliases now remain
	// negative-cached for the existing snapshot's TTL.
	index = sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB, requestC}, known)
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestB: conversationID}) {
		t.Fatalf("one duplicate identity value escaped the current-claim veto: %#v, %v", got, err)
	}
}

func TestResolveHistoricalCanonicalEvidenceIsScopedToEachCall(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "g", conversationID, requestB, "")
	conversationRecord(t, root, "h", firstID, requestC, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	for _, group := range []string{conversationID, firstID} {
		known := map[string]string{strings.ToUpper(requestA): strings.ToUpper(group)}
		if got, err := index.ResolveHistorical(context.Background(), []string{requestA}, known); err != nil || !reflect.DeepEqual(got, map[string]string{requestA: group}) {
			t.Fatalf("caller-specific evidence mixed namespaces: %#v, %v", got, err)
		}
	}
	for _, known := range []map[string]string{
		nil,
		{requestA: "local_" + conversationID},
		{requestA: "00000000-0000-0000-0000-000000000000"},
		{requestA: conversationID, strings.ToUpper(requestA): firstID},
		{requestA: conversationID, strings.ToUpper(requestA): "invalid"},
		{"../session": conversationID},
	} {
		if got, err := index.ResolveHistorical(context.Background(), []string{requestA}, known); err != nil || len(got) != 0 {
			t.Fatalf("invalid or disputed prior evidence granted authority: %#v, %v", got, err)
		}
	}
}

func TestResolveHistoricalCanceledNilAndConcurrentCalls(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "current", conversationID, requestB, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	known := map[string]string{requestA: conversationID}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := index.ResolveHistorical(ctx, []string{requestA}, known); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("canceled historical lookup = %#v, %v", got, err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if got, err := index.ResolveHistorical(deadline, []string{requestA}, known); !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Fatalf("expired historical lookup = %#v, %v", got, err)
	}
	var unset *sessionmeta.Index
	if got, err := unset.ResolveHistorical(context.Background(), []string{requestA}, known); err != nil || len(got) != 0 {
		t.Fatalf("nil index supplied historical authority: %#v, %v", got, err)
	}
	var wg sync.WaitGroup
	for j := 0; j < 24; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, known)
			if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID}) {
				t.Errorf("concurrent historical resolution = %#v, %v", got, err)
			}
			got[requestA] = "caller mutation"
		}()
	}
	wg.Wait()
	if current, err := index.Resolve(context.Background(), []string{requestA, requestB}); err != nil || !reflect.DeepEqual(current, map[string]string{requestB: conversationID}) {
		t.Fatalf("concurrent history changed current cache: %#v, %v", current, err)
	}
}

func TestResolveHistoricalDoesNotTreatIgnoredContentAsNativeClaims(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "current", conversationID, requestB, `,"firstPrompt":"{\"cliSessionId\":\"`+requestA+`\"}","unknown":{"cliSessionId":"`+requestA+`"}`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	if got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, nil); err != nil || !reflect.DeepEqual(got, map[string]string{requestB: conversationID}) {
		t.Fatalf("ignored content invented historical membership: %#v, %v", got, err)
	}
	if got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, map[string]string{requestA: conversationID}); err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID}) {
		t.Fatalf("ignored content was interpreted as a native claim: %#v, %v", got, err)
	}
}

func TestResolveHistoricalOpaqueReservedAliasDisablesOnlyHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"array", []string{requestA}},
		{"object", map[string]any{"id": requestA}},
		{"number", 123},
		{"boolean", false},
		{"null", json.RawMessage("null")},
		{"empty string", ""},
		{"invalid UUID string", "opaque alias"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t)
			conversationRecord(t, root, "healthy", conversationID, requestB, "")
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			known := map[string]string{requestA: conversationID}
			if got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, known); err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID}) {
				t.Fatalf("initial readable snapshot = %#v, %v", got, err)
			}
			path := conversationRecord(t, root, "opaque", firstID, requestC, "")
			reviseConversation(t, path, map[string]any{"cliSessionId": tc.value})
			// A new snapshot must reject history despite the previous proof.
			// Repeated misses on the old snapshot are cached until its TTL.
			index = sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			want := map[string]string{requestB: conversationID}
			if got, err := index.ResolveHistorical(context.Background(), []string{requestA, requestB}, known); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("opaque reserved field was treated as alias absence: %#v, %v", got, err)
			}
			if got, err := index.Resolve(context.Background(), []string{requestA, requestB}); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("opaque history check removed independently valid current mappings: %#v, %v", got, err)
			}
			info := index.Lookup(context.Background(), []string{requestA, requestB})
			if len(info) != 1 || info[requestB].Title != "Saved title" || info[requestB].ConversationID != conversationID || info[requestA].ConversationID != "" {
				t.Fatalf("opaque history check changed current lookup metadata: %#v", info)
			}
		})
	}
}
