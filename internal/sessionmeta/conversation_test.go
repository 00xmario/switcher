package sessionmeta_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"switcher/internal/sessionmeta"
)

const (
	conversationID = "92ef147b-9a97-440c-a9c4-073a2b683cb3"
	otherID        = "11111111-1111-4111-8111-111111111111"
	requestA       = "f1992147-b857-4b8b-b1c9-e22b0ba542ac"
	requestB       = "57b82f5b-7be7-49de-82e4-63f18abc87c1"
	requestC       = "44444444-4444-4444-8444-444444444444"
)

func conversationRecord(t *testing.T, root, account, conversation, request, extra string) string {
	t.Helper()
	return saved(t, root, account+"/workspace/local_"+conversation+".json", `{"sessionId":"local_`+conversation+`","cliSessionId":"`+request+`","createdAt":1788432998079,"cwd":"/fixture/work/Salenza","title":"Saved title"`+extra+`}`)
}

func TestResolveMapsCLISessionToDesktopConversation(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "account-a", conversationID, requestA, "")
	// Forks and children are still conversations the user can select.
	conversationRecord(t, root, "account-b", conversationID, requestB, `,"parentSessionId":"x","isFork":true`)
	saved(t, root, "account-a/workspace/local_broken.json", `{not json`)
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got, err := index.Resolve(context.Background(), []string{requestA, strings.ToUpper(requestB), requestC})
	want := map[string]string{requestA: conversationID, strings.ToUpper(requestB): conversationID}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve = %#v, %v; want %#v", got, err, want)
	}
	if cached := index.ResolveCached([]string{requestA}); cached[requestA] != conversationID {
		t.Fatalf("ResolveCached = %#v", cached)
	}
	info := index.Lookup(context.Background(), []string{requestA})
	if info[requestA].Title != "Saved title" || info[requestA].ConversationID != conversationID {
		t.Fatalf("Lookup = %#v", info[requestA])
	}
}

func TestResolveSkipsSessionClaimedByTwoConversations(t *testing.T) {
	root := fixtureRoot(t)
	conversationRecord(t, root, "account-a", conversationID, requestA, "")
	conversationRecord(t, root, "account-b", otherID, requestA, "")
	conversationRecord(t, root, "account-a", otherID, requestB, "")
	index := sessionmeta.New(sessionmeta.Config{DesktopRoot: root})
	got, err := index.Resolve(context.Background(), []string{requestA, requestB})
	if err != nil || !reflect.DeepEqual(got, map[string]string{requestB: otherID}) {
		t.Fatalf("Resolve = %#v, %v", got, err)
	}
}
