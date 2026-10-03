package desktoprelay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestResponseEvidenceDescribesReceivedMessageCredentialAndNotControlTraffic(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA, sessionB: conversationA})
	tokens := make(chan string, 16)
	cfg.Transport = physicalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("Authorization")
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "ok")
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	<-tokens
	if observed(t, m, scope.ID, sessionA).LastResponse != nil {
		t.Fatal("token counting claimed a Messages response")
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages?beta=true", `{"model":"requested-model","messages":[{"role":"user","content":"private prompt"}]}`, sessionB, nil))
	if got := <-tokens; got != "Bearer token-B" {
		t.Fatal(got)
	}
	s := observed(t, m, scope.ID, sessionB)
	if s.LastResponse == nil || s.LastResponse.Route != "selected" || s.LastResponse.AccountID != "B" || s.LastResponse.Model != "requested-model" || s.LastResponse.Status != 200 || s.LastResponse.At.IsZero() || s.LastResponse.Sequence == 0 {
		t.Fatalf("actual forwarding evidence %+v", s.LastResponse)
	}
	proof := *s.LastResponse
	// Public views own their evidence values, so a consumer cannot mutate state.
	s.LastResponse.AccountID = "external mutation"
	if observed(t, m, scope.ID, sessionB).LastResponse.AccountID != "B" {
		t.Fatal("public evidence pointer aliases manager state")
	}
	r, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	r.Header.Set("Authorization", "Bearer caller-token")
	r.Header.Set("X-Claude-Code-Session-Id", sessionB)
	if err := r.Write(c); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(br, r)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, response)
	if got := <-tokens; got != "Bearer caller-token" {
		t.Fatal(got)
	}
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{"model":"counter-model"}`, sessionB, nil))
	<-tokens
	if got := observed(t, m, scope.ID, sessionB).LastResponse; !reflect.DeepEqual(got, &proof) {
		t.Fatalf("control traffic replaced message evidence: %+v", got)
	}
	view, _ := json.Marshal(m.Sessions())
	for _, secret := range []string{"private prompt", "token-B", "caller-token", "external mutation", "access_token", "refresh_token", "Authorization"} {
		if strings.Contains(string(view), secret) {
			t.Fatalf("unsafe public view %s", view)
		}
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := observed(t, m, scope.ID, sessionB).LastResponse; !reflect.DeepEqual(got, &proof) {
		t.Fatalf("checkpoint lost safe response evidence: %+v", got)
	}
}

func TestResponseEvidenceIgnoresLocalBlockersAndNetworkFailures(t *testing.T) {
	for _, action := range []string{"thread", "beta", "removed account", "network failure", "count tokens", "missing identity"} {
		t.Run(action, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var fail atomic.Bool
			cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				if fail.Load() && action == "removed account" {
					return desktoprelay.Credential{}, desktoprelay.ErrNotFound
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			var sent atomic.Int32
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				sent.Add(1)
				if fail.Load() && action == "network failure" {
					return nil, errors.New("fixture transport failed")
				}
				return echoCredential(r)
			})
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			path, body, id, status := "/v1/messages", `{"model":"actual-model"}`, sessionA, 200
			var extra http.Header
			switch action {
			case "thread":
				body = `{"thread":null,"model":"actual-model"}`
				status = 400
			case "beta":
				extra = http.Header{"Anthropic-Beta": []string{"caller-feature"}}
				status = 400
			case "removed account":
				status = 404
			case "network failure":
				status = 502
			case "count tokens":
				path = "/v1/messages/count_tokens"
			case "missing identity":
				id = ""
			}
			r := send(t, c, br, path, body, id, extra)
			drain(t, r)
			if r.StatusCode != status || observed(t, m, scope.ID, sessionA).LastResponse != nil {
				t.Fatalf("non-response claimed Messages evidence: HTTP %d %+v", r.StatusCode, m.Sessions())
			}
			wantSends := int32(1)
			if action == "network failure" || action == "count tokens" || action == "missing identity" {
				wantSends = 2
			}
			if sent.Load() != wantSends {
				t.Fatalf("local refusal dispatched or fell back: sends %d", sent.Load())
			}
		})
	}
}

func TestResponseEvidence401RecoveryRecordsFinalReceivedGeneration(t *testing.T) {
	for _, final := range []string{"201", "401", "network failure", "removed account"} {
		t.Run(final, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
			src := fixtureSource().(sourceFunc)
			src.refresh = func(_ context.Context, id, rejected string) (desktoprelay.Credential, error) {
				if id != "B" || rejected != "token-B" {
					t.Errorf("wrong rejected generation %q %q", id, rejected)
				}
				if final == "removed account" {
					return desktoprelay.Credential{}, desktoprelay.ErrNotFound
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "fresh-B"}, nil
			}
			cfg.Source = src
			var messageSends atomic.Int32
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/v1/messages/count_tokens" {
					return echoCredential(r)
				}
				messageSends.Add(1)
				status := 401
				if r.Header.Get("Authorization") == "Bearer fresh-B" {
					if final == "network failure" {
						return nil, errors.New("fixture disconnected")
					}
					if final == "201" {
						status = 201
					}
				} else if r.Header.Get("Authorization") != "Bearer token-B" {
					t.Errorf("wrong selected account %v", r.Header)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream"))}, nil
			})
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
				t.Fatal(err)
			}
			r := send(t, c, br, "/v1/messages", `{"model":"preserved-model"}`, sessionA, nil)
			drain(t, r)
			proof := observed(t, m, scope.ID, sessionA).LastResponse
			wantStatus := 401
			if final == "201" {
				wantStatus = 201
			}
			if proof == nil || proof.Route != "selected" || proof.AccountID != "B" || proof.Model != "preserved-model" || proof.Status != wantStatus {
				t.Fatalf("retry response mislabeled %+v", proof)
			}
			wantSends := int32(2)
			if final == "removed account" {
				wantSends = 1
			}
			if messageSends.Load() != wantSends {
				t.Fatalf("generation replay count %d", messageSends.Load())
			}
			if final == "network failure" && r.StatusCode != 502 || final == "removed account" && r.StatusCode != 404 {
				t.Fatalf("local failure HTTP %d", r.StatusCode)
			}
		})
	}
}

func TestOlderConcurrentResponseCannotOverwriteNewerForwardingEvidence(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
	entered, release := make(chan struct{}), make(chan struct{})
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer token-A" {
			close(entered)
			<-release
		}
		return echoCredential(r)
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	go func() { result <- drain(t, send(t, c, br, "/v1/messages", `{"model":"older-model"}`, sessionA, nil)) }()
	await(t, entered)
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", binding.Revision, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	c2, br2 := tunnel(t, scope)
	if got := drain(t, send(t, c2, br2, "/v1/messages", `{"model":"newer-model"}`, sessionA, nil)); got != "Bearer token-B" {
		t.Fatal(got)
	}
	newer := observed(t, m, scope.ID, sessionA).LastResponse
	if newer == nil || newer.AccountID != "B" || newer.Model != "newer-model" {
		t.Fatalf("new response missing %+v", newer)
	}
	close(release)
	if got := <-result; got != "Bearer token-A" {
		t.Fatalf("admitted old request switched %q", got)
	}
	if got := observed(t, m, scope.ID, sessionA).LastResponse; !reflect.DeepEqual(got, newer) {
		t.Fatalf("older headers won completion race: %+v, want %+v", got, newer)
	}
}

func TestAdmittedConversationStreamEvidenceKeepsCapturedAccountAfterSwitch(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA, sessionB: conversationA})
	finish := make(chan struct{})
	cfg.Transport = physicalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		if r.Header.Get("Authorization") == "Bearer token-A" {
			<-finish
		}
		io.WriteString(w, "data: stop\n\n")
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	r := send(t, c, br, "/v1/messages", `{"model":"old-stream-model"}`, sessionA, nil)
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(r.Body, first); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", binding.Revision, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	s := observed(t, m, scope.ID, sessionA)
	if s.AccountID != "B" || s.LastResponse == nil || s.LastResponse.AccountID != "A" || s.LastResponse.Model != "old-stream-model" {
		t.Fatalf("selection acknowledgment fabricated forwarding evidence %+v", s)
	}
	c2, br2 := tunnel(t, scope)
	child := `{"model":"utility-model","metadata":{"user_id":"{\"session_id\":\"` + sessionB + `\",\"parent_session_id\":\"` + sessionA + `\",\"agent_id\":\"child\"}"}}`
	drain(t, send(t, c2, br2, "/v1/messages", child, sessionB, nil))
	if child := observed(t, m, scope.ID, sessionB); child.AccountID != "B" || child.LastResponse == nil || child.LastResponse.AccountID != "B" || child.LastResponse.Model != "utility-model" {
		t.Fatalf("verified child alias missed whole-conversation consent %+v", child)
	}
	close(finish)
	if got := drain(t, r); got != "data: stop\n\n" {
		t.Fatalf("SSE changed %q", got)
	}
}
