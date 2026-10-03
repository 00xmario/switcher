package sessionmeta_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"switcher/internal/sessionmeta"
)

const (
	firstID  = "11111111-1111-4111-8111-111111111111"
	secondID = "22222222-2222-4222-8222-222222222222"
	thirdID  = "33333333-3333-4333-8333-333333333333"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func saved(t *testing.T, root, name, data string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookupMatchesOnlySavedCLISessionUUID(t *testing.T) {
	root := fixtureRoot(t)
	saved(t, root, "account/workspace/local_unrelated-name.json", `{"sessionId":"desktop-private-id","cliSessionId":"`+firstID+`","title":"  Fix relay cards  ","cwd":"/private/work/switcher","lastActivityAt":1780000000000,"messages":["fixture-private-message"],"token":"fixture-private-token"}`)
	saved(t, root, "account/workspace/local_"+secondID+".json", `{"sessionId":"`+secondID+`","cliSessionId":"`+thirdID+`","title":"Fork title","cwd":"/private/work/fork"}`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got := index.Lookup(context.Background(), []string{firstID, strings.ToUpper(thirdID), secondID, firstID[:8], "00000000-0000-0000-0000-000000000000", "../settings.json"})
	want := map[string]sessionmeta.Info{
		firstID: {Title: "Fix relay cards", Project: "switcher", TitleSource: "desktop", ClientKind: "desktop"},
		thirdID: {Title: "Fork title", Project: "fork", TitleSource: "desktop", ClientKind: "desktop"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup = %#v, want %#v", got, want)
	}
}

func TestLookupUsesDesktopTitleBeforeSavedCLIMetadata(t *testing.T) {
	root := fixtureRoot(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	saved(t, desktop, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":"Desktop title","cwd":"/work/desktop-project"}`)
	saved(t, desktop, "account/workspace/local_second.json", `{"cliSessionId":"`+secondID+`","title":" \n ","cwd":"/work/desktop-project"}`)
	saved(t, projects, "encoded-project/sessions-index.json", `{"version":1,"entries":[
		{"sessionId":"`+firstID+`","customTitle":"CLI stale title","summary":"CLI summary","projectPath":"/work/cli-project","firstPrompt":"fixture-private-first-prompt"},
		{"sessionId":"`+secondID+`","customTitle":"Saved custom title","summary":"Saved summary","projectPath":"/work/cli-project"},
		{"sessionId":"`+thirdID+`","summary":"Saved summary","projectPath":"/work/third-project"},
		{"sessionId":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","firstPrompt":"fixture-private-first-prompt","projectPath":"/work/untitled"},
		{"sessionId":"invalid","customTitle":"Never use this"},
		{"sessionId":false,"customTitle":"Malformed row"}
	]}`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects})
	const untitledID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
	want := map[string]sessionmeta.Info{
		firstID:    {Title: "Desktop title", Project: "desktop-project", TitleSource: "desktop", ClientKind: "desktop"},
		secondID:   {Title: "Saved custom title", Project: "desktop-project", TitleSource: "cli", ClientKind: "desktop"},
		thirdID:    {Title: "Saved summary", Project: "third-project", TitleSource: "cli", ClientKind: "cli"},
		untitledID: {Project: "untitled", ClientKind: "cli"},
	}
	if got := index.Lookup(context.Background(), []string{firstID, secondID, thirdID, untitledID}); !reflect.DeepEqual(got, want) {
		t.Fatalf("saved title precedence = %#v, want %#v", got, want)
	}
}

func TestLookupResolvesCopiesByActivityOrSkipsAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, older, newer string
		want               string
	}{
		{"newest activity", `"lastActivityAt":1780000000000`, `"lastActivityAt":1780000001000`, "New title"},
		{"ISO activity", `"lastActivityAt":"2026-01-01T00:00:00Z"`, `"lastActivityAt":"2026-01-02T00:00:00Z"`, "New title"},
		{"equal activity", `"lastActivityAt":1780000000000`, `"lastActivityAt":1780000000000`, ""},
		{"missing activity", `"createdAt":1780000000000`, `"lastActivityAt":1780000001000`, ""},
		{"invalid activity", `"lastActivityAt":"bad"`, `"lastActivityAt":1780000001000`, ""},
		{"out of range activity", `"lastActivityAt":1e100`, `"lastActivityAt":1780000001000`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reversed := range []bool{false, true} {
				root := fixtureRoot(t)
				old, newer := "a", "b"
				if reversed {
					old, newer = newer, old
				}
				saved(t, root, old+"/workspace/local_copy.json", `{"cliSessionId":"`+firstID+`","title":"Old title","cwd":"/work/old",`+tc.older+`}`)
				saved(t, root, newer+"/workspace/local_copy.json", `{"cliSessionId":"`+firstID+`","title":"New title","cwd":"/work/new",`+tc.newer+`}`)
				got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID})
				if tc.want == "" {
					if len(got) != 0 {
						t.Fatalf("ambiguous copies selected %#v", got)
					}
				} else if got[firstID].Title != tc.want || got[firstID].Project != "new" {
					t.Fatalf("newest copy = %#v", got)
				}
			}
		})
	}
	t.Run("identical copies and different UUID fork", func(t *testing.T) {
		root := fixtureRoot(t)
		for _, account := range []string{"a", "b"} {
			saved(t, root, account+"/workspace/local_copy.json", `{"cliSessionId":"`+firstID+`","title":"Same title","cwd":"/work/same"}`)
		}
		saved(t, root, "b/workspace/local_fork.json", `{"cliSessionId":"`+secondID+`","title":"Fork title","cwd":"/work/fork","parentSessionId":"`+firstID+`"}`)
		got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID, secondID, thirdID})
		if len(got) != 2 || got[firstID].Title != "Same title" || got[secondID].Title != "Fork title" {
			t.Fatalf("copies/fork = %#v", got)
		}
	})
	t.Run("CLI conflicts fall back to absence", func(t *testing.T) {
		root := fixtureRoot(t)
		saved(t, root, "a/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","summary":"One","projectPath":"/work/a"}]}`)
		saved(t, root, "b/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","summary":"Two","projectPath":"/work/b"}]}`)
		if got := sessionmeta.New(sessionmeta.Config{ProjectsRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("ambiguous CLI records selected %#v", got)
		}
	})
	t.Run("same basename is not the same saved cwd", func(t *testing.T) {
		root := fixtureRoot(t)
		for j, cwd := range []string{"/work/a/project", "/work/b/project"} {
			saved(t, root, string(rune('a'+j))+"/workspace/local_copy.json", `{"cliSessionId":"`+firstID+`","title":"Same title","cwd":"`+cwd+`"}`)
		}
		if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("different saved cwd copies selected %#v", got)
		}
	})
	t.Run("CLI conflict beyond display clamp", func(t *testing.T) {
		root := fixtureRoot(t)
		for _, suffix := range []string{"a", "b"} {
			saved(t, root, suffix+"/sessions-index.json", `{"entries":[{"sessionId":"`+firstID+`","customTitle":"`+strings.Repeat("x", 160)+suffix+`"}]}`)
		}
		if got := sessionmeta.New(sessionmeta.Config{ProjectsRoot: root}).Lookup(context.Background(), []string{firstID}); len(got) != 0 {
			t.Fatalf("display clamp concealed conflicting saved titles: %#v", got)
		}
	})
}

func TestLookupNormalizesDisplayFieldsWithoutInterpretingMarkup(t *testing.T) {
	root := fixtureRoot(t)
	saved(t, root, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","title":" \u0000Fix\n\t<b>cards</b>\u007f\u202e ","cwd":"/private/work/project/"}`)
	saved(t, root, "account/workspace/local_second.json", `{"cliSessionId":"`+secondID+`","title":"`+strings.Repeat("界", 180)+`","cwd":"https://example.invalid/private"}`)
	saved(t, root, "account/workspace/local_third.json", `{"cliSessionId":"`+thirdID+`","title":"\u0000\n\t","cwd":"/"}`)
	got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID, secondID, thirdID})
	if got[firstID].Title != "Fix <b>cards</b>" || got[firstID].Project != "project" {
		t.Fatalf("normalized metadata = %#v", got[firstID])
	}
	if !utf8.ValidString(got[secondID].Title) || utf8.RuneCountInString(got[secondID].Title) != 160 || got[secondID].Project != "" {
		t.Fatalf("bounded title or nonlocal project = %#v", got[secondID])
	}
	if got[thirdID] != (sessionmeta.Info{ClientKind: "desktop"}) {
		t.Fatalf("unknown title fabricated display data: %#v", got[thirdID])
	}
}
