package desktoprelay_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

const conversationA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const conversationB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
const sessionC = "33333333-3333-4333-8333-333333333333"
const sessionD = "44444444-4444-4444-8444-444444444444"
const sessionE = "55555555-5555-4555-8555-555555555555"

type resolverFunc func(context.Context, []string) (map[string]string, error)

func (f resolverFunc) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	return f(ctx, ids)
}

// These synthetic fixture maps have no metadata I/O or scan to wait for.
func (f resolverFunc) ResolveCached(ids []string, _ map[string]string) map[string]string {
	result, _ := f(context.Background(), ids)
	return result
}

func TestMetadataChangeDuringCredentialPreparationBlocksSeparateChatDispatch(t *testing.T) {
	cfg := fixtureConfig(t)
	var changed, block atomic.Bool
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		conversation := conversationA
		if changed.Load() {
			conversation = conversationB
		}
		out := make(map[string]string)
		for _, id := range ids {
			out[id] = conversation
		}
		return out, nil
	})
	entered, release := make(chan struct{}), make(chan struct{})
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		if block.Load() {
			close(entered)
			<-release
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	var sent atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	result := make(chan int, 1)
	go func() { r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil); drain(t, r); result <- r.StatusCode }()
	await(t, entered)
	changed.Store(true)
	close(release)
	if status := <-result; status != 503 || sent.Load() != 1 {
		t.Fatalf("stale association dispatched into separate chat: HTTP %d sends %d", status, sent.Load())
	}
	if s := observed(t, m, scope.ID, sessionA); s.ConversationID != conversationA || s.AccountID != "A" || s.LastResponse.Route != "caller" {
		t.Fatalf("failed verification modified prior state: %+v", s)
	}
	_, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 1, memberRevisions(m, scope.ID, conversationA))
	var association *desktoprelay.AssociationError
	if !errors.As(err, &association) || !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("changed association classification %v", err)
	}
}

func metadataFixture(associations map[string]string) desktoprelay.ConversationResolver {
	return resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		for _, id := range ids {
			if conversation := associations[id]; conversation != "" {
				out[id] = conversation
			}
		}
		return out, nil
	})
}

// Both sides use real HTTPS, with no system trust, external DNS or provider.
func physicalProvider(t *testing.T, handler http.HandlerFunc) http.RoundTripper {
	t.Helper()
	upstream := httptest.NewTLSServer(handler)
	t.Cleanup(upstream.Close)
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api.anthropic.com:443" {
			t.Errorf("unexpected origin %q", addr)
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		secured := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		if err := secured.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return secured, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func memberRevisions(m *desktoprelay.Manager, scope, conversation string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, s := range m.Sessions() {
		if s.ScopeID == scope && s.ConversationID == conversation {
			out[s.SessionID] = s.Revision
		}
	}
	return out
}

func TestVerifiedConversationSelectionReachesFutureAliasesWithoutMigratingLegacySelection(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA, sessionB: conversationA, sessionC: conversationB, sessionE: conversationA})
	tokens := make(chan string, 16)
	body := ` {"model":"native-model","title":"same title","conversation_id":"` + conversationA + `","messages":[{"role":"user","content":"signed untouched"}],"future":{"opaque":true}} `
	cfg.Transport = physicalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("Authorization")
		got, _ := io.ReadAll(r.Body)
		if string(got) != body || r.URL.RawQuery != "beta=true" || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20,caller-feature" {
			t.Errorf("native request changed: %q %s %v", got, r.URL, r.Header)
		}
		w.WriteHeader(200)
		io.WriteString(w, "ok")
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	request := func(id, want string) {
		t.Helper()
		r := send(t, c, br, "/v1/messages?beta=true", body, id, nil)
		drain(t, r)
		if got := <-tokens; r.StatusCode != 200 || got != want {
			t.Fatalf("alias %s: HTTP %d credential %q, want %q", id, r.StatusCode, got, want)
		}
	}
	request(sessionA, "Bearer caller-token")
	first := observed(t, m, scope.ID, sessionA)
	if first.ConversationID != conversationA {
		t.Fatalf("unverified observation %+v", first)
	}
	if _, err := m.Bind(context.Background(), scope.ID, sessionA, "B", first.Revision); err != nil {
		t.Fatal(err)
	}
	if len(m.ConversationBindings()) != 0 {
		t.Fatal("legacy selection silently became whole-conversation consent")
	}
	request(sessionB, "Bearer caller-token")
	if observed(t, m, scope.ID, sessionB).AccountID != "" {
		t.Fatal("legacy override copied to new alias")
	}
	binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil || binding.Revision != 1 || binding.AccountID != "B" {
		t.Fatalf("group selection %+v %v", binding, err)
	}
	request(sessionB, "Bearer token-B")
	request(sessionE, "Bearer token-B")
	request(sessionC, "Bearer caller-token")
	request(sessionD, "Bearer caller-token")
	if !strings.EqualFold(observed(t, m, scope.ID, sessionC).ConversationID, conversationB) || observed(t, m, scope.ID, sessionD).ConversationID != "" {
		t.Fatal("body identity or title widened membership")
	}
}

func TestExactBindCannotBypassActiveConversationForNewlyVerifiedObservedAlias(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var known atomic.Bool
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		for _, id := range ids {
			if id == sessionA || known.Load() {
				out[id] = conversationA
			}
		}
		return out, nil
	})
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil))
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatal(err)
	}
	alias := observed(t, m, scope.ID, sessionB)
	known.Store(true)
	if _, err := m.Bind(context.Background(), scope.ID, sessionB, "A", alias.Revision); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("exact route bypassed whole-conversation selection: %v", err)
	}
	if got := observed(t, m, scope.ID, sessionB); got.AccountID != "" || got.ConversationID != "" || got.Revision != alias.Revision {
		t.Fatalf("management lookup silently changed observation %+v", got)
	}
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil))
	if got := observed(t, m, scope.ID, sessionB); got.AccountID != "B" || got.ConversationID != conversationA || got.Revision != alias.Revision+1 {
		t.Fatalf("later verified alias did not converge: %+v", got)
	}
}
