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

const (
	conversationID = "92ef147b-9a97-440c-a9c4-073a2b683cb3"
	requestA       = "f1992147-b857-4b8b-b1c9-e22b0ba542ac"
	requestB       = "57b82f5b-7be7-49de-82e4-63f18abc87c1"
	requestC       = "44444444-4444-4444-8444-444444444444"
)

func conversationRecord(t *testing.T, root, account, conversation, request, extra string) string {
	t.Helper()
	return saved(t, root, account+"/workspace/local_"+conversation+".json", `{"sessionId":"local_`+conversation+`","cliSessionId":"`+request+`","createdAt":1788432998079,"cwd":"/fixture/work/Salenza","title":"Saved title"`+extra+`}`)
}

func TestResolveNativeConversationAcrossAccountCopies(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "account-a", conversationID, requestA, `,"bridgeSessionIds":["bridge-a","bridge-b"]`)
	conversationRecord(t, root, "account-b", conversationID, requestB, `,"bridgeSessionIds":["bridge-b","bridge-c"]`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got, err := index.Resolve(context.Background(), []string{requestA, strings.ToUpper(requestB), requestC})
	want := map[string]string{requestA: conversationID, strings.ToUpper(requestB): conversationID}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("verified native aliases = %#v, %v; want %#v", got, err, want)
	}
	// All known source members must be indexed even when only one is requested.
	one, err := index.Resolve(context.Background(), []string{requestB})
	if err != nil || one[requestB] != conversationID {
		t.Fatalf("unqueried source member was omitted: %#v, %v", one, err)
	}
	info := index.Lookup(context.Background(), []string{requestA, requestB})
	for _, id := range []string{requestA, requestB} {
		if info[id].ConversationID != conversationID || info[id].Title != "Saved title" || info[id].TitleSource != "desktop" {
			t.Fatalf("display metadata lost its exact source or identity: %#v", info[id])
		}
		raw, err := json.Marshal(info[id])
		if err != nil || strings.Contains(string(raw), "conversation") || strings.Contains(string(raw), conversationID) {
			t.Fatalf("internal identity was serialized implicitly: %s, %v", raw, err)
		}
	}
	got[requestA] = "caller-mutation"
	again, err := index.Resolve(context.Background(), []string{requestA})
	if err != nil || again[requestA] != conversationID {
		t.Fatalf("caller changed cached membership: %#v, %v", again, err)
	}
}

func TestResolveTitleOnlyAndCLIIndexNeverCreateConversationIdentity(t *testing.T) {
	root := fixtureRoot(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	saved(t, desktop, "account/workspace/local_title-only.json", `{"cliSessionId":"`+requestA+`","title":"Saved title","cwd":"/fixture/work/Salenza"}`)
	saved(t, projects, "project/sessions-index.json", `{"entries":[{"sessionId":"`+requestB+`","customTitle":"Saved title","projectPath":"/fixture/work/Salenza","createdAt":1788432998079}]}`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects})
	got, err := index.Resolve(context.Background(), []string{requestA, requestB})
	if err != nil || len(got) != 0 {
		t.Fatalf("display-only sources created authority: %#v, %v", got, err)
	}
	info := index.Lookup(context.Background(), []string{requestA, requestB})
	if len(info) != 2 || info[requestA].TitleSource != "desktop" || info[requestB].TitleSource != "cli" || info[requestA].ConversationID != "" || info[requestB].ConversationID != "" {
		t.Fatalf("legacy title lookup changed: %#v", info)
	}
}

func reviseConversation(t *testing.T, path string, fields map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		if value == nil {
			delete(record, key)
		} else {
			record[key] = value
		}
	}
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConflictingCopiesDisableEveryGroupMember(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
	}{
		{"different creation", map[string]any{"createdAt": int64(1788432998080)}},
		{"missing creation", map[string]any{"createdAt": nil}},
		{"zero creation", map[string]any{"createdAt": 0}},
		{"negative creation", map[string]any{"createdAt": -1}},
		{"string creation", map[string]any{"createdAt": "2026-09-03T00:00:00Z"}},
		{"same basename different cwd", map[string]any{"cwd": "/fixture/other/Salenza"}},
		{"relative cwd", map[string]any{"cwd": "Salenza"}},
		{"missing cwd", map[string]any{"cwd": nil}},
		{"noncanonical cwd", map[string]any{"cwd": "/fixture/work/../work/Salenza"}},
		{"missing native ID", map[string]any{"sessionId": nil}},
		{"nonlocal native ID", map[string]any{"sessionId": conversationID}},
		{"zero native ID", map[string]any{"sessionId": "local_00000000-0000-0000-0000-000000000000"}},
		{"wrong type native ID", map[string]any{"sessionId": false}},
		{"missing CLI ID", map[string]any{"cliSessionId": nil}},
		{"invalid CLI ID", map[string]any{"cliSessionId": "not-a-UUID"}},
		{"parent field", map[string]any{"parentSessionId": requestA}},
		{"malformed parent flag", map[string]any{"parentSessionId": false}},
		{"malformed parent zero", map[string]any{"parentSessionId": 0}},
		{"malformed parent object", map[string]any{"parentSessionId": map[string]any{}}},
		{"malformed parent array", map[string]any{"parentSessionId": []any{}}},
		{"fork field", map[string]any{"forkedFromSessionId": "local_" + conversationID}},
		{"nested fork field", map[string]any{"forkedFrom": map[string]any{"sessionId": requestA}}},
		{"child flag", map[string]any{"isChild": true}},
		{"malformed child flag", map[string]any{"isChild": ""}},
		{"agent field", map[string]any{"agentId": "agent-fixture"}},
		{"wrong type bridge field", map[string]any{"bridgeSessionIds": false}},
		{"wrong type bridge object", map[string]any{"bridgeSessionIds": map[string]any{}}},
		{"empty string bridge field", map[string]any{"bridgeSessionIds": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t)
			conversationRecord(t, root, "a", conversationID, requestA, "")
			path := conversationRecord(t, root, "b", conversationID, requestB, "")
			reviseConversation(t, path, tc.fields)
			index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
			got, err := index.Resolve(context.Background(), []string{requestA})
			if err != nil || len(got) != 0 {
				t.Fatalf("unqueried conflicting copy did not veto group: %#v, %v", got, err)
			}
			for _, info := range index.Lookup(context.Background(), []string{requestA, requestB}) {
				if info.ConversationID != "" {
					t.Fatalf("invalid membership leaked through Info: %#v", info)
				}
			}
		})
	}
}

func TestResolveSameCLIIDInDifferentNativeIdentitiesVetoesBothGroups(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, `,"lastActivityAt":1788432999000`)
	conversationRecord(t, root, "b", conversationID, requestB, "")
	conversationRecord(t, root, "c", firstID, requestA, `,"lastActivityAt":1788433009000`)
	conversationRecord(t, root, "d", firstID, requestC, "")
	got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA, requestB, requestC})
	if err != nil || len(got) != 0 {
		t.Fatalf("newest activity picked binding authority or peer escaped veto: %#v, %v", got, err)
	}
}

func TestResolveInvalidNativeCopyCannotBorrowOtherCopyThroughCLIID(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, "")
	conversationRecord(t, root, "b", conversationID, requestB, "")
	saved(t, root, "c/workspace/local_unknown.json", `{"sessionId":false,"cliSessionId":"`+requestA+`","title":"Unknown native record"}`)
	got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA, requestB})
	if err != nil || len(got) != 0 {
		t.Fatalf("unverifiable same-CLI copy silently borrowed authority: %#v, %v", got, err)
	}
}

func TestResolveNativeIdentityAppearingForDisplayKnownAliasRefreshes(t *testing.T) {
	root := fixtureRoot(t)
	path := conversationRecord(t, root, "a", conversationID, requestA, "")
	reviseConversation(t, path, map[string]any{"sessionId": nil})
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	if info := index.Lookup(context.Background(), []string{requestA}); info[requestA].Title != "Saved title" || info[requestA].ConversationID != "" {
		t.Fatalf("staged display-only metadata = %#v", info)
	}
	reviseConversation(t, path, map[string]any{"sessionId": "local_" + conversationID})
	got, err := index.Resolve(context.Background(), []string{requestA})
	if err != nil || got[requestA] != conversationID {
		t.Fatalf("unmapped cached alias did not get a current scan: %#v, %v", got, err)
	}
}

func TestResolveBridgeEvidenceUsesAllCopiesAndConnectedOverlap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  []string
		linked bool
	}{
		{"native tuple without bridges", []string{"", ""}, true},
		{"one copy without bridges", []string{`,"bridgeSessionIds":["bridge-a"]`, ""}, true},
		{"overlap", []string{`,"bridgeSessionIds":["bridge-a","bridge-b"]`, `,"bridgeSessionIds":["bridge-b","bridge-c"]`}, true},
		{"transitive unqueried copy", []string{`,"bridgeSessionIds":["bridge-a"]`, `,"bridgeSessionIds":["bridge-b"]`, `,"bridgeSessionIds":["bridge-a","bridge-b"]`}, true},
		{"disjoint", []string{`,"bridgeSessionIds":["bridge-a"]`, `,"bridgeSessionIds":["bridge-b"]`}, false},
		{"empty copy cannot connect disjoint bridges", []string{`,"bridgeSessionIds":["bridge-a"]`, `,"bridgeSessionIds":["bridge-b"]`, `,"bridgeSessionIds":[]`}, false},
		{"bridge IDs stay exact", []string{`,"bridgeSessionIds":["Bridge-a"]`, `,"bridgeSessionIds":["bridge-a"]`}, false},
		{"null default lineage is not a parent", []string{`,"parentSessionId":null,"forkedFromSessionId":"","isChild":false`, ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t)
			ids := []string{requestA, requestB, requestC}
			for j, extra := range tc.extra {
				path := conversationRecord(t, root, string(rune('a'+j)), conversationID, ids[j], extra)
				if j == 2 {
					workspace := filepath.Dir(path)
					if err := os.Rename(workspace, filepath.Join(filepath.Dir(workspace), "another-workspace")); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA, requestB})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.linked {
				if len(got) != 0 {
					t.Fatalf("conflicting bridge histories linked: %#v", got)
				}
			} else if !reflect.DeepEqual(got, map[string]string{requestA: conversationID, requestB: conversationID}) {
				t.Fatalf("corroborated native copies not linked: %#v", got)
			}
		})
	}
}

func TestResolveTitlesModelsAndBridgeIDsCannotLinkDifferentNativeIDs(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", conversationID, requestA, `,"model":"Opus","bridgeSessionIds":["shared-bridge"]`)
	conversationRecord(t, root, "b", firstID, requestB, `,"model":"Haiku","bridgeSessionIds":["shared-bridge"]`)
	path := conversationRecord(t, root, "c", conversationID, requestC, `,"model":"Haiku","bridgeSessionIds":["shared-bridge"]`)
	reviseConversation(t, path, map[string]any{"title": "Renamed native copy"})
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	want := map[string]string{requestA: conversationID, requestB: firstID, requestC: conversationID}
	got, err := index.Resolve(context.Background(), []string{requestA, requestB, requestC})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("display fields or bridge alone determined membership: %#v, %v", got, err)
	}
	if info := index.Lookup(context.Background(), []string{requestC}); info[requestC].Title != "Renamed native copy" || info[requestC].ConversationID != conversationID {
		t.Fatalf("native identity changed display title selection: %#v", info)
	}
}

func TestResolveRequiresExactNativeFilenameAndUnambiguousJSON(t *testing.T) {
	for _, kind := range []string{"wrong filename", "duplicate native identity", "duplicate creation", "duplicate CLI identity"} {
		t.Run(kind, func(t *testing.T) {
			root := fixtureRoot(t)
			conversationRecord(t, root, "a", conversationID, requestA, "")
			var path string
			switch kind {
			case "wrong filename":
				path = conversationRecord(t, root, "b", conversationID, requestB, "")
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), "local_"+requestB+".json")); err != nil {
					t.Fatal(err)
				}
			case "duplicate native identity":
				conversationRecord(t, root, "b", conversationID, requestB, `,"SESSIONID":"local_`+conversationID+`"`)
			case "duplicate creation":
				conversationRecord(t, root, "b", conversationID, requestB, `,"CREATEDAT":1788432998079`)
			case "duplicate CLI identity":
				conversationRecord(t, root, "b", conversationID, requestB, `,"CLISESSIONID":"`+requestA+`"`)
			}
			got, err := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Resolve(context.Background(), []string{requestA, requestB})
			if err != nil || len(got) != 0 {
				t.Fatalf("mismatched or ambiguous native source created membership: %#v, %v", got, err)
			}
		})
	}
}

func TestResolveCanonicalUUIDsHaveNoFilenameOrParentFallback(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "a", strings.ToUpper(conversationID), strings.ToUpper(requestA), "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got, err := index.Resolve(context.Background(), []string{requestA, requestB, conversationID, requestA[:8], "local_" + conversationID, "00000000-0000-0000-0000-000000000000", "../history.jsonl"})
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestA: conversationID}) {
		t.Fatalf("nonexact aliases or IDs resolved: %#v, %v", got, err)
	}
}
