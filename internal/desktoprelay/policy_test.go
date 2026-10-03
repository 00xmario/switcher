package desktoprelay_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestScopeAndPeerIsolationIncludesUnattachedChildrenAndControlTraffic(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var mu sync.Mutex
	var got []string
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	other, err := m.CreateScope("peer profile")
	if err != nil {
		t.Fatal(err)
	}
	c, br := tunnel(t, s)
	c2, br2 := tunnel(t, other)
	body := `{"model":"same-model"}`
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	drain(t, send(t, c2, br2, "/v1/messages", body, sessionA, nil))
	drain(t, send(t, c, br, "/v1/messages", body, sessionB, nil))
	child := `{"model":"same-model","metadata":{"user_id":"{\"session_id\":\"33333333-3333-4333-8333-333333333333\",\"parent_session_id\":\"` + sessionA + `\",\"agent_id\":\"child\"}"}}`
	drain(t, send(t, c, br, "/v1/messages", child, "", nil))
	childView := observed(t, m, s.ID, "33333333-3333-4333-8333-333333333333")
	if childView.AccountID != "" || childView.ParentSessionID != sessionA || childView.AgentID != "child" {
		t.Fatalf("child inherited: %+v", childView)
	}
	for _, path := range []string{"/v1/models", "/api/oauth/usage", "/v1/messages/", "/v1/messages-extra"} {
		drain(t, send(t, c, br, path, body, sessionA, nil))
	}
	mu.Lock()
	defer mu.Unlock()
	if got[1] != "Bearer token-A" {
		t.Fatalf("bound peer not selected %v", got)
	}
	for i, token := range got {
		if i != 1 && token != "Bearer caller-token" {
			t.Fatalf("leak at %d: %v", i, got)
		}
	}
}

func TestSelectedThreadsRefuseEveryTopLevelValueBeforeUpstream(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var sends int
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends++
		b, _ := io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{"model":"m"}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		for _, value := range []string{`{"type":"continue","previous_message_id":"opaque"}`, `null`, `false`, `"future"`, `[]`} {
			r := send(t, c, br, path, `{"model":"m","thread":`+value+`,"messages":[]}`, sessionA, nil)
			b := drain(t, r)
			var e struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
					Details struct {
						Code string `json:"error_code"`
					} `json:"details"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(b), &e) != nil || r.StatusCode != 400 || e.Type != "error" || e.Error.Type != "invalid_request_error" || e.Error.Details.Code != "thread_unsupported_request" || e.Error.Message != "message threads are not supported on selected-account routes" {
				t.Fatalf("thread refusal: %d %s", r.StatusCode, b)
			}
		}
	}
	if sends != 1 {
		t.Fatalf("thread delta sent upstream %d times", sends)
	}
	full := `{"model":"m","system":"full history","tools":[],"messages":[{"role":"user","content":"original context"}]}`
	if b := drain(t, send(t, c, br, "/v1/messages", full, sessionA, nil)); b != full {
		t.Fatal("full history retry changed")
	}
	unbound := `{"model":"m","thread":{"type":"continue"}}`
	if b := drain(t, send(t, c, br, "/v1/messages", unbound, sessionB, nil)); b != unbound {
		t.Fatal("unbound thread rewritten")
	}
}

func TestIdentityAmbiguityNeverObservesOrSelects(t *testing.T) {
	cases := []struct {
		name, body string
		headers    http.Header
	}{
		{"duplicate header", `{"model":"m"}`, http.Header{"X-Claude-Code-Session-Id": []string{sessionA, sessionA}}},
		{"zero UUID", `{"model":"m"}`, http.Header{"X-Claude-Code-Session-Id": []string{"00000000-0000-0000-0000-000000000000"}}},
		{"unsupported header", `{"model":"m"}`, http.Header{"X-Claude-Code-Session-Id": []string{"not-a-uuid"}}},
		{"metadata conflict", `{"metadata":{"user_id":"{\"session_id\":\"` + sessionB + `\"}"}}`, http.Header{"X-Claude-Code-Session-Id": []string{sessionA}}},
		{"duplicate metadata", `{"metadata":{},"metadata":{}}`, nil},
		{"duplicate user_id", `{"metadata":{"user_id":"{}","user_id":"{}"}}`, nil},
		{"duplicate inner session", `{"metadata":{"user_id":"{\"session_id\":\"` + sessionA + `\",\"session_id\":\"` + sessionA + `\"}"}}`, nil},
		{"legacy metadata", `{"metadata":{"user_id":"user_opaque"}}`, nil},
		{"unsupported encoding", `{"model":"m"}`, http.Header{"Content-Encoding": []string{"br"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			r := send(t, c, br, "/v1/messages", tc.body, "", tc.headers)
			b := drain(t, r)
			if r.StatusCode != 400 || !strings.Contains(b, "session_identity_invalid") || len(m.Sessions()) != 0 {
				t.Fatalf("identity admitted: %d %s %v", r.StatusCode, b, m.Sessions())
			}
		})
	}
}

func TestGzipRetainsOriginalWireBytesAndBoundsDecodedBody(t *testing.T) {
	// Race instrumentation and host load can make compression expensive. Build
	// both fixture payloads before opening a socket with an exchange deadline.
	compress := func(raw io.Reader) string {
		t.Helper()
		var wire bytes.Buffer
		z := gzip.NewWriter(&wire)
		if _, err := io.Copy(z, raw); err != nil {
			z.Close()
			t.Fatal(err)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		return wire.String()
	}
	nativeWire := compress(strings.NewReader(` {"model":"native-model","signed":"opaque","metadata":{"user_id":"{\"session_id\":\"` + sessionA + `\"}"}} `))
	oversizedWire := compress(io.MultiReader(strings.NewReader(`{"padding":"`), io.LimitReader(zeroReader{}, desktoprelay.MaxBodyBytes+1), strings.NewReader(`"}`)))
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var bodySeen []byte
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		bodySeen, _ = io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	request := func(body string, headers http.Header) *http.Response {
		t.Helper()
		// Bound each wire exchange separately, without charging fixture setup or
		// binding persistence against an earlier request's socket deadline.
		if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		return send(t, c, br, "/v1/messages", body, sessionA, headers)
	}
	drain(t, request(`{"model":"m"}`, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	r := request(nativeWire, http.Header{"Content-Encoding": []string{"gzip"}})
	drain(t, r)
	if r.StatusCode != 200 || !bytes.Equal(bodySeen, []byte(nativeWire)) {
		t.Fatal("compressed native body changed")
	}
	r = request(oversizedWire, http.Header{"Content-Encoding": []string{"gzip"}})
	drain(t, r)
	if r.StatusCode != 413 {
		t.Fatalf("gzip bomb status = %d", r.StatusCode)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestSelectedOAuthRequiresCallerBetaWithoutRewritingFeatureHeaders(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	for _, h := range []http.Header{{"Anthropic-Beta": []string{"caller-only"}}, {"Connection": []string{"Anthropic-Beta"}}} {
		r := send(t, c, br, "/v1/messages", `{}`, sessionA, h)
		b := drain(t, r)
		if r.StatusCode != 400 || !strings.Contains(b, "oauth_client_incompatible") {
			t.Fatalf("beta policy %d %s", r.StatusCode, b)
		}
	}
}
