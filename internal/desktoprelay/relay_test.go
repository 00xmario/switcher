package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

const conversationX = "33333333-3333-4333-8333-333333333333"

type resolverFunc func(context.Context, []string) (map[string]string, error)

func (f resolverFunc) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	return f(ctx, ids)
}

func echoAuth(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization")))}, nil
}

// The relay never refuses a request for local policy reasons: thread fields,
// missing betas, unusual metadata and bodies it cannot parse go to Anthropic.
func TestRelayForwardsRequestsItDoesNotUnderstand(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var sent []string
	var mu sync.Mutex
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = append(sent, string(b))
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization")))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	bodies := []string{
		`{"thread":{"id":"t"},"messages":[]}`,
		`{"metadata":{"user_id":"not json"}}`,
		`not json at all`,
	}
	for _, body := range bodies {
		resp := send(t, c, br, "/v1/messages", body, sessionA, http.Header{"Anthropic-Beta": {"other-feature"}})
		if got := drain(t, resp); resp.StatusCode != 200 || got != "Bearer token-A" {
			t.Fatalf("%s: status %d auth %q", body, resp.StatusCode, got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 4 || sent[1] != bodies[0] || sent[3] != bodies[2] {
		t.Fatalf("bodies changed: %q", sent)
	}
}

func TestSelectedAccountReplacesOnlyTheCredential(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Api-Key") != "" && r.Header.Get("Authorization") != "Bearer caller-token" {
			t.Error("caller API key forwarded with selected credential")
		}
		if r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20,caller-feature" {
			t.Errorf("beta header changed: %q", r.Header.Get("Anthropic-Beta"))
		}
		return echoAuth(r)
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer caller-token" {
		t.Fatalf("unselected session used %q", got)
	}
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer token-A" {
		t.Fatalf("selected session used %q", got)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil)); got != "Bearer caller-token" {
		t.Fatalf("other session used %q", got)
	}
	session := observed(t, m, s.ID, sessionA)
	if session.LastResponse == nil || session.LastResponse.Route != "selected" || session.LastResponse.AccountID != "A" {
		t.Fatalf("last response %+v", session.LastResponse)
	}
}

func TestRejectedSelectedTokenRefreshesAndRetriesOnce(t *testing.T) {
	cfg := fixtureConfig(t)
	var refreshes atomic.Int32
	cfg.Source = sourceFunc{
		prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
			return desktoprelay.Credential{AccountID: id, AccessToken: "old"}, nil
		},
		refresh: func(_ context.Context, id, rejected string) (desktoprelay.Credential, error) {
			refreshes.Add(1)
			if rejected != "old" {
				t.Errorf("refresh got %q", rejected)
			}
			return desktoprelay.Credential{AccountID: id, AccessToken: "new"}, nil
		},
	}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") == "Bearer old" {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("expired"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization") + " " + string(b)))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	resp := send(t, c, br, "/v1/messages", `{"n":1}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || got != `Bearer new {"n":1}` || refreshes.Load() != 1 {
		t.Fatalf("retry status %d body %q refreshes %d", resp.StatusCode, got, refreshes.Load())
	}
}

// When the selected account's token is rejected and cannot be refreshed, the
// relay says so instead of passing a 401 that Claude Code would blame on its
// own login.
func TestRefreshFailureReportsTheSelectedAccount(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource() // refresh is disabled in this fixture
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer token-A" {
			return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("upstream 401"))}, nil
		}
		return echoAuth(r)
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 503 || !strings.Contains(got, "credential_unavailable") {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if last := observed(t, m, s.ID, sessionA).LastResponse; last == nil || last.Status != 401 || last.AccountID != "A" {
		t.Fatalf("last response %+v", last)
	}
}

func TestCredentialFailureIsReportedWithoutSendingCallerCredential(t *testing.T) {
	cfg := fixtureConfig(t)
	var broken atomic.Bool
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		if broken.Load() {
			return desktoprelay.Credential{}, errors.New("refresh token revoked: secret-detail")
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	var upstream atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return echoAuth(r)
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	broken.Store(true)
	resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	body := drain(t, resp)
	if resp.StatusCode != 503 || !strings.Contains(body, "credential_unavailable") || strings.Contains(body, "secret-detail") || upstream.Load() != 1 {
		t.Fatalf("status %d body %s upstream %d", resp.StatusCode, body, upstream.Load())
	}
}

// There is no local concurrency cap: Anthropic does its own rate limiting.
func TestManyConcurrentRequestsAllReachUpstream(t *testing.T) {
	const n = 40 // above the old local cap of 32
	cfg := fixtureConfig(t)
	release := make(chan struct{})
	var arrived atomic.Int32
	all := make(chan struct{})
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if arrived.Add(1) == n {
			close(all)
		}
		<-release
		return echoAuth(r)
	})
	_, s := startFixture(t, cfg)
	var wg sync.WaitGroup
	statuses := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, br := tunnel(t, s)
			resp := send(t, c, br, "/v1/messages", `{}`, "", nil)
			drain(t, resp)
			statuses <- resp.StatusCode
		}()
	}
	await(t, all)
	close(release)
	wg.Wait()
	close(statuses)
	for status := range statuses {
		if status != 200 {
			t.Fatalf("status %d", status)
		}
	}
}

func TestConversationSelectionFollowsNewSessionsAndIgnoresMetadataFailures(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(echoAuth)
	var fail atomic.Bool
	var members sync.Map
	members.Store(sessionA, conversationX)
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		if fail.Load() {
			return nil, errors.New("metadata unavailable")
		}
		out := make(map[string]string)
		for _, id := range ids {
			if conversation, ok := members.Load(id); ok {
				out[id] = conversation.(string)
			}
		}
		return out, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.BindConversation(context.Background(), s.ID, conversationX, "A"); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer token-A" {
		t.Fatalf("conversation member used %q", got)
	}
	// Desktop rotates the request session of the same conversation.
	members.Store(sessionB, conversationX)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil)); got != "Bearer token-A" {
		t.Fatalf("new conversation session used %q", got)
	}
	// A metadata failure never turns into an error for an unrelated session.
	fail.Store(true)
	other := "44444444-4444-4444-8444-444444444444"
	resp := send(t, c, br, "/v1/messages", `{}`, other, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || got != "Bearer caller-token" {
		t.Fatalf("metadata failure: status %d auth %q", resp.StatusCode, got)
	}
	if _, err := m.UnbindConversation(context.Background(), s.ID, conversationX); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil)); got != "Bearer caller-token" {
		t.Fatalf("reset conversation used %q", got)
	}
}

func TestOtherDestinationsAreTunneledUnchanged(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	cfg := fixtureConfig(t)
	_, s := startFixture(t, cfg)
	c, br, resp := connectRaw(t, s, l.Addr().String(), "setup")
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT %s = %d", l.Addr(), resp.StatusCode)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("tunnel echoed %q, %v", got, err)
	}
}

func TestSelectedRequestLooksLikeClaudeCodeLoggedInToThatAccount(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id, AccountUUID: "bbbbbbbb-0000-4000-8000-00000000000b"}, nil
	}}
	var seen struct {
		body, beta string
	}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		seen.body, seen.beta = string(b), r.Header.Get("Anthropic-Beta")
		return echoAuth(r)
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	userID := `{\"device_id\":\"dev\",\"account_uuid\":\"aaaaaaaa-0000-4000-8000-00000000000a\",\"session_id\":\"` + sessionA + `\"}`
	body := `{"model":"m","messages":[{"role":"user","content":"keep <&> bytes"}],"metadata":{"user_id":"` + userID + `"}}`
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil))
	if seen.body != body {
		t.Fatalf("unselected request changed: %s", seen.body)
	}
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	drain(t, send(t, c, br, "/v1/messages", body, sessionA, http.Header{"Anthropic-Beta": {"other-feature"}}))
	want := strings.Replace(body, "aaaaaaaa-0000-4000-8000-00000000000a", "bbbbbbbb-0000-4000-8000-00000000000b", 1)
	if seen.body != want {
		t.Fatalf("selected body\n got %s\nwant %s", seen.body, want)
	}
	if seen.beta != "other-feature,oauth-2025-04-20" {
		t.Fatalf("beta %q", seen.beta)
	}
}

func TestContinuedThreadAfterSwitchAsksForFullHistory(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var upstream atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		upstream.Add(1)
		return echoAuth(r)
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	create, cont := `{"thread":{"type":"create"}}`, `{"thread":{"type":"continue","previous_message_id":"msg_1"}}`
	for _, body := range []string{create, cont} {
		if resp := send(t, c, br, "/v1/messages", body, sessionA, nil); drain(t, resp) != "Bearer caller-token" {
			t.Fatal("thread on the caller account was not forwarded")
		}
	}
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	resp := send(t, c, br, "/v1/messages", cont, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 400 || !strings.Contains(got, "thread_unsupported_request") || upstream.Load() != 2 {
		t.Fatalf("continued thread after switch: %d %s", resp.StatusCode, got)
	}
	// The stateless resend and a new thread go to the selected account.
	for _, body := range []string{`{"messages":[]}`, create, cont} {
		if got := drain(t, send(t, c, br, "/v1/messages", body, sessionA, nil)); got != "Bearer token-A" {
			t.Fatalf("%s used %q", body, got)
		}
	}
}

func TestSubagentFollowsItsParentsSelection(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(echoAuth)
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	child := `{"metadata":{"user_id":"{\"session_id\":\"` + sessionB + `\",\"parent_session_id\":\"` + sessionA + `\"}"}}`
	if got := drain(t, send(t, c, br, "/v1/messages", child, "", nil)); got != "Bearer token-A" {
		t.Fatalf("subagent used %q", got)
	}
}

// Plain proxy requests (T3 Code's local MCP server) are forwarded with their
// streamed response, with or without the proxy login.
func TestPlainHTTPProxyRequestsAreForwarded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy login leaked to the destination")
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: "+r.Method+" "+r.URL.Path+" "+string(b)+"\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: done\n\n")
	}))
	defer upstream.Close()
	cfg := fixtureConfig(t)
	cfg.DialContext = (&net.Dialer{}).DialContext
	_, s := startFixture(t, cfg)
	proxy, _ := url.Parse(s.ProxyURL)
	for _, withLogin := range []bool{true, false} {
		if !withLogin {
			proxy.User = nil
		}
		client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy)}}
		resp, err := client.Post(upstream.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := drain(t, resp); resp.StatusCode != 200 || got != "data: POST /mcp {\"jsonrpc\":\"2.0\"}\n\ndata: done\n\n" {
			t.Fatalf("login=%v: %d %q", withLogin, resp.StatusCode, got)
		}
	}
}
