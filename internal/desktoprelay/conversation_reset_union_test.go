package desktoprelay_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestConversationResetAcceptsSavedMembersAndNewlyVerifiedObservedAliases(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "running"
		if stopped {
			name = "stopped"
		}
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var phase, preparations atomic.Int32
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				out := make(map[string]string)
				for _, id := range ids {
					if id == sessionB || id == sessionA && phase.Load() == 0 || id == sessionC && phase.Load() == 1 {
						out[id] = conversationA
					}
				}
				return out, nil
			})
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				preparations.Add(1)
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			body := `{"model":"captured-model","title":"same title","conversation_id":"` + conversationA + `"}`
			for _, id := range []string{sessionA, sessionB, sessionC, sessionD} {
				drain(t, send(t, c, br, "/v1/messages", body, id, nil))
			}
			// An exact legacy override on an unverified alias must also be cleared
			// once fresh resolver proof and explicit reset consent include it.
			if _, err := m.Bind(context.Background(), scope.ID, sessionC, "A", observed(t, m, scope.ID, sessionC).Revision); err != nil {
				t.Fatal(err)
			}
			group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			expected := memberRevisions(m, scope.ID, conversationA)
			expected[sessionC] = observed(t, m, scope.ID, sessionC).Revision
			before := m.Sessions()
			phase.Store(1) // Saved A loses metadata; already-observed C gains proof.
			if stopped {
				if err := m.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", group.Revision, expected); !errors.Is(err, desktoprelay.ErrConflict) {
					t.Fatalf("mixed proof widened grouped selection: %v", err)
				}
			}
			if !reflect.DeepEqual(m.Sessions(), before) || observed(t, m, scope.ID, sessionC).ConversationID != "" {
				t.Fatal("read/preflight migrated the newly verified observation")
			}
			prepared := preparations.Load()
			status := m.Status()
			reset, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, group.Revision, expected)
			if err != nil || reset.AccountID != "" || reset.Revision != 2 {
				t.Fatalf("mixed-proof union reset: %+v %v", reset, err)
			}
			if preparations.Load() != prepared || m.Status().Enabled != status.Enabled || m.Status().Listening != status.Listening {
				t.Fatal("reset acquired credentials or changed listener enablement")
			}
			for _, prior := range before {
				current := observed(t, m, scope.ID, prior.SessionID)
				if prior.SessionID == sessionD {
					if !reflect.DeepEqual(current, prior) {
						t.Fatalf("title/body promoted an unverified alias: %+v", current)
					}
					continue
				}
				if current.AccountID != "" || current.ConversationID != conversationA || current.Revision != prior.Revision+1 || !reflect.DeepEqual(current.LastResponse, prior.LastResponse) {
					t.Fatalf("union member not safely cleared: %+v", current)
				}
			}
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			cfg.Conversations = nil
			other, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(context.Background())
			// Removing the resolver after union publication must still allow a
			// complete recorded reset, proving durable membership adoption.
			if err := other.Resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			if stopped {
				if _, err := other.UnbindConversation(context.Background(), scope.ID, conversationA, reset.Revision, map[string]uint64{sessionA: expected[sessionA] + 1, sessionB: expected[sessionB] + 1, sessionC: expected[sessionC] + 1}); err != nil {
					t.Fatal(err)
				}
			}
			if member := observed(t, other, scope.ID, sessionC); member.ConversationID != conversationA || member.AccountID != "" {
				t.Fatalf("union reset lost durable alias ownership %+v", member)
			}
		})
	}
}

func TestConversationResetUnionRequiresCompleteCASAndRevalidatedAliasProof(t *testing.T) {
	for _, refusal := range []string{"missing member", "stale new member", "unverified alias", "source failure", "source failure after proof", "proof lost", "proof changed", "different saved conversation"} {
		t.Run(refusal, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			cfg.Transport = transportFunc(echoCredential)
			var armed atomic.Bool
			var lookups atomic.Int32
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				current := armed.Load()
				sequence := int32(0)
				if current {
					sequence = lookups.Add(1)
				}
				if current && (refusal == "source failure" || refusal == "source failure after proof" && sequence > 1) {
					return nil, errors.New("fixture metadata source failed")
				}
				out := make(map[string]string)
				for _, id := range ids {
					if id == sessionB || id == sessionA && !current {
						out[id] = conversationA
					}
					if id == sessionC {
						if !current && refusal == "different saved conversation" {
							out[id] = conversationB
						}
						if current && refusal != "unverified alias" && !(refusal == "proof lost" && sequence > 1) {
							out[id] = conversationA
							if refusal == "proof changed" && sequence > 1 {
								out[id] = conversationB
							}
						}
					}
				}
				return out, nil
			})
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			for _, id := range []string{sessionA, sessionB, sessionC} {
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{"title":"same title","conversation_id":"`+conversationA+`"}`, id, nil))
			}
			group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			expected := memberRevisions(m, scope.ID, conversationA)
			expected[sessionC] = observed(t, m, scope.ID, sessionC).Revision
			if refusal == "missing member" {
				delete(expected, sessionC)
			}
			if refusal == "stale new member" {
				expected[sessionC]++
			}
			before, bindings := m.Sessions(), m.ConversationBindings()
			armed.Store(true)
			if _, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, group.Revision, expected); !errors.Is(err, desktoprelay.ErrConflict) {
				t.Fatalf("unproven or stale union reset %s: %v", refusal, err)
			}
			if !reflect.DeepEqual(m.Sessions(), before) || !reflect.DeepEqual(m.ConversationBindings(), bindings) || m.Status().Listening {
				t.Fatal("rejected union reset changed authority, association or listener state")
			}
		})
	}
}
