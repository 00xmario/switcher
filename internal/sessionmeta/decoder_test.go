package sessionmeta_test

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"switcher/internal/sessionmeta"
)

func TestLookupRejectsDuplicateDesktopKeysBeforeMatching(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
	}{
		{"conflicting identities", `"cliSessionId":"` + firstID + `","cliSessionId":"` + secondID + `","title":"Untrusted title"`},
		{"identical identities", `"cliSessionId":"` + firstID + `","cliSessionId":"` + firstID + `","title":"Untrusted title"`},
		{"case variant identity", `"cliSessionId":"` + firstID + `","CLISESSIONID":"` + secondID + `","title":"Untrusted title"`},
		{"escaped identity", `"cliSessionId":"` + firstID + `","cli\u0053essionId":"` + secondID + `","title":"Untrusted title"`},
		{"Unicode fold identity", `"cliSessionId":"` + firstID + `","cliſessionId":"` + secondID + `","title":"Untrusted title"`},
		{"duplicate title", `"cliSessionId":"` + firstID + `","title":"One","TITLE":"Two"`},
		{"duplicate cwd", `"cliSessionId":"` + firstID + `","title":"Untrusted title","cwd":"/work/one","CWD":"/work/two"`},
		{"duplicate activity", `"cliSessionId":"` + firstID + `","title":"Untrusted title","lastActivityAt":1780000000000,"LASTACTIVITYAT":1780000001000`},
		{"nested unknown duplicate", `"cliSessionId":"` + firstID + `","title":"Untrusted title","unknown":{"key":1,"KEY":2}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t)
			saved(t, root, "account/workspace/local_first.json", `{`+tc.fields+`}`)
			got := sessionmeta.New(sessionmeta.Config{DesktopRoot: root}).Lookup(context.Background(), []string{firstID, secondID})
			if len(got) != 0 {
				t.Fatalf("ambiguous Desktop record reached matching: %#v", got)
			}
		})
	}
}

func TestLookupRejectsDuplicateCLIKeysAndIndexEntries(t *testing.T) {
	for _, tc := range []struct {
		name, data string
	}{
		{"conflicting identities", `{"entries":[{"sessionId":"` + firstID + `","sessionId":"` + secondID + `","summary":"Untrusted title"}]}`},
		{"identical identities", `{"entries":[{"sessionId":"` + firstID + `","sessionId":"` + firstID + `","summary":"Untrusted title"}]}`},
		{"case variant identity", `{"entries":[{"sessionId":"` + firstID + `","SESSIONID":"` + secondID + `","summary":"Untrusted title"}]}`},
		{"escaped identity", `{"entries":[{"sessionId":"` + firstID + `","\u0073essionId":"` + secondID + `","summary":"Untrusted title"}]}`},
		{"Unicode fold identity", `{"entries":[{"sessionId":"` + firstID + `","ſessionId":"` + secondID + `","summary":"Untrusted title"}]}`},
		{"duplicate custom title", `{"entries":[{"sessionId":"` + firstID + `","customTitle":"One","CUSTOMTITLE":"Two"}]}`},
		{"duplicate summary", `{"entries":[{"sessionId":"` + firstID + `","summary":"One","SUMMARY":"Two"}]}`},
		{"duplicate project", `{"entries":[{"sessionId":"` + firstID + `","summary":"Untrusted title","projectPath":"/work/one","PROJECTPATH":"/work/two"}]}`},
		{"duplicate entries", `{"entries":[{"sessionId":"` + firstID + `","summary":"One"}],"entries":[{"sessionId":"` + secondID + `","summary":"Two"}]}`},
		{"case variant entries", `{"entries":[{"sessionId":"` + firstID + `","summary":"One"}],"ENTRIES":[{"sessionId":"` + secondID + `","summary":"Two"}]}`},
		{"identical entries", `{"entries":[{"sessionId":"` + firstID + `","summary":"One"}],"entries":[{"sessionId":"` + firstID + `","summary":"One"}]}`},
		{"nested unknown duplicate", `{"entries":[{"sessionId":"` + firstID + `","summary":"Untrusted title","unknown":[{"key":1,"KEY":2}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureRoot(t)
			saved(t, root, "project/sessions-index.json", tc.data)
			got := sessionmeta.New(sessionmeta.Config{ProjectsRoot: root}).Lookup(context.Background(), []string{firstID, secondID})
			if len(got) != 0 {
				t.Fatalf("ambiguous CLI metadata reached matching: %#v", got)
			}
		})
	}
}

func TestLookupAmbiguousDesktopFallsThroughToIndependentCLIRecord(t *testing.T) {
	root := fixtureRoot(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	saved(t, desktop, "account/workspace/local_first.json", `{"cliSessionId":"`+firstID+`","CLISESSIONID":"`+secondID+`","title":"Untrusted Desktop title","lastActivityAt":1780000001000}`)
	saved(t, desktop, "account/workspace/local_trusted.json", `{"cliSessionId":"`+firstID+`","title":"Trusted Desktop title","lastActivityAt":1780000000000}`)
	saved(t, projects, "a/sessions-index.json", `{"entries":[{"sessionId":"`+secondID+`","SESSIONID":"`+thirdID+`","customTitle":"Untrusted CLI title"}]}`)
	saved(t, projects, "b/sessions-index.json", `{"entries":[{"sessionId":"`+secondID+`","customTitle":"Trusted CLI title"}]}`)
	got := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}).Lookup(context.Background(), []string{firstID, secondID, thirdID})
	if len(got) != 2 || got[firstID].Title != "Trusted Desktop title" || got[firstID].TitleSource != "desktop" || got[secondID].Title != "Trusted CLI title" || got[secondID].TitleSource != "cli" {
		t.Fatalf("ambiguous identity contributed a title or alias: %#v", got)
	}
}

func TestLookupUniqueDecoderAliasesAndSeparateObjectsRemainValid(t *testing.T) {
	root := fixtureRoot(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	saved(t, desktop, "account/workspace/local_first.json", `{"CLISESSIONID":"`+firstID+`","TITLE":"Desktop title","CWD":"/work/desktop","unknown":{"key":1},"other":{"KEY":2},"firstPrompt":"{\"sessionId\":\"one\",\"SESSIONID\":\"two\"}"}`)
	saved(t, desktop, "account/workspace/local_second.json", `{"cliſessionId":"`+secondID+`","title":"Unicode alias title"}`)
	saved(t, projects, "project/sessions-index.json", `{"ENTRIES":[{"ſessionId":"`+thirdID+`","CUSTOMTITLE":"CLI title","PROJECTPATH":"/work/cli","unknown":{"key":1},"other":{"KEY":2}}]}`)
	want := map[string]sessionmeta.Info{
		firstID:  {Title: "Desktop title", Project: "desktop", TitleSource: "desktop", ClientKind: "desktop"},
		secondID: {Title: "Unicode alias title", TitleSource: "desktop", ClientKind: "desktop"},
		thirdID:  {Title: "CLI title", Project: "cli", TitleSource: "cli", ClientKind: "cli"},
	}
	if got := sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}).Lookup(context.Background(), []string{firstID, secondID, thirdID}); !reflect.DeepEqual(got, want) {
		t.Fatalf("unique Go decoder aliases or separate objects rejected: %#v", got)
	}
}

func TestLookupJSONContainerDepthIsBoundedAt64(t *testing.T) {
	for _, source := range []string{"Desktop", "CLI"} {
		for _, exceeds := range []bool{false, true} {
			name := source + "/64 layers"
			if exceeds {
				name = source + "/65 layers"
			}
			t.Run(name, func(t *testing.T) {
				root := fixtureRoot(t)
				cfg := sessionmeta.Config{DesktopRoot: root}
				path, layers := "account/workspace/local_first.json", 63
				if source == "CLI" {
					cfg = sessionmeta.Config{ProjectsRoot: root}
					path, layers = "project/sessions-index.json", 61
				}
				if exceeds {
					layers++
				}
				nested := strings.Repeat("[", layers) + `"ignored scalar"` + strings.Repeat("]", layers)
				data := `{"cliSessionId":"` + firstID + `","title":"Trusted title","unknown":` + nested + `}`
				if source == "CLI" {
					data = `{"entries":[{"sessionId":"` + firstID + `","summary":"Trusted title","unknown":` + nested + `}]}`
				}
				saved(t, root, path, data)
				got := sessionmeta.New(cfg).Lookup(context.Background(), []string{firstID})
				if exceeds {
					if len(got) != 0 {
						t.Fatalf("over-depth metadata accepted: %#v", got)
					}
				} else if len(got) != 1 || got[firstID].Title != "Trusted title" {
					t.Fatalf("valid depth boundary rejected: %#v", got)
				}
			})
		}
	}
}
