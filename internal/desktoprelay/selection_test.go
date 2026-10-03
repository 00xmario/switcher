package desktoprelay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"switcher/internal/desktoprelay"
)

const sessionA = "11111111-1111-4111-8111-111111111111"
const sessionB = "22222222-2222-4222-8222-222222222222"

type sourceFunc struct {
	prepare func(context.Context, string) (desktoprelay.Credential, error)
	refresh func(context.Context, string, string) (desktoprelay.Credential, error)
}

func (s sourceFunc) Prepare(ctx context.Context, id string) (desktoprelay.Credential, error) {
	return s.prepare(ctx, id)
}
func (s sourceFunc) RefreshRejected(ctx context.Context, id, token string) (desktoprelay.Credential, error) {
	if s.refresh != nil {
		return s.refresh(ctx, id, token)
	}
	return desktoprelay.Credential{}, errors.New("fixture refresh disabled")
}
func fixtureSource() desktoprelay.CredentialSource {
	return sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
}

func drain(t *testing.T, r *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func observed(t *testing.T, m *desktoprelay.Manager, scope, id string) desktoprelay.Session {
	t.Helper()
	for _, s := range m.Sessions() {
		if s.ScopeID == scope && s.SessionID == id {
			return s
		}
	}
	t.Fatalf("task %s not observed", id)
	return desktoprelay.Session{}
}

func TestExplicitBindingSwitchesFutureRequestsOnSameTLSTunnel(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var mu sync.Mutex
	var tokens, bodies, keys []string
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		bodies = append(bodies, string(b))
		keys = append(keys, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", 0); !errors.Is(err, desktoprelay.ErrNotFound) {
		t.Fatalf("unobserved binding: %v", err)
	}
	body := ` {"model":"same-model","metadata":{"user_id":"{\"session_id\":\"` + sessionA + `\",\"agent_id\":\"worker\"}"},"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"native-id","content":"done"},{"type":"thinking","signature":"signed-opaque","thinking":"same"},{"type":"image","source":{"type":"base64","data":"opaque"}}]}],"tools":[{"name":"native.tool","defer_loading":true}],"future":{"cache_control":{"type":"ephemeral"},"reference":"unchanged"}} `
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	one := observed(t, m, s.ID, sessionA)
	bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", one.Revision)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	bound, err = m.Bind(context.Background(), s.ID, sessionA, "B", bound.Revision)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages/count_tokens", body, sessionA, nil))
	if _, err = m.Unbind(s.ID, sessionA, bound.Revision); err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(tokens, ",") != "Bearer caller-token,Bearer token-A,Bearer token-B,Bearer caller-token" {
		t.Fatalf("tokens: %v", tokens)
	}
	for _, b := range bodies {
		if b != body {
			t.Fatal("native payload changed")
		}
	}
	if strings.Join(keys, ",") != "caller-key,,,caller-key" {
		t.Fatalf("conflicting credentials: %v", keys)
	}
	view, _ := json.Marshal(struct {
		Status     desktoprelay.Status
		Scopes     []desktoprelay.Scope
		Sessions   []desktoprelay.Session
		Credential desktoprelay.Credential
	}{m.Status(), m.Scopes(), m.Sessions(), desktoprelay.Credential{AccessToken: "oauth-secret"}})
	if strings.Contains(string(view), "oauth-secret") || strings.Contains(string(view), "token-A") || strings.Contains(string(view), "proxy_url") {
		t.Fatalf("secret view: %s", view)
	}
}
