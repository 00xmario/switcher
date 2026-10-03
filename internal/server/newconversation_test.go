package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
	"switcher/internal/server"
	"switcher/internal/sessionmeta"
)

const (
	relayConversationID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	relayForeignID      = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	relayFutureAlias    = "33333333-3333-4333-8333-333333333333"
	relayUnknownAlias   = "44444444-4444-4444-8444-444444444444"
	relayForeignAlias   = "55555555-5555-4555-8555-555555555555"
	relayCLIAlias       = "66666666-6666-4666-8666-666666666666"
	relayResetAlias     = "77777777-7777-4777-8777-777777777777"
)

type relayConversationView struct {
	Sessions []struct {
		desktoprelay.Session
		Title               string `json:"title"`
		ClientKind          string `json:"client_kind"`
		AssociationVerified bool   `json:"association_verified"`
		AssociationConflict bool   `json:"association_conflict"`
	} `json:"sessions"`
	Bindings      []desktoprelay.ConversationBinding `json:"conversation_bindings"`
	Conversations []struct {
		desktoprelay.ConversationBinding
		SessionIDs          []string          `json:"session_ids"`
		MemberRevisions     map[string]uint64 `json:"member_revisions"`
		AssociationVerified bool              `json:"association_verified"`
	} `json:"conversations"`
}

func conversationView(t *testing.T, f *relayE2EFixture) relayConversationView {
	t.Helper()
	var view relayConversationView
	raw := f.controlRequest(t, "GET", "/api/desktop-relay", "", 200)
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Bindings == nil || view.Conversations == nil {
		t.Fatal("conversation arrays must be present, including when empty")
	}
	var fields struct {
		Sessions      []map[string]json.RawMessage `json:"sessions"`
		Conversations []map[string]json.RawMessage `json:"conversations"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, objects := range [][]map[string]json.RawMessage{fields.Sessions, fields.Conversations} {
		for _, object := range objects {
			if value := string(object["association_verified"]); value != "true" && value != "false" {
				t.Fatal("GET omitted the explicit association verification flag")
			}
		}
	}
	return view
}

func conversationAccountPath(scope, conversation string) string {
	return "/api/desktop-relay/scopes/" + scope + "/conversations/" + conversation + "/account"
}

// Fixture file mutations do not invalidate the production index. Positive and
// negative proof snapshots remain valid for three seconds; wait once per mutation
// before asserting the next snapshot, without bypassing the shared resolver.
func waitConversationMetadataExpiry() {
	time.Sleep(3100 * time.Millisecond)
}

func conversationBody(t *testing.T, account string, revision uint64, members map[string]uint64) string {
	t.Helper()
	body := map[string]any{"revision": revision, "member_revisions": members}
	if account != "" {
		body["account_id"] = account
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func conversationReply(t *testing.T, raw []byte, scope, account string, revision uint64) desktoprelay.ConversationBinding {
	t.Helper()
	var reply struct {
		Binding desktoprelay.ConversationBinding `json:"conversation_binding"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Binding.ScopeID != scope || reply.Binding.ConversationID != relayConversationID || reply.Binding.AccountID != account || reply.Binding.Revision != revision {
		t.Fatalf("group selection acknowledgement = %s, %v", raw, err)
	}
	return reply.Binding
}

func fixtureConversationIndex(t *testing.T) *sessionmeta.Index {
	t.Helper()
	index, _ := fixtureConversationMetadata(t)
	return index
}

func fixtureConversationMetadata(t *testing.T) (*sessionmeta.Index, sessionmeta.Config) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	write := func(relative, raw string) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for j, id := range []string{relayE2ESession, relayE2EPeer, relayFutureAlias, relayResetAlias, relayForeignAlias} {
		conversation := relayConversationID
		bridge := "fixture-shared-bridge"
		if id == relayForeignAlias {
			conversation = relayForeignID
			bridge = "fixture-other-bridge"
		}
		write(fmt.Sprintf("desktop/account-%d/workspace/local_%s.json", j, conversation),
			fmt.Sprintf(`{"sessionId":"local_%s","cliSessionId":%q,"createdAt":1788432998079,"cwd":"/fixture/private/switcher","title":"Same saved title","bridgeSessionIds":[%q],"messages":["fixture-private-transcript"],"token":"fixture-private-metadata-token"}`, conversation, id, bridge))
	}
	write("projects/project/sessions-index.json", `{"entries":[{"sessionId":"`+relayCLIAlias+`","customTitle":"Same saved title","projectPath":"/fixture/private/switcher"}]}`)
	cfg := sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects}
	return sessionmeta.New(cfg), cfg
}

func TestDesktopRelayIntegrationWholeConversationRequiresExplicitSelection(t *testing.T) {
	f := newRelayE2EFixture(t, fixtureConversationIndex(t))
	f.secrets = append(f.secrets, "fixture-private-transcript", "fixture-private-metadata-token", "/fixture/private", "fixture-shared-bridge")
	own, peer := f.startScope(t), f.startScope(t)
	client, peerClient := f.desktopClient(t, own), f.desktopClient(t, peer)
	requests := make(map[string]uint64)
	message := func(client *http.Client, scope, id, model, bearer string, selected bool) desktoprelay.Session {
		t.Helper()
		body := bytes.ReplaceAll(relayE2EPayload(id, true), []byte("fixture-sonnet"), []byte(model))
		if id == relayUnknownAlias {
			body = append([]byte(`{"conversation_id":"`+relayConversationID+`","title":"Same saved title","account_id":"beta",`), body[bytes.IndexByte(body, '{')+1:]...)
		}
		name := "fixture-" + id + "-" + model
		response, _ := relayE2ERequest(t, client, id, name, body)
		relayE2EDrain(t, response, 200, relayE2ESSE)
		captures := f.upstream.snapshot()
		relayE2ECheckCapture(t, captures[len(captures)-1], id, name, bearer, body, selected)
		key := scope + "/" + id
		requests[key]++
		return f.session(t, scope, id, requests[key], 0)
	}
	root := message(client, own.ID, relayE2ESession, "claude-opus-4-6", relayE2ECallerA, false)
	alias := message(client, own.ID, relayE2EPeer, "claude-haiku-4-5", relayE2ECallerA, false)
	message(client, own.ID, relayForeignAlias, "claude-haiku-4-5", relayE2ECallerA, false)
	message(client, own.ID, relayCLIAlias, "claude-haiku-4-5", relayE2ECallerA, false)
	message(peerClient, peer.ID, relayE2ESession, "claude-opus-4-6", relayE2ECallerA, false)
	f.bind(t, own.ID, relayE2ESession, "beta", root.Revision)

	beforeSessions := f.manager.Sessions()
	statePath := filepath.Join(f.cfg.DataRoot, "state.json")
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	view := conversationView(t, f)
	if len(view.Bindings) != 0 || len(view.Conversations) != 3 || len(view.Sessions) != 5 {
		t.Fatalf("verified scope-separated groups = %+v", view.Conversations)
	}
	var members map[string]uint64
	for _, group := range view.Conversations {
		if group.ScopeID == own.ID && group.ConversationID == relayConversationID {
			if !reflect.DeepEqual(group.SessionIDs, []string{relayE2ESession, relayE2EPeer}) || group.Revision != 0 || group.AccountID != "" || !group.AssociationVerified {
				t.Fatal("GET promoted the legacy override or omitted a verified alias")
			}
			members = group.MemberRevisions
		}
	}
	if len(members) != 2 || members[relayE2EPeer] != alias.Revision {
		t.Fatal("group DTO did not expose every alias CAS revision")
	}
	for _, session := range view.Sessions {
		if session.Title != "Same saved title" {
			t.Fatal("synthetic metadata did not enrich the authorized view")
		}
		switch session.SessionID {
		case relayCLIAlias:
			if session.ConversationID != "" || session.ClientKind != "cli" || session.AssociationVerified {
				t.Fatal("CLI title created Desktop membership")
			}
		case relayForeignAlias:
			if session.ConversationID != relayForeignID || session.AccountID != "" || !session.AssociationVerified {
				t.Fatal("equal title merged an unrelated Desktop identity")
			}
		case relayE2EPeer:
			if session.ConversationID != relayConversationID || session.AccountID != "" || !session.AssociationVerified {
				t.Fatal("GET copied the root selection to the Haiku alias")
			}
		}
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(beforeState, afterState) || !reflect.DeepEqual(beforeSessions, f.manager.Sessions()) {
		t.Fatal("GET mutated relay sessions or durable selection")
	}
	raw := f.controlRequest(t, "POST", conversationAccountPath(own.ID, relayConversationID), conversationBody(t, "beta", 0, members), 200)
	conversationReply(t, raw, own.ID, "beta", 1)
	for _, turn := range []struct{ id, model string }{{relayE2ESession, "claude-opus-4-6"}, {relayE2EPeer, "claude-haiku-4-5"}, {relayFutureAlias, "claude-haiku-4-5"}} {
		session := message(client, own.ID, turn.id, turn.model, relayE2ESavedB, true)
		proof := session.LastResponse
		if session.ConversationID != relayConversationID || session.AccountID != "beta" || proof == nil || proof.Route != "selected" || proof.AccountID != "beta" || proof.Model != turn.model || proof.Status != 200 || proof.At.IsZero() {
			t.Fatalf("Messages response evidence for %s = %+v", turn.id, proof)
		}
	}
	message(client, own.ID, relayForeignAlias, "claude-haiku-4-5", relayE2ECallerA, false)
	message(client, own.ID, relayCLIAlias, "claude-haiku-4-5", relayE2ECallerA, false)
	unknown := message(client, own.ID, relayUnknownAlias, "claude-haiku-4-5", relayE2ECallerA, false)
	if unknown.ConversationID != "" || unknown.AccountID != "" || unknown.LastResponse.Route != "caller" {
		t.Fatal("arbitrary request fields established conversation membership")
	}
	message(peerClient, peer.ID, relayE2ESession, "claude-opus-4-6", relayE2ECallerA, false)
	f.assertGlobalSelection(t)

	// Usage stays caller-authorized. Count-tokens may use B but cannot replace
	// the last actual Messages response or claim another inference turn.
	proof := f.session(t, own.ID, relayE2EPeer, requests[own.ID+"/"+relayE2EPeer], 0).LastResponse
	for _, control := range []struct{ method, path, body, bearer string }{
		{"GET", "/api/oauth/usage", "", relayE2ECallerA},
		{"POST", "/v1/messages/count_tokens", `{"model":"fixture-count-model"}`, relayE2ESavedB},
	} {
		r, err := http.NewRequest(control.method, "https://api.anthropic.com"+control.path, strings.NewReader(control.body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+relayE2ECallerA)
		r.Header.Set("X-Claude-Code-Session-Id", relayE2EPeer)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		relayE2EDrain(t, response, 200, relayE2ESSE)
		captures := f.upstream.snapshot()
		capture := captures[len(captures)-1]
		if capture.URL != r.URL.String() || capture.Method != control.method || capture.Header.Get("Authorization") != "Bearer "+control.bearer || string(capture.Body) != control.body {
			t.Fatal("control-plane request changed its caller/selected credential policy")
		}
		if control.method == "POST" {
			requests[own.ID+"/"+relayE2EPeer]++
		}
	}
	for _, session := range conversationView(t, f).Sessions {
		if session.ScopeID == own.ID && session.SessionID == relayE2EPeer && !reflect.DeepEqual(session.LastResponse, proof) {
			t.Fatal("usage or token counting replaced actual Messages evidence")
		}
	}
	view = conversationView(t, f)
	for _, group := range view.Conversations {
		if group.ScopeID == own.ID && group.ConversationID == relayConversationID {
			if len(group.SessionIDs) != 3 || group.AccountID != "beta" || group.Revision != 1 {
				t.Fatal("future verified alias did not join the durable selection")
			}
			raw = f.controlRequest(t, "DELETE", conversationAccountPath(own.ID, relayConversationID), conversationBody(t, "", group.Revision, group.MemberRevisions), 200)
			conversationReply(t, raw, own.ID, "", 2)
		}
	}
	for _, id := range []string{relayE2ESession, relayE2EPeer, relayFutureAlias, relayResetAlias} {
		if session := message(client, own.ID, id, "claude-haiku-4-5", relayE2ECallerA, false); session.AccountID != "" {
			t.Fatal("group reset retained an override or selected a future alias")
		}
	}
	f.assertGlobalSelection(t)
}

type relayFixtureResolver struct {
	mu      sync.RWMutex
	aliases map[string]string
	err     error
}

func (r *relayFixtureResolver) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string)
	for _, id := range ids {
		if conversation := r.aliases[id]; conversation != "" {
			out[id] = conversation
		}
	}
	return out, r.err
}

func TestDesktopRelayIntegrationSavedOwnershipSurvivesMissingMetadata(t *testing.T) {
	resolver := &relayFixtureResolver{aliases: map[string]string{relayE2ESession: relayConversationID, relayE2EPeer: relayConversationID}}
	f := newRelayE2EFixture(t, resolver)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	members := make(map[string]uint64)
	for _, id := range []string{relayE2ESession, relayE2EPeer} {
		response, _ := relayE2ERequest(t, client, id, "fixture-observe-owned", relayE2EPayload(id, false))
		relayE2EDrain(t, response, 200, relayE2ESSE)
		members[id] = f.session(t, scope.ID, id, 1, 0).Revision
	}
	response, _ := relayE2ERequest(t, client, relayUnknownAlias, "fixture-unmapped-caller", []byte(`{"model":"fixture-sonnet","conversation_id":"`+relayConversationID+`","title":"Same saved title","messages":[]}`))
	relayE2EDrain(t, response, 200, relayE2ESSE)
	f.session(t, scope.ID, relayUnknownAlias, 1, 0)
	path := conversationAccountPath(scope.ID, relayConversationID)
	conversationReply(t, f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 200), scope.ID, "beta", 1)
	resolver.mu.Lock()
	resolver.err = errors.New(relayE2ESourceBody)
	resolver.mu.Unlock()
	view := conversationView(t, f)
	if len(view.Conversations) != 1 || len(view.Conversations[0].SessionIDs) != 2 {
		t.Fatal("GET dropped saved group ownership when metadata was unavailable")
	}
	group := view.Conversations[0]
	if group.ConversationID != relayConversationID || group.AccountID != "beta" || group.Revision != 1 || group.AssociationVerified {
		t.Fatalf("saved ownership view = %+v", group)
	}
	for _, session := range view.Sessions {
		if session.SessionID == relayUnknownAlias {
			if session.ConversationID != "" || session.AssociationVerified {
				t.Fatal("caller conversation fields promoted an unknown alias into saved ownership")
			}
			continue
		}
		if session.ConversationID != relayConversationID || session.AssociationVerified || session.Title != "" || group.MemberRevisions[session.SessionID] != session.Revision {
			t.Fatal("saved member is missing from reset CAS or marked currently verified")
		}
	}
	resolver.mu.Lock()
	resolver.err = errors.New(relayE2ESourceBody)
	resolver.mu.Unlock()
	f.controlRequest(t, "POST", path, conversationBody(t, "alpha", group.Revision, group.MemberRevisions), 503)
	f.controlRequest(t, "POST", "/api/desktop-relay/stop", "", 200)
	f.source.mu.Lock()
	prepared, refreshed := len(f.source.prepared), len(f.source.refreshed)
	f.source.mu.Unlock()
	f.controlRequest(t, "DELETE", path, conversationBody(t, "", 0, group.MemberRevisions), 409)
	f.controlRequest(t, "DELETE", path, conversationBody(t, "", group.Revision, map[string]uint64{relayE2ESession: group.MemberRevisions[relayE2ESession]}), 409)
	conversationReply(t, f.controlRequest(t, "DELETE", path, conversationBody(t, "", group.Revision, group.MemberRevisions), 200), scope.ID, "", 2)
	cleared := conversationView(t, f)
	if len(cleared.Conversations) != 1 || cleared.Conversations[0].Revision != 2 || cleared.Conversations[0].AccountID != "" || cleared.Conversations[0].AssociationVerified {
		t.Fatal("reset lost the empty-account ownership revision record")
	}
	for _, session := range cleared.Sessions {
		if session.AccountID != "" {
			t.Fatal("stopped group reset retained a member override")
		}
	}
	f.source.mu.Lock()
	if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
		t.Error("stopped reset accessed saved credentials")
	}
	f.source.mu.Unlock()
	if len(f.upstream.snapshot()) != 3 || f.manager.Status().Enabled || f.manager.Status().Listening {
		t.Fatal("stopped reset dispatched a request or restarted the relay")
	}
	// An empty group record still owns all saved aliases. A later exact override
	// must stay in the reset CAS even when current metadata then disappears.
	resolver.mu.Lock()
	resolver.err = nil
	resolver.mu.Unlock()
	f.controlRequest(t, "POST", "/api/desktop-relay/start", "", 200)
	resetGroup := cleared.Conversations[0]
	f.bind(t, scope.ID, relayE2ESession, "beta", resetGroup.MemberRevisions[relayE2ESession])
	resolver.mu.Lock()
	resolver.err = errors.New(relayE2ESourceBody)
	resolver.mu.Unlock()
	f.controlRequest(t, "POST", "/api/desktop-relay/stop", "", 200)
	view = conversationView(t, f)
	if len(view.Conversations) != 1 || len(view.Conversations[0].MemberRevisions) != 2 || view.Conversations[0].AccountID != "" {
		t.Fatal("empty ownership record lost a later exact member override")
	}
	group = view.Conversations[0]
	conversationReply(t, f.controlRequest(t, "DELETE", path, conversationBody(t, "", group.Revision, group.MemberRevisions), 200), scope.ID, "", 3)
	for _, session := range conversationView(t, f).Sessions {
		if session.AccountID != "" {
			t.Fatal("reset of an empty group record retained its member's exact override")
		}
	}
	f.assertGlobalSelection(t)
}

func TestDesktopRelayIntegrationMetadataChangesPreserveSavedControlGroup(t *testing.T) {
	for _, change := range []string{"root_removed", "record_conflict", "remapped_source", "missing_title", "ambiguous_title"} {
		t.Run(change, func(t *testing.T) {
			index, roots := fixtureConversationMetadata(t)
			writeRecord := func(account, conversation, id, title string, created int64) {
				t.Helper()
				path := filepath.Join(roots.DesktopRoot, account, "workspace", "local_"+conversation+".json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				raw := fmt.Sprintf(`{"sessionId":"local_%s","cliSessionId":%q,"createdAt":%d,"cwd":"/fixture/private/switcher","bridgeSessionIds":["fixture-shared-bridge"]%s}`, conversation, id, created, title)
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// This title-only record can decorate an unknown caller, but it must
			// never acquire saved group ownership or verified native membership.
			unknownPath := filepath.Join(roots.DesktopRoot, "account-unknown", "workspace", "local_title-only.json")
			if err := os.MkdirAll(filepath.Dir(unknownPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(unknownPath, []byte(`{"cliSessionId":"`+relayUnknownAlias+`","title":"Same saved title"}`), 0600); err != nil {
				t.Fatal(err)
			}
			f := newRelayE2EFixture(t, index)
			scope := f.startScope(t)
			client := f.desktopClient(t, scope)
			members := make(map[string]uint64)
			for _, id := range []string{relayE2ESession, relayE2EPeer, relayUnknownAlias} {
				response, _ := relayE2ERequest(t, client, id, "fixture-observe-metadata-change", relayE2EPayload(id, false))
				relayE2EDrain(t, response, 200, relayE2ESSE)
				session := f.session(t, scope.ID, id, 1, 0)
				if id != relayUnknownAlias {
					members[id] = session.Revision
				}
			}
			path := conversationAccountPath(scope.ID, relayConversationID)
			conversationReply(t, f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 200), scope.ID, "beta", 1)
			switch change {
			case "root_removed":
				if err := os.RemoveAll(roots.DesktopRoot); err != nil {
					t.Fatal(err)
				}
			case "record_conflict":
				writeRecord("account-conflict", relayConversationID, relayE2ESession, `,"title":"Conflicting saved title"`, 1788432998080)
			case "remapped_source":
				if err := os.RemoveAll(roots.DesktopRoot); err != nil {
					t.Fatal(err)
				}
				writeRecord("account-new-root", relayForeignID, relayE2ESession, "", 1788432998079)
				writeRecord("account-new-alias", relayForeignID, relayE2EPeer, "", 1788432998079)
			case "missing_title":
				writeRecord("account-0", relayConversationID, relayE2ESession, "", 1788432998079)
				writeRecord("account-1", relayConversationID, relayE2EPeer, "", 1788432998079)
			case "ambiguous_title":
				writeRecord("account-title-copy", relayConversationID, relayE2ESession, `,"title":"Conflicting saved title"`, 1788432998079)
			}
			waitConversationMetadataExpiry()
			if _, err := index.Resolve(context.Background(), []string{relayUnknownAlias}); err != nil {
				t.Fatal(err)
			}
			before := f.manager.Sessions()
			statePath := filepath.Join(f.cfg.DataRoot, "state.json")
			state, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			f.source.mu.Lock()
			prepared, refreshed := len(f.source.prepared), len(f.source.refreshed)
			f.source.mu.Unlock()
			view := conversationView(t, f)
			verified := change == "missing_title" || change == "ambiguous_title"
			if len(view.Conversations) != 1 || view.Conversations[0].ConversationID != relayConversationID || view.Conversations[0].AssociationVerified != verified {
				t.Fatalf("metadata change dropped or forked the recorded group: %+v", view.Conversations)
			}
			group := view.Conversations[0]
			if len(group.MemberRevisions) != 2 || group.Revision != 1 || group.AccountID != "beta" {
				t.Fatal("metadata change lost the saved group reset CAS")
			}
			for _, session := range view.Sessions {
				if session.SessionID == relayUnknownAlias {
					if session.ConversationID != "" || session.AssociationVerified {
						t.Fatal("a matching title promoted an unknown caller into the saved group")
					}
					continue
				}
				if session.ConversationID != relayConversationID || session.AssociationVerified != verified || session.AssociationConflict != (change == "remapped_source") || group.MemberRevisions[session.SessionID] != session.Revision {
					t.Fatal("member view did not separate saved ownership from current verification")
				}
				if session.SessionID == relayE2ESession && session.Title != "" {
					t.Fatal("missing or ambiguous display metadata retained a title")
				}
				if session.LastResponse == nil || session.LastResponse.Route != "caller" || session.LastResponse.Model != "fixture-sonnet" || session.LastResponse.Status != 200 {
					t.Fatal("metadata change lost the actual captured response evidence")
				}
			}
			after, err := os.ReadFile(statePath)
			if err != nil || !bytes.Equal(state, after) || !reflect.DeepEqual(before, f.manager.Sessions()) {
				t.Fatal("GET changed persisted ownership while decorating verification")
			}
			f.source.mu.Lock()
			if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
				t.Error("GET accessed saved credentials")
			}
			f.source.mu.Unlock()
			if !verified {
				f.controlRequest(t, "POST", path, conversationBody(t, "alpha", group.Revision, group.MemberRevisions), 409)
				if change == "remapped_source" {
					f.controlRequest(t, "POST", conversationAccountPath(scope.ID, relayForeignID), conversationBody(t, "alpha", 0, group.MemberRevisions), 409)
				}
			}
			f.controlRequest(t, "POST", "/api/desktop-relay/stop", "", 200)
			conversationReply(t, f.controlRequest(t, "DELETE", path, conversationBody(t, "", group.Revision, group.MemberRevisions), 200), scope.ID, "", 2)
			for _, session := range conversationView(t, f).Sessions {
				if session.AccountID != "" {
					t.Fatal("safe reset did not clear the recorded member overrides")
				}
			}
			f.source.mu.Lock()
			if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
				t.Error("stopped reset or refused rebind accessed saved credentials")
			}
			f.source.mu.Unlock()
			if len(f.upstream.snapshot()) != 3 || f.manager.Status().Enabled || f.manager.Status().Listening {
				t.Fatal("ownership-only controls dispatched inference or enabled the relay")
			}
			f.assertGlobalSelection(t)
		})
	}
}

func TestDesktopRelayIntegrationResetViewIncludesNewlyVerifiedObservedAlias(t *testing.T) {
	index, roots := fixtureConversationMetadata(t)
	futurePath := filepath.Join(roots.DesktopRoot, "account-2", "workspace", "local_"+relayConversationID+".json")
	futureRecord, err := os.ReadFile(futurePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(futurePath); err != nil {
		t.Fatal(err)
	}
	f := newRelayE2EFixture(t, index)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	members := make(map[string]uint64)
	for _, id := range []string{relayE2ESession, relayE2EPeer, relayFutureAlias} {
		response, _ := relayE2ERequest(t, client, id, "fixture-observe-before-proof", relayE2EPayload(id, false))
		relayE2EDrain(t, response, 200, relayE2ESSE)
		session := f.session(t, scope.ID, id, 1, 0)
		if id == relayFutureAlias {
			if session.ConversationID != "" || session.AccountID != "" {
				t.Fatal("missing metadata promoted an unverified alias")
			}
		} else {
			members[id] = session.Revision
		}
	}
	path := conversationAccountPath(scope.ID, relayConversationID)
	conversationReply(t, f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 200), scope.ID, "beta", 1)
	if err := os.WriteFile(futurePath, futureRecord, 0600); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(roots.DesktopRoot, "account-0", "workspace", "local_"+relayConversationID+".json")
	if err := os.Remove(rootPath); err != nil {
		t.Fatal(err)
	}
	// A current unproved claim for the retired UUID forbids historical fallback.
	// The other aliases still have strict proof, so reset must cover that union.
	claimPath := filepath.Join(filepath.Dir(rootPath), "local_unverified.json")
	if err := os.WriteFile(claimPath, []byte(`{"cliSessionId":"`+relayE2ESession+`","title":"Unproved rotated alias"}`), 0600); err != nil {
		t.Fatal(err)
	}
	waitConversationMetadataExpiry()
	if _, err := index.Resolve(context.Background(), []string{relayUnknownAlias}); err != nil {
		t.Fatal(err)
	}
	before := f.manager.Sessions()
	view := conversationView(t, f)
	if len(view.Conversations) != 1 || len(view.Conversations[0].MemberRevisions) != 3 || view.Conversations[0].AssociationVerified {
		t.Fatal("reset view omitted saved ownership or a newly verified observed alias")
	}
	group := view.Conversations[0]
	for _, session := range view.Sessions {
		if session.ConversationID != relayConversationID || session.AssociationVerified != (session.SessionID != relayE2ESession) || group.MemberRevisions[session.SessionID] != session.Revision {
			t.Fatal("mixed-proof group did not preserve complete alias revisions")
		}
	}
	if !reflect.DeepEqual(before, f.manager.Sessions()) {
		t.Fatal("GET adopted the newly verified alias into persisted membership")
	}
	f.controlRequest(t, "POST", path, conversationBody(t, "alpha", group.Revision, group.MemberRevisions), 409)
	f.source.mu.Lock()
	prepared, refreshed := len(f.source.prepared), len(f.source.refreshed)
	f.source.mu.Unlock()
	conversationReply(t, f.controlRequest(t, "DELETE", path, conversationBody(t, "", group.Revision, group.MemberRevisions), 200), scope.ID, "", 2)
	for _, session := range conversationView(t, f).Sessions {
		if session.AccountID != "" {
			t.Fatal("mixed-proof reset left a saved override")
		}
	}
	f.source.mu.Lock()
	if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
		t.Error("reset prepared or refreshed saved credentials")
	}
	f.source.mu.Unlock()
	if len(f.upstream.snapshot()) != 3 {
		t.Fatal("reset dispatched another request")
	}
	f.assertGlobalSelection(t)
}

func TestDesktopRelayIntegrationRotatedKnownAliasKeepsGroupedAccountControls(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := sessionmeta.Config{DesktopRoot: filepath.Join(root, "desktop")}
	path := filepath.Join(roots.DesktopRoot, "account", "workspace", "local_"+relayConversationID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(alias string) {
		t.Helper()
		raw := fmt.Sprintf(`{"sessionId":"local_%s","cliSessionId":%q,"createdAt":1788432998079,"cwd":"/fixture/private/switcher","title":"Same saved title","bridgeSessionIds":["fixture-rotation-bridge"]}`, relayConversationID, alias)
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(relayE2ESession)
	index := sessionmeta.New(roots)
	f := newRelayE2EFixture(t, index)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	for _, id := range []string{relayE2ESession, relayUnknownAlias} {
		response, _ := relayE2ERequest(t, client, id, "fixture-before-rotation", relayE2EPayload(id, false))
		relayE2EDrain(t, response, 200, relayE2ESSE)
		f.session(t, scope.ID, id, 1, 0)
	}
	observed := f.session(t, scope.ID, relayE2ESession, 1, 0)
	accountPath := conversationAccountPath(scope.ID, relayConversationID)
	conversationReply(t, f.controlRequest(t, "POST", accountPath, conversationBody(t, "beta", 0, map[string]uint64{relayE2ESession: observed.Revision}), 200), scope.ID, "beta", 1)
	write(relayE2EPeer)
	waitConversationMetadataExpiry()
	if current, err := index.Resolve(context.Background(), []string{relayE2EPeer}); err != nil || current[relayE2EPeer] != relayConversationID {
		t.Fatalf("rotated fixture lacks strict current proof: %v, %v", current, err)
	}
	if info := index.Lookup(context.Background(), []string{relayE2ESession}); len(info) != 0 {
		t.Fatal("retired alias still has current display metadata")
	}
	before := f.manager.Sessions()
	bindings := f.manager.ConversationBindings()
	statePath := filepath.Join(f.cfg.DataRoot, "state.json")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	f.source.mu.Lock()
	prepared, refreshed := len(f.source.prepared), len(f.source.refreshed)
	f.source.mu.Unlock()
	view := conversationView(t, f)
	if len(view.Conversations) != 1 || !view.Conversations[0].AssociationVerified || view.Conversations[0].ConversationID != relayConversationID {
		t.Fatal("GET disabled grouped account changes for a source-verified retired alias")
	}
	for _, session := range view.Sessions {
		if session.SessionID == relayE2ESession {
			if session.ConversationID != relayConversationID || !session.AssociationVerified || session.AssociationConflict || session.Title != "" {
				t.Fatal("historical proof did not verify the persisted alias independently of its missing title")
			}
		} else if session.ConversationID != "" || session.AssociationVerified || session.AccountID != "" {
			t.Fatal("rotation retroactively joined an unproved legacy alias")
		}
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(state, after) || !reflect.DeepEqual(before, f.manager.Sessions()) || !reflect.DeepEqual(bindings, f.manager.ConversationBindings()) {
		t.Fatal("historical verification GET adopted or saved membership")
	}
	f.source.mu.Lock()
	if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
		t.Error("historical verification GET acquired credentials")
	}
	f.source.mu.Unlock()
	body := relayE2EPayload(relayE2EPeer, true)
	response, _ := relayE2ERequest(t, client, relayE2EPeer, "fixture-current-alias-inherits-beta", body)
	relayE2EDrain(t, response, 200, relayE2ESSE)
	f.session(t, scope.ID, relayE2EPeer, 1, 0)
	captures := f.upstream.snapshot()
	relayE2ECheckCapture(t, captures[len(captures)-1], relayE2EPeer, "fixture-current-alias-inherits-beta", relayE2ESavedB, body, true)
	view = conversationView(t, f)
	if len(view.Conversations) != 1 || !view.Conversations[0].AssociationVerified || !reflect.DeepEqual(view.Conversations[0].SessionIDs, []string{relayE2ESession, relayE2EPeer}) {
		t.Fatal("current and historical aliases split into different control cards")
	}
	group := view.Conversations[0]
	if len(group.MemberRevisions) != 2 {
		t.Fatal("grouped selection omitted a historical or current alias revision")
	}
	conversationReply(t, f.controlRequest(t, "POST", accountPath, conversationBody(t, "alpha", group.Revision, group.MemberRevisions), 200), scope.ID, "alpha", 2)
	for _, session := range conversationView(t, f).Sessions {
		if session.SessionID != relayUnknownAlias && session.AccountID != "alpha" {
			t.Fatal("grouped account change failed to update a verified rotated alias")
		}
	}
	claimPath := filepath.Join(filepath.Dir(path), "local_"+relayForeignID+".json")
	claim := fmt.Sprintf(`{"sessionId":"local_%s","cliSessionId":%q,"createdAt":1788432998079,"cwd":"/fixture/private/switcher","title":"Different current conversation"}`, relayForeignID, relayE2ESession)
	if err := os.WriteFile(claimPath, []byte(claim), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"contradictory_current_claim", "native_group_removed"} {
		if failure == "native_group_removed" {
			for _, remove := range []string{path, claimPath} {
				if err := os.Remove(remove); err != nil {
					t.Fatal(err)
				}
			}
		}
		waitConversationMetadataExpiry()
		if _, err := index.Resolve(context.Background(), []string{relayUnknownAlias}); err != nil {
			t.Fatal(err)
		}
		before = f.manager.Sessions()
		bindings = f.manager.ConversationBindings()
		state, err = os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		f.source.mu.Lock()
		prepared, refreshed = len(f.source.prepared), len(f.source.refreshed)
		f.source.mu.Unlock()
		view = conversationView(t, f)
		if len(view.Conversations) != 1 || view.Conversations[0].AssociationVerified || view.Conversations[0].ConversationID != relayConversationID || len(view.Conversations[0].MemberRevisions) != 2 {
			t.Fatalf("%s lost reset ownership or retained invalid historical proof", failure)
		}
		for _, session := range view.Sessions {
			if session.SessionID == relayE2ESession {
				if session.ConversationID != relayConversationID || session.AssociationVerified || session.AssociationConflict != (failure == "contradictory_current_claim") {
					t.Fatalf("%s silently remapped the recorded alias", failure)
				}
			}
			if failure == "native_group_removed" && (session.AssociationVerified || session.Title != "") {
				t.Fatal("missing native group retained current proof or display metadata")
			}
		}
		group = view.Conversations[0]
		f.controlRequest(t, "POST", accountPath, conversationBody(t, "beta", group.Revision, group.MemberRevisions), 409)
		after, err = os.ReadFile(statePath)
		if err != nil || !bytes.Equal(state, after) || !reflect.DeepEqual(before, f.manager.Sessions()) || !reflect.DeepEqual(bindings, f.manager.ConversationBindings()) {
			t.Fatal("degraded GET or refused rebind changed saved ownership")
		}
		f.source.mu.Lock()
		if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
			t.Error("degraded GET or refused rebind acquired credentials")
		}
		f.source.mu.Unlock()
	}
	if len(f.upstream.snapshot()) != 3 {
		t.Fatal("historical GET or account changes dispatched another request")
	}
	f.assertGlobalSelection(t)
}

func TestDesktopRelayIntegrationMissingSourceProofDoesNotReuseDisplayProof(t *testing.T) {
	resolver := &relayFixtureResolver{aliases: map[string]string{relayE2ESession: relayConversationID}}
	f := newRelayE2EFixture(t, resolver)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	response, _ := relayE2ERequest(t, client, relayE2ESession, "fixture-before-source-error", relayE2EPayload(relayE2ESession, false))
	relayE2EDrain(t, response, 200, relayE2ESSE)
	observed := f.session(t, scope.ID, relayE2ESession, 1, 0)
	conversationReply(t, f.controlRequest(t, "POST", conversationAccountPath(scope.ID, relayConversationID), conversationBody(t, "beta", 0, map[string]uint64{relayE2ESession: observed.Revision}), 200), scope.ID, "beta", 1)
	// Keep legitimate current display information while the manager's independent
	// source hook fails. This isolates error handling from absence of a title.
	f.main.Close()
	f.metadata = fixtureConversationIndex(t)
	f.serveManagement(t)
	before := f.manager.Sessions()
	f.source.mu.Lock()
	prepared, refreshed := len(f.source.prepared), len(f.source.refreshed)
	f.source.mu.Unlock()
	for _, failure := range []string{"source_error", "sparse_no_error"} {
		resolver.mu.Lock()
		resolver.err = nil
		if failure == "source_error" {
			resolver.err = errors.New(relayE2ESourceBody)
		} else {
			resolver.aliases = map[string]string{}
		}
		resolver.mu.Unlock()
		view := conversationView(t, f)
		if len(view.Conversations) != 1 || view.Conversations[0].AssociationVerified || len(view.Sessions) != 1 {
			t.Fatalf("%s dropped saved ownership or reused display proof", failure)
		}
		session := view.Sessions[0]
		if session.ConversationID != relayConversationID || session.AssociationVerified || session.Title != "Same saved title" || session.AccountID != "beta" {
			t.Fatalf("%s confused display metadata with source authority", failure)
		}
		if !reflect.DeepEqual(before, f.manager.Sessions()) {
			t.Fatal("absence of source verification mutated saved observations")
		}
	}
	f.source.mu.Lock()
	if len(f.source.prepared) != prepared || len(f.source.refreshed) != refreshed {
		t.Error("failed verification GET acquired credentials")
	}
	f.source.mu.Unlock()
}

func TestDesktopRelayIntegrationConversationCASAndSanitizedFailures(t *testing.T) {
	resolver := &relayFixtureResolver{aliases: map[string]string{relayE2ESession: relayConversationID, relayE2EPeer: relayConversationID, relayFutureAlias: relayConversationID, relayForeignAlias: relayForeignID}}
	f := newRelayE2EFixture(t, resolver)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	for _, id := range []string{relayE2ESession, relayE2EPeer, relayForeignAlias} {
		response, _ := relayE2ERequest(t, client, id, "fixture-observe-cas", relayE2EPayload(id, false))
		relayE2EDrain(t, response, 200, relayE2ESSE)
		f.session(t, scope.ID, id, 1, 0)
	}
	// The manager's current source proof remains authoritative even with display
	// lookup disabled. A saved observation alone would not suffice without it.
	view := conversationView(t, f)
	if len(view.Conversations) != 2 {
		t.Fatal("GET omitted source-verified associations when display lookup was nil")
	}
	for _, session := range view.Sessions {
		if session.ConversationID == "" || !session.AssociationVerified || session.Title != "" {
			t.Fatal("GET lost current source proof or fabricated display metadata")
		}
	}
	members := map[string]uint64{}
	// Caller observation uses cached proof only. GET resolves membership without
	// persisting it, so grouped CAS must use the management DTO, just like the UI.
	for _, group := range view.Conversations {
		if group.ScopeID == scope.ID && group.ConversationID == relayConversationID {
			members = group.MemberRevisions
		}
	}
	if len(members) != 2 || members[relayE2ESession] == 0 || members[relayE2EPeer] == 0 {
		t.Fatal("management view omitted verified members or their observed revisions")
	}
	path := conversationAccountPath(scope.ID, relayConversationID)
	before := f.manager.Sessions()
	statePath := filepath.Join(f.cfg.DataRoot, "state.json")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func() {
		t.Helper()
		after, err := os.ReadFile(statePath)
		if err != nil || !bytes.Equal(state, after) || !reflect.DeepEqual(before, f.manager.Sessions()) || len(f.manager.ConversationBindings()) != 0 {
			t.Fatal("failed grouped operation changed observed or durable selection")
		}
	}
	for _, tc := range []struct {
		conversation string
		revision     uint64
		members      map[string]uint64
		status       int
	}{
		{relayConversationID, 0, map[string]uint64{relayE2ESession: members[relayE2ESession]}, 409},
		{relayConversationID, 0, map[string]uint64{relayE2ESession: members[relayE2ESession], relayE2EPeer: members[relayE2EPeer] + 1}, 409},
		{relayConversationID, 0, map[string]uint64{relayE2ESession: members[relayE2ESession], relayForeignAlias: 1}, 409},
		{relayConversationID, 1, members, 409},
		{relayUnknownAlias, 0, members, 404},
	} {
		f.controlRequest(t, "POST", conversationAccountPath(scope.ID, tc.conversation), conversationBody(t, "beta", tc.revision, tc.members), tc.status)
		unchanged()
	}
	f.source.fail("beta", fmt.Errorf("%w: %s", desktoprelay.ErrCredentialBusy, relayE2ESourceBody))
	raw := f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 503)
	if !bytes.Contains(raw, []byte(`"error_code":"credential_busy"`)) {
		t.Fatal("grouped credential preparation lost its public classification")
	}
	unchanged()
	f.source.fail("beta", nil)
	resolver.mu.Lock()
	resolver.err = errors.New(relayE2ESourceBody)
	resolver.mu.Unlock()
	raw = f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 503)
	if !bytes.Contains(raw, []byte(`"error_code":"conversation_association_unavailable"`)) {
		t.Fatal("resolver error was not sanitized and classified")
	}
	unchanged()
	resolver.mu.Lock()
	resolver.err = nil
	resolver.mu.Unlock()
	mux := http.NewServeMux()
	(&server.API{Store: f.accounts, DesktopRelay: f.manager, ManagementKey: relayE2EControlKey}).Register(mux)
	r := httptest.NewRequest("POST", "http://127.0.0.1:9123"+path, strings.NewReader(conversationBody(t, "beta", 0, members)))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Switcher-Desktop-Control", relayE2EControlKey)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 503 {
		t.Fatalf("cancelled group operation: %d", w.Code)
	}
	f.assertPublic(t, w.Body.Bytes())
	unchanged()
	raw = f.controlRequest(t, "POST", path, conversationBody(t, "beta", 0, members), 200)
	conversationReply(t, raw, scope.ID, "beta", 1)
	f.controlRequest(t, "POST", path, conversationBody(t, "alpha", 0, members), 409)
	for _, id := range []string{relayE2ESession, relayE2EPeer, relayFutureAlias} {
		body := relayE2EPayload(id, true)
		response, _ := relayE2ERequest(t, client, id, "fixture-group-selected", body)
		relayE2EDrain(t, response, 200, relayE2ESSE)
		captures := f.upstream.snapshot()
		relayE2ECheckCapture(t, captures[len(captures)-1], id, "fixture-group-selected", relayE2ESavedB, body, true)
	}
	f.controlRequest(t, "DELETE", path, conversationBody(t, "", 1, members), 409)
	current := make(map[string]uint64)
	for _, session := range f.manager.Sessions() {
		if session.ScopeID == scope.ID && session.ConversationID == relayConversationID {
			if session.AccountID != "beta" {
				t.Fatal("failed stale reset changed a member override")
			}
			current[session.SessionID] = session.Revision
		}
	}
	raw = f.controlRequest(t, "DELETE", path, conversationBody(t, "", 1, current), 200)
	conversationReply(t, raw, scope.ID, "", 2)
	for _, session := range f.manager.Sessions() {
		if session.AccountID != "" {
			t.Fatal("atomic conversation reset left a member override")
		}
	}
	f.assertGlobalSelection(t)
}
