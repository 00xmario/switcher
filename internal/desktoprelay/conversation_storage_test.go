package desktoprelay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestConversationStateRejectsInvalidAssociationsBindingsAndEvidence(t *testing.T) {
	for _, invalid := range []string{"local identity", "invalid identity", "binding key", "binding scope", "binding revision", "member account mismatch", "group missing members", "evidence route", "evidence status", "evidence account", "evidence time", "record bound", "byte bound"} {
		t.Run(invalid, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages", `{"model":"fixture-model"}`, sessionA, nil))
			if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
				t.Fatal(err)
			}
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cfg.DataRoot, "state.json")
			bytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var disk map[string]any
			if err := json.Unmarshal(bytes, &disk); err != nil {
				t.Fatal(err)
			}
			session := disk["sessions"].(map[string]any)[scope.ID+"/"+sessionA].(map[string]any)
			bindings := disk["conversation_bindings"].(map[string]any)
			binding := bindings[scope.ID+"/"+conversationA].(map[string]any)
			evidence := session["last_response"].(map[string]any)
			switch invalid {
			case "local identity":
				session["conversation_id"] = "local_" + conversationA
			case "invalid identity":
				session["conversation_id"] = strings.Repeat("x", 1024)
			case "binding key":
				delete(bindings, scope.ID+"/"+conversationA)
				bindings["wrong"] = binding
			case "binding scope":
				binding["scope_id"] = sessionC
			case "binding revision":
				binding["revision"] = 0
			case "member account mismatch":
				session["account_id"] = "A"
			case "group missing members":
				delete(bindings, scope.ID+"/"+conversationA)
				binding["conversation_id"] = conversationB
				bindings[scope.ID+"/"+conversationB] = binding
			case "evidence route":
				evidence["route"] = "billing_verified"
			case "evidence status":
				evidence["status"] = 0
			case "evidence account":
				evidence["account_id"] = "B"
			case "evidence time":
				evidence["at"] = "0001-01-01T00:00:00Z"
			case "record bound":
				for i := 1; i <= 4096; i++ {
					id := fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i)
					bindings[scope.ID+"/"+id] = map[string]any{"scope_id": scope.ID, "conversation_id": id, "account_id": "B", "revision": 1}
				}
			}
			bytes, err = json.Marshal(disk)
			if err != nil {
				t.Fatal(err)
			}
			if invalid == "byte bound" {
				bytes = append(bytes, []byte(strings.Repeat(" ", 4<<20))...)
			}
			if err := os.WriteFile(path, bytes, 0600); err != nil {
				t.Fatal(err)
			}
			other, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(context.Background())
			if err := other.Resume(context.Background()); !errors.Is(err, desktoprelay.ErrUnavailable) {
				t.Fatalf("invalid %s resumed: %v", invalid, err)
			}
			if other.Status().Listening {
				t.Fatal("corrupt grouped state admitted requests")
			}
		})
	}
}

func TestConversationStateBoundsCombinedAliasAndBindingRecords(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.DataRoot, "state.json")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err := json.Unmarshal(bytes, &disk); err != nil {
		t.Fatal(err)
	}
	sessions := disk["sessions"].(map[string]any)
	bindings := disk["conversation_bindings"].(map[string]any)
	for i := 1; i <= 2047; i++ {
		alias := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		conversation := fmt.Sprintf("%08x-aaaa-4aaa-8aaa-aaaaaaaaaaaa", i)
		sessions[scope.ID+"/"+alias] = map[string]any{"scope_id": scope.ID, "session_id": alias, "conversation_id": conversation, "account_id": "B", "revision": 1}
		bindings[scope.ID+"/"+conversation] = map[string]any{"scope_id": scope.ID, "conversation_id": conversation, "account_id": "B", "revision": 1}
	}
	sessions[scope.ID+"/"+sessionB] = map[string]any{"scope_id": scope.ID, "session_id": sessionB, "conversation_id": conversationA, "account_id": "B", "revision": 1}
	bytes, err = json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes) > 4<<20 {
		t.Fatal("record limit fixture unexpectedly exceeds byte limit")
	}
	if err := os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err := other.Resume(context.Background()); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatalf("4097 alias/group records admitted: %v", err)
	}
}
