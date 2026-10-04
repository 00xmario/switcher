package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
	"switcher/internal/proxy"
	"switcher/internal/server"
	"switcher/internal/sessionmeta"
	"switcher/internal/settings"
	"switcher/internal/store"
)

const (
	relayE2EControlKey = "fixture-desktop-management-secret"
	relayE2ECallerA    = "fixture-caller-alpha-OAuth"
	relayE2ESavedA     = "fixture-saved-alpha-OAuth"
	relayE2ESavedB     = "fixture-saved-beta-OAuth"
	relayE2ERefresh    = "fixture-private-refresh-token"
	relayE2EUser       = "fixture-private-user-metadata"
	relayE2ESourceBody = "fixture-private-source-body"
	relayE2ESession    = "11111111-1111-4111-8111-111111111111"
	relayE2EPeer       = "22222222-2222-4222-8222-222222222222"
	relayE2ETarget     = "https://api.anthropic.com/v1/messages?beta=true"
	relayE2ESSE        = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture-message\"}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_fixture\",\"name\":\"fixture.deferred\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"ok\\\":true}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":17}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

type relayE2EBlockedTransport struct{ calls atomic.Int64 }

func (b *relayE2EBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	b.calls.Add(1)
	return nil, errors.New("fixture blocks default HTTP egress")
}

func relayE2EBlockDefaults(t *testing.T) {
	t.Helper()
	previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
	blocked := &relayE2EBlockedTransport{}
	http.DefaultTransport = blocked
	http.DefaultClient = &http.Client{Transport: blocked}
	t.Cleanup(func() {
		http.DefaultTransport, http.DefaultClient = previousTransport, previousClient
		if blocked.calls.Load() != 0 {
			t.Error("integration fixture attempted default HTTP egress")
		}
	})
}

type relayE2ESource struct {
	mu              sync.Mutex
	failures        map[string]error
	refreshFailures map[string]error
	prepared        []string
	refreshed       []relayE2ERefreshCall
}

type relayE2ERefreshCall struct{ accountID, rejected string }

func (s *relayE2ESource) Prepare(ctx context.Context, id string) (desktoprelay.Credential, error) {
	if err := ctx.Err(); err != nil {
		return desktoprelay.Credential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepared = append(s.prepared, id)
	if err := s.failures[id]; err != nil {
		return desktoprelay.Credential{}, err
	}
	switch id {
	case "alpha":
		return desktoprelay.Credential{AccountID: id, AccessToken: relayE2ESavedA}, nil
	case "beta":
		return desktoprelay.Credential{AccountID: id, AccessToken: relayE2ESavedB}, nil
	default:
		return desktoprelay.Credential{}, desktoprelay.ErrNotFound
	}
}

func (s *relayE2ESource) RefreshRejected(ctx context.Context, id, rejected string) (desktoprelay.Credential, error) {
	if err := ctx.Err(); err != nil {
		return desktoprelay.Credential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshed = append(s.refreshed, relayE2ERefreshCall{accountID: id, rejected: rejected})
	if err := s.refreshFailures[id]; err != nil {
		return desktoprelay.Credential{}, err
	}
	return desktoprelay.Credential{}, errors.New("fixture forbids unconfigured refresh")
}

func (s *relayE2ESource) fail(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[id] = err
}

type relayE2ECapture struct {
	URL    string
	Method string
	Header http.Header
	Body   []byte
}

type relayE2EUpstream struct {
	mu       sync.Mutex
	captures []relayE2ECapture
	reply    func(*http.Request) (*http.Response, error)
}

func (u *relayE2EUpstream) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" {
		return nil, errors.New("fixture refuses an unexpected upstream destination")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	u.captures = append(u.captures, relayE2ECapture{URL: r.URL.String(), Method: r.Method, Header: r.Header.Clone(), Body: body})
	reply := u.reply
	u.mu.Unlock()
	if reply != nil {
		return reply(r)
	}
	return relayE2EResponse(200, relayE2ESSE), nil
}

func (u *relayE2EUpstream) snapshot() []relayE2ECapture {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]relayE2ECapture(nil), u.captures...)
}

func (u *relayE2EUpstream) setReply(reply func(*http.Request) (*http.Response, error)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = reply
}

func relayE2EResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status,
		Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Fixture-Upstream": {"unchanged"}},
		Body:   io.NopCloser(strings.NewReader(body))}
}

type relayE2EFixture struct {
	manager  *desktoprelay.Manager
	cfg      desktoprelay.Config
	source   *relayE2ESource
	upstream *relayE2EUpstream
	accounts *store.Store
	proxy    *proxy.Manager
	main     *httptest.Server
	control  *http.Client
	device   string
	secrets  []string
	metadata *sessionmeta.Index
}

func newRelayE2EFixture(t *testing.T, resolvers ...desktoprelay.ConversationResolver) *relayE2EFixture {
	t.Helper()
	relayE2EBlockDefaults(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := &relayE2ESource{failures: map[string]error{}, refreshFailures: map[string]error{}}
	upstream := &relayE2EUpstream{}
	cfg := desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Port: 0, Source: source, Transport: upstream,
		FixtureTLSRoots: x509.NewCertPool(),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture blocks blind-tunnel egress")
		}}
	if len(resolvers) != 0 {
		cfg.Conversations = resolvers[0]
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	accounts := store.New(filepath.Join(root, "accounts"))
	for _, id := range []string{"alpha", "beta"} {
		if err := accounts.Save(store.Account{ID: id, Provider: "claude", Email: id + "@fixture.example.test",
			Token: store.Token{AccessToken: "fixture-store-access-" + id, RefreshToken: relayE2ERefresh,
				Extra: map[string]any{"user_id": relayE2EUser}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := accounts.SaveState(store.State{Active: map[string]string{"claude": "alpha"}, ManagementKey: relayE2EControlKey}); err != nil {
		t.Fatal(err)
	}
	pm, err := proxy.New(accounts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &relayE2EFixture{manager: m, cfg: cfg, source: source, upstream: upstream, accounts: accounts, proxy: pm,
		secrets: []string{relayE2EControlKey, relayE2ECallerA, relayE2ESavedA, relayE2ESavedB, relayE2ERefresh,
			relayE2EUser, relayE2ESourceBody, "fixture-store-access-alpha", "fixture-store-access-beta"}}
	f.metadata, _ = cfg.Conversations.(*sessionmeta.Index)
	f.serveManagement(t)
	return f
}

func (f *relayE2EFixture) serveManagement(t *testing.T) {
	t.Helper()
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-desktop-password"); err != nil {
		t.Fatal(err)
	}
	device, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	f.device = device
	api := &server.API{Store: f.accounts, Proxy: f.proxy, DesktopRelay: f.manager, DesktopSessionMetadata: f.metadata,
		Settings: prefs, ManagementKey: relayE2EControlKey}
	mux := http.NewServeMux()
	api.Register(mux)
	srv := httptest.NewUnstartedServer(mux)
	addr := srv.Listener.Addr().String()
	_, portText, err := net.SplitHostPort(addr)
	if err != nil || portText == "8787" {
		t.Fatal("management fixture did not receive an isolated ephemeral port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = server.ControlRequests(server.LocalOnly(port,
		(&server.AuthGate{Store: prefs, DesktopManagementKey: relayE2EControlKey}).Wrap(mux)))
	srv.Start()
	t.Cleanup(srv.Close)
	transport := &http.Transport{Proxy: nil, DialContext: relayE2EDialOnly(addr)}
	t.Cleanup(transport.CloseIdleConnections)
	f.main, f.control = srv, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func relayE2EDialOnly(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != address {
			return nil, errors.New("fixture refuses non-fixture network access")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

func (f *relayE2EFixture) controlRequest(t *testing.T, method, path, body string, want int) []byte {
	t.Helper()
	r, err := http.NewRequest(method, f.main.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Switcher-Desktop-Control", relayE2EControlKey)
	if path == "/api/state" {
		r.Header.Set("Authorization", "Bearer "+f.device)
	}
	response, err := f.control.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != want {
		t.Fatalf("%s %s: HTTP %d, read error %v", method, path, response.StatusCode, err)
	}
	if !json.Valid(raw) || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("management reply is not non-cacheable JSON")
	}
	if path != "/api/desktop-relay/scopes" || method != "POST" {
		f.assertPublic(t, raw)
	}
	return raw
}

func (f *relayE2EFixture) assertPublic(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range f.secrets {
		if strings.Contains(string(raw), secret) {
			t.Fatal("management or classified error reply disclosed fixture credential/source data")
		}
	}
	for _, field := range []string{`"access_token"`, `"refresh_token"`, `"oauth_account"`, `"credentials"`, `"proxy_url"`, `"user_id"`} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatal("public reply serialized secret-bearing metadata")
		}
	}
}

func (f *relayE2EFixture) startScope(t *testing.T) desktoprelay.ScopeSetup {
	t.Helper()
	f.controlRequest(t, "POST", "/api/desktop-relay/start", "", 200)
	raw := f.controlRequest(t, "POST", "/api/desktop-relay/scopes", `{"label":"fixture Desktop task"}`, 201)
	var scope desktoprelay.ScopeSetup
	if err := json.Unmarshal(raw, &scope); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(scope.ProxyURL)
	if err != nil || u.User == nil || scope.ID == "" || scope.CAPath != filepath.Join(f.cfg.DataRoot, "ca.pem") || scope.Env["HTTPS_PROXY"] != scope.ProxyURL || scope.Env["NODE_EXTRA_CA_CERTS"] != scope.CAPath {
		t.Fatal("HTTP scope setup omitted its process-local proxy/CA instructions")
	}
	secret, ok := u.User.Password()
	if !ok || secret == "" {
		t.Fatal("HTTP scope setup omitted proxy admission")
	}
	f.secrets = append(f.secrets, secret, scope.ProxyURL)
	return scope
}

func (f *relayE2EFixture) desktopClient(t *testing.T, scope desktoprelay.ScopeSetup) *http.Client {
	t.Helper()
	if scope.CAPath != filepath.Join(f.cfg.DataRoot, "ca.pem") {
		t.Fatal("fixture CA path escaped its private temporary data root")
	}
	ca, err := os.ReadFile(scope.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA is not valid PEM")
	}
	proxyURL, err := url.Parse(scope.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DialContext: relayE2EDialOnly(f.manager.Status().Address),
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableCompression: true,
		MaxIdleConnsPerHost: 2, ForceAttemptHTTP2: false}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func relayE2EPayload(session string, toolResult bool) []byte {
	inner, _ := json.Marshal(map[string]string{"session_id": session, "agent_id": "fixture-worker", "private": relayE2EUser})
	userID, _ := json.Marshal(string(inner))
	content := `[{"role":"user","content":"fixture first turn"}]`
	if toolResult {
		content = `[{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"fixture-signed-opaque"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_fixture","content":[{"type":"text","text":"done"}]},{"type":"tool_reference","tool_name":"fixture.deferred"}]}]`
	}
	return []byte(fmt.Sprintf(" {\n \"model\":\"fixture-sonnet\", \"stream\":true,\n \"metadata\":{\"user_id\":%s},\n \"messages\":%s,\n \"tools\":[{\"name\":\"fixture.deferred\",\"input_schema\":{\"type\":\"object\"},\"defer_loading\":true}],\n \"future\":{\"signed\":\"AA+/==\",\"cache_control\":{\"type\":\"ephemeral\"}}\n} ", userID, content))
}

func relayE2ERequest(t *testing.T, client *http.Client, session, name string, body []byte) (*http.Response, httptrace.GotConnInfo) {
	t.Helper()
	r, err := http.NewRequest("POST", relayE2ETarget, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = http.Header{
		"Authorization":            {"Bearer " + relayE2ECallerA},
		"X-Api-Key":                {"fixture-caller-api-key"},
		"Content-Type":             {"application/json"},
		"Accept":                   {"text/event-stream"},
		"User-Agent":               {"fixture-Desktop-Code"},
		"Anthropic-Version":        {"2023-06-01"},
		"Anthropic-Beta":           {"oauth-2025-04-20, interleaved-thinking-2025-05-14", "tools-2025-04-04"},
		"X-Claude-Code-Session-Id": {session},
		"X-Fixture-Request":        {name},
		"X-Fixture-End-To-End":     {"first value", "second value"},
	}
	var mu sync.Mutex
	var connection httptrace.GotConnInfo
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		mu.Lock()
		connection = info
		mu.Unlock()
	}}
	response, err := client.Do(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := connection.Conn.(*tls.Conn); !ok {
		t.Fatal("fake Desktop did not use a verified HTTPS connection through CONNECT")
	}
	return response, connection
}

func relayE2EDrain(t *testing.T, response *http.Response, status int, want string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status || string(body) != want {
		t.Fatalf("fake Desktop reply: HTTP %d, bytes %d, read error %v", response.StatusCode, len(body), err)
	}
}

type relayE2EView struct {
	Status   desktoprelay.Status    `json:"status"`
	Scopes   []desktoprelay.Scope   `json:"scopes"`
	Sessions []desktoprelay.Session `json:"sessions"`
}

func (f *relayE2EFixture) view(t *testing.T) relayE2EView {
	t.Helper()
	var view relayE2EView
	if err := json.Unmarshal(f.controlRequest(t, "GET", "/api/desktop-relay", "", 200), &view); err != nil {
		t.Fatal(err)
	}
	if view.Status.Validation != "fixture_tested" {
		t.Fatal("fake Desktop fixture was reported as live validated")
	}
	return view
}

func (f *relayE2EFixture) session(t *testing.T, scope, id string, requests, inFlight uint64) desktoprelay.Session {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, session := range f.view(t).Sessions {
			if session.ScopeID == scope && session.SessionID == id && session.Requests == requests && session.InFlight == inFlight {
				return session
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed task did not reach requests=%d in_flight=%d", requests, inFlight)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func relayE2EAccountPath(scope, session string) string {
	return "/api/desktop-relay/scopes/" + scope + "/sessions/" + session + "/account"
}

func (f *relayE2EFixture) bind(t *testing.T, scope, session, account string, revision uint64) desktoprelay.Session {
	t.Helper()
	raw := f.controlRequest(t, "POST", relayE2EAccountPath(scope, session), fmt.Sprintf(`{"account_id":%q,"revision":%d}`, account, revision), 200)
	var reply struct {
		Session desktoprelay.Session `json:"session"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Session.AccountID != account || reply.Session.Revision != revision+1 {
		t.Fatal("management did not acknowledge the requested account and next revision")
	}
	return reply.Session
}

func relayE2ECheckCapture(t *testing.T, capture relayE2ECapture, session, name, bearer string, body []byte, selected bool) {
	t.Helper()
	if capture.URL != relayE2ETarget || capture.Method != "POST" || !bytes.Equal(capture.Body, body) || capture.Header.Get("Authorization") != "Bearer "+bearer {
		t.Fatal("combined control/TLS forwarding changed the account, URL or original payload bytes")
	}
	for key, want := range map[string][]string{
		"Content-Type": {"application/json"}, "Accept": {"text/event-stream"}, "User-Agent": {"fixture-Desktop-Code"},
		"Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"oauth-2025-04-20, interleaved-thinking-2025-05-14", "tools-2025-04-04"},
		"X-Claude-Code-Session-Id": {session}, "X-Fixture-Request": {name}, "X-Fixture-End-To-End": {"first value", "second value"},
	} {
		if !reflect.DeepEqual(capture.Header.Values(key), want) {
			t.Fatalf("combined forwarding changed end-to-end header %s", key)
		}
	}
	if capture.Header.Get("X-Switcher-Desktop-Control") != "" || capture.Header.Get("Proxy-Authorization") != "" {
		t.Fatal("management or CONNECT authority reached the upstream fixture")
	}
	if (selected && capture.Header.Get("X-Api-Key") != "") || (!selected && capture.Header.Get("X-Api-Key") != "fixture-caller-api-key") {
		t.Fatal("selected/caller credential fencing changed")
	}
	raw, _ := json.Marshal(capture)
	if bytes.Contains(raw, []byte(relayE2EControlKey)) {
		t.Fatal("management key appeared in the upstream capture")
	}
}

func (f *relayE2EFixture) assertGlobalSelection(t *testing.T) {
	t.Helper()
	var state struct {
		Active map[string]string   `json:"active"`
		Relay  desktoprelay.Status `json:"desktop_relay"`
	}
	if err := json.Unmarshal(f.controlRequest(t, "GET", "/api/state", "", 200), &state); err != nil || state.Active["claude"] != "alpha" || state.Relay.Validation != "fixture_tested" {
		t.Fatal("Desktop task control changed global account selection or live-validation status")
	}
}

func TestDesktopRelayIntegrationSwitchOnReusedTLSAndResume(t *testing.T) {
	f := newRelayE2EFixture(t)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	firstBody := relayE2EPayload(relayE2ESession, false)
	response, firstTLS := relayE2ERequest(t, client, relayE2ESession, "caller-alpha", firstBody)
	relayE2EDrain(t, response, 200, relayE2ESSE)
	observed := f.session(t, scope.ID, relayE2ESession, 1, 0)
	if observed.AccountID != "" || observed.AgentID != "fixture-worker" || observed.Model != "fixture-sonnet" || observed.LastSeen.IsZero() {
		t.Fatal("the management listener did not observe the fake Desktop task")
	}
	f.assertGlobalSelection(t)
	bound := f.bind(t, scope.ID, relayE2ESession, "beta", observed.Revision)
	f.controlRequest(t, "POST", relayE2EAccountPath(scope.ID, relayE2ESession),
		fmt.Sprintf(`{"account_id":"alpha","revision":%d}`, observed.Revision), 409)
	toolBody := relayE2EPayload(relayE2ESession, true)
	response, secondTLS := relayE2ERequest(t, client, relayE2ESession, "selected-beta-tool-result", toolBody)
	if !secondTLS.Reused || secondTLS.Conn != firstTLS.Conn {
		response.Body.Close()
		t.Fatal("account selection was not exercised on the same keepalive TLS tunnel")
	}
	if response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("X-Fixture-Upstream") != "unchanged" {
		response.Body.Close()
		t.Fatal("SSE response headers changed")
	}
	relayE2EDrain(t, response, 200, relayE2ESSE)
	peerBody := relayE2EPayload(relayE2EPeer, true)
	response, peerTLS := relayE2ERequest(t, client, relayE2EPeer, "peer-caller-alpha", peerBody)
	if !peerTLS.Reused || peerTLS.Conn != firstTLS.Conn {
		response.Body.Close()
		t.Fatal("peer task did not share the fixture's real TLS tunnel")
	}
	relayE2EDrain(t, response, 200, relayE2ESSE)
	captures := f.upstream.snapshot()
	if len(captures) != 3 {
		t.Fatal("combined management/TLS flow sent unexpected upstream attempts")
	}
	relayE2ECheckCapture(t, captures[0], relayE2ESession, "caller-alpha", relayE2ECallerA, firstBody, false)
	relayE2ECheckCapture(t, captures[1], relayE2ESession, "selected-beta-tool-result", relayE2ESavedB, toolBody, true)
	relayE2ECheckCapture(t, captures[2], relayE2EPeer, "peer-caller-alpha", relayE2ECallerA, peerBody, false)
	selected := f.session(t, scope.ID, relayE2ESession, 2, 0)
	peer := f.session(t, scope.ID, relayE2EPeer, 1, 0)
	if selected.AccountID != "beta" || selected.Revision != bound.Revision || peer.AccountID != "" {
		t.Fatal("observed metrics/binding disagree with captured account attribution")
	}
	f.assertGlobalSelection(t)

	// Unbinding remains a management operation while the transport is disabled.
	f.controlRequest(t, "POST", "/api/desktop-relay/stop", "", 200)
	f.controlRequest(t, "DELETE", relayE2EAccountPath(scope.ID, relayE2ESession),
		fmt.Sprintf(`{"revision":%d}`, bound.Revision), 200)
	off := f.view(t)
	if off.Status.Enabled || off.Status.Listening || f.session(t, scope.ID, relayE2ESession, 2, 0).AccountID != "" {
		t.Fatal("unbinding while off enabled inference or retained the selected account")
	}
	f.controlRequest(t, "POST", "/api/desktop-relay/start", "", 200)

	// Restart only this explicitly enabled private fixture, preserving its scope
	// and unbinding. Port 0 requires rebasing the retained admission URL locally.
	f.main.Close()
	if err := f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := desktoprelay.New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	if err := resumed.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.manager = resumed
	f.serveManagement(t)
	if status := f.view(t).Status; !status.Enabled || !status.Listening {
		t.Fatal("private fixture restart lost explicit enablement")
	}
	u, err := url.Parse(scope.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = resumed.Status().Address
	scope.ProxyURL = u.String()
	scope.Env["HTTPS_PROXY"] = scope.ProxyURL
	restartedClient := f.desktopClient(t, scope)
	response, _ = relayE2ERequest(t, restartedClient, relayE2ESession, "unbound-after-resume", toolBody)
	relayE2EDrain(t, response, 200, relayE2ESSE)
	captures = f.upstream.snapshot()
	if len(captures) != 4 {
		t.Fatal("resume sent an unexpected inference request")
	}
	relayE2ECheckCapture(t, captures[3], relayE2ESession, "unbound-after-resume", relayE2ECallerA, toolBody, false)
	if f.session(t, scope.ID, relayE2ESession, 3, 0).AccountID != "" {
		t.Fatal("resume resurrected a selected account after explicit unbinding")
	}
	f.assertGlobalSelection(t)
	f.controlRequest(t, "DELETE", "/api/desktop-relay/scopes/"+scope.ID, "", 200)
	response, _ = relayE2ERequest(t, restartedClient, relayE2ESession, "revoked-scope", toolBody)
	defer response.Body.Close()
	if response.StatusCode != 403 || len(f.upstream.snapshot()) != 4 {
		t.Fatal("revoked admission allowed another selected or caller upstream request")
	}
	if view := f.view(t); len(view.Scopes) != 0 || len(view.Sessions) != 0 {
		t.Fatal("scope revocation retained its observed bindings")
	}
}

func TestDesktopRelayIntegrationCredentialErrorsHaveStableCodes(t *testing.T) {
	f := newRelayE2EFixture(t)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	response, _ := relayE2ERequest(t, client, relayE2ESession, "observe-error-task", relayE2EPayload(relayE2ESession, false))
	relayE2EDrain(t, response, 200, relayE2ESSE)
	observed := f.session(t, scope.ID, relayE2ESession, 1, 0)
	for _, tc := range []struct {
		name, code string
		failure    error
		status     int
	}{
		{"source account removed", "account_not_found", fmt.Errorf("%w: %s", desktoprelay.ErrNotFound, relayE2ESourceBody), 404},
		{"source busy", "credential_unavailable", fmt.Errorf("%w: %s", desktoprelay.ErrBusy, relayE2ESourceBody), 503},
		{"source unavailable", "credential_unavailable", errors.New(relayE2ESourceBody), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.source.fail("beta", tc.failure)
			raw := f.controlRequest(t, "POST", relayE2EAccountPath(scope.ID, relayE2ESession),
				fmt.Sprintf(`{"account_id":"beta","revision":%d}`, observed.Revision), tc.status)
			var reply struct {
				Error     string `json:"error"`
				ErrorCode string `json:"error_code"`
			}
			if err := json.Unmarshal(raw, &reply); err != nil || reply.Error == "" || reply.ErrorCode != tc.code {
				t.Fatalf("classified control reply has error_code=%q, want %q", reply.ErrorCode, tc.code)
			}
			current := f.session(t, scope.ID, relayE2ESession, 1, 0)
			if current.AccountID != "" || current.Revision != observed.Revision || len(f.upstream.snapshot()) != 1 {
				t.Fatal("failed credential preparation changed binding or sent selected inference")
			}
		})
	}
	f.source.fail("beta", nil)
	f.bind(t, scope.ID, relayE2ESession, "beta", observed.Revision)
	for _, tc := range []struct {
		name, code string
		failure    error
		status     int
	}{
		{"selected account removed", "account_not_found", fmt.Errorf("%w: %s", desktoprelay.ErrNotFound, relayE2ESourceBody), 404},
		{"selected credential unavailable", "credential_unavailable", errors.New(relayE2ESourceBody), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.source.fail("beta", tc.failure)
			response, _ := relayE2ERequest(t, client, relayE2ESession, tc.name, relayE2EPayload(relayE2ESession, true))
			f.assertIngressCredentialError(t, response, tc.status, tc.code)
			if len(f.upstream.snapshot()) != 1 {
				t.Fatal("failed selected credential reached the upstream transport")
			}
		})
	}
	f.source.fail("beta", nil)
	f.source.mu.Lock()
	f.source.refreshFailures["beta"] = fmt.Errorf("%w: %s", desktoprelay.ErrNotFound, relayE2ESourceBody)
	f.source.mu.Unlock()
	f.upstream.setReply(func(*http.Request) (*http.Response, error) {
		response := relayE2EResponse(401, `{"error":{"message":"fixture-private-upstream-401-body"}}`)
		response.Header.Set("Content-Type", "application/json")
		return response, nil
	})
	f.secrets = append(f.secrets, "fixture-private-upstream-401-body")
	response, _ = relayE2ERequest(t, client, relayE2ESession, "rejected-beta", relayE2EPayload(relayE2ESession, true))
	f.assertIngressCredentialError(t, response, 404, "account_not_found")
	captures := f.upstream.snapshot()
	if len(captures) != 2 {
		t.Fatal("removed selected account was retried after its real upstream 401")
	}
	relayE2ECheckCapture(t, captures[1], relayE2ESession, "rejected-beta", relayE2ESavedB, relayE2EPayload(relayE2ESession, true), true)
	f.source.mu.Lock()
	refreshed := append([]relayE2ERefreshCall(nil), f.source.refreshed...)
	f.source.mu.Unlock()
	if len(refreshed) != 1 || refreshed[0].accountID != "beta" || refreshed[0].rejected != relayE2ESavedB {
		t.Fatal("401 recovery did not belong to the selected rejected generation")
	}
	if f.session(t, scope.ID, relayE2ESession, 4, 0).AccountID != "beta" {
		t.Fatal("classified credential errors changed the explicit binding")
	}
	f.assertGlobalSelection(t)
}

func (f *relayE2EFixture) assertIngressCredentialError(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("classified TLS ingress reply: HTTP %d, read error %v", response.StatusCode, err)
	}
	f.assertPublic(t, raw)
	var reply struct {
		Type  string `json:"type"`
		Error struct {
			Details struct {
				ErrorCode string `json:"error_code"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Type != "error" || reply.Error.Details.ErrorCode != code {
		t.Fatalf("classified TLS ingress error_code=%q, want %q", reply.Error.Details.ErrorCode, code)
	}
}

func TestDesktopRelayIntegrationBindingPreservesAdmittedSSE(t *testing.T) {
	f := newRelayE2EFixture(t)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	initialBody := relayE2EPayload(relayE2ESession, false)
	response, _ := relayE2ERequest(t, client, relayE2ESession, "observe-stream-task", initialBody)
	relayE2EDrain(t, response, 200, relayE2ESSE)
	observed := f.session(t, scope.ID, relayE2ESession, 1, 0)
	alpha := f.bind(t, scope.ID, relayE2ESession, "alpha", observed.Revision)

	reader, writer := io.Pipe()
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); _ = reader.Close(); _ = writer.Close() })
	prefix := relayE2ESSE[:strings.Index(relayE2ESSE, "event: content_block_delta")]
	f.upstream.setReply(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Fixture-Request") != "held-alpha" {
			return relayE2EResponse(200, relayE2ESSE), nil
		}
		go func() {
			defer close(finished)
			defer writer.Close()
			if _, err := io.WriteString(writer, prefix); err != nil {
				return
			}
			select {
			case <-release:
				_, _ = io.WriteString(writer, strings.TrimPrefix(relayE2ESSE, prefix))
			case <-r.Context().Done():
				_ = writer.CloseWithError(r.Context().Err())
			}
		}()
		response := relayE2EResponse(200, "")
		response.Body = reader
		return response, nil
	})
	toolBody := relayE2EPayload(relayE2ESession, true)
	old, oldTLS := relayE2ERequest(t, client, relayE2ESession, "held-alpha", toolBody)
	defer old.Body.Close()
	if old.StatusCode != 200 {
		t.Fatalf("admitted old-account SSE: HTTP %d", old.StatusCode)
	}
	seenPrefix := make([]byte, len(prefix))
	if _, err := io.ReadFull(old.Body, seenPrefix); err != nil || string(seenPrefix) != prefix {
		t.Fatal("admitted SSE did not deliver its original message-start and ping bytes")
	}
	active := f.session(t, scope.ID, relayE2ESession, 2, 1)
	if active.AccountID != "alpha" || active.Revision != alpha.Revision {
		t.Fatal("in-flight metrics do not identify the admitted alpha stream")
	}
	beta := f.bind(t, scope.ID, relayE2ESession, "beta", active.Revision)
	if switched := f.session(t, scope.ID, relayE2ESession, 2, 1); switched.AccountID != "beta" || switched.Revision != beta.Revision {
		t.Fatal("management did not acknowledge beta while alpha remained in flight")
	}
	response, newTLS := relayE2ERequest(t, client, relayE2ESession, "new-beta-after-ack", toolBody)
	if newTLS.Conn == oldTLS.Conn {
		response.Body.Close()
		t.Fatal("concurrent HTTP/1.1 SSE requests unexpectedly shared an occupied connection")
	}
	relayE2EDrain(t, response, 200, relayE2ESSE)
	if f.session(t, scope.ID, relayE2ESession, 3, 1).AccountID != "beta" {
		t.Fatal("new request did not retain the acknowledged beta binding")
	}
	captures := f.upstream.snapshot()
	if len(captures) != 3 {
		t.Fatal("switching an admitted stream opened a replacement upstream stream")
	}
	relayE2ECheckCapture(t, captures[0], relayE2ESession, "observe-stream-task", relayE2ECallerA, initialBody, false)
	relayE2ECheckCapture(t, captures[1], relayE2ESession, "held-alpha", relayE2ESavedA, toolBody, true)
	relayE2ECheckCapture(t, captures[2], relayE2ESession, "new-beta-after-ack", relayE2ESavedB, toolBody, true)
	unblock()
	suffix, err := io.ReadAll(old.Body)
	if err != nil || string(seenPrefix)+string(suffix) != relayE2ESSE {
		t.Fatal("account selection interrupted or rewrote the admitted old-account SSE")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture stream writer did not finish")
	}
	if final := f.session(t, scope.ID, relayE2ESession, 3, 0); final.AccountID != "beta" || final.Revision != beta.Revision || len(f.upstream.snapshot()) != 3 {
		t.Fatal("completed metrics do not match the original alpha and later beta admissions")
	}
	f.assertGlobalSelection(t)
}

type relayE2EHeldBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *relayE2EHeldBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return 0, io.EOF
}

// Deliberately uncooperative fixture I/O lets management observe a runtime that
// has cancelled its sockets but has not finished draining its admitted work.
func (*relayE2EHeldBody) Close() error { return nil }

func TestDesktopRelayIntegrationLifecycleBusyIsConflict(t *testing.T) {
	f := newRelayE2EFixture(t)
	scope := f.startScope(t)
	client := f.desktopClient(t, scope)
	held := &relayE2EHeldBody{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(held.release) }) }
	t.Cleanup(unblock)
	f.upstream.setReply(func(*http.Request) (*http.Response, error) {
		response := relayE2EResponse(200, "")
		response.Body = held
		return response, nil
	})
	response, _ := relayE2ERequest(t, client, relayE2ESession, "held-shutdown", relayE2EPayload(relayE2ESession, false))
	defer response.Body.Close()
	select {
	case <-held.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture upstream body was not admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := f.manager.Close(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("private fixture shutdown did not retain its draining runtime: %v", err)
	}
	raw := f.controlRequest(t, "POST", "/api/desktop-relay/start", "", 409)
	var reply struct {
		Error     string `json:"error"`
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil || reply.Error == "" || reply.ErrorCode != "" {
		t.Fatal("lifecycle contention was misclassified as a credential-source 503")
	}
	unblock()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := f.manager.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	f.controlRequest(t, "POST", "/api/desktop-relay/start", "", 200)
	if view := f.view(t); !view.Status.Listening || !view.Status.Enabled {
		t.Fatal("completed private-fixture shutdown did not permit an explicit retry")
	}
}
