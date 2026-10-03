package server

import (
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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
	"switcher/internal/proxy"
	"switcher/internal/sessionmeta"
	"switcher/internal/settings"
	"switcher/internal/store"
)

type desktopControlBlockedTransport struct{}

func (desktopControlBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fixture blocks default HTTP egress")
}

type desktopControlSource struct{}

func (desktopControlSource) Prepare(_ context.Context, id string) (desktoprelay.Credential, error) {
	switch id {
	case "claude-busy":
		return desktoprelay.Credential{}, fmt.Errorf("%w: fixture-provider-body-secret", desktoprelay.ErrBusy)
	case "claude-broken":
		return desktoprelay.Credential{}, errors.New("fixture-provider-body-secret")
	}
	return desktoprelay.Credential{AccountID: id, AccessToken: "fixture-access-secret"}, nil
}

func (s desktopControlSource) RefreshRejected(ctx context.Context, id, _ string) (desktoprelay.Credential, error) {
	return s.Prepare(ctx, id)
}

type desktopControlTransportFunc func(*http.Request) (*http.Response, error)

func (f desktopControlTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newDesktopControlManager(t *testing.T, upstream ...http.RoundTripper) (*desktoprelay.Manager, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(root, "relay")
	var transport http.RoundTripper = desktopControlBlockedTransport{}
	if len(upstream) != 0 {
		transport = upstream[0]
	}
	m, err := desktoprelay.New(desktoprelay.Config{DataRoot: root, Port: 0, Source: desktopControlSource{},
		Transport: transport,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture blocks tunnel egress")
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, root
}

func observeDesktopControlTask(t *testing.T, manager *desktoprelay.Manager, setup desktoprelay.ScopeSetup, session string) {
	t.Helper()
	ca, err := os.ReadFile(setup.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("fixture CA is not PEM")
	}
	proxyURL, err := url.Parse(setup.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	address := manager.Status().Address
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != address {
				return nil, errors.New("fixture allows only ephemeral relay listener")
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	r, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"fixture-model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-Claude-Code-Session-Id", session)
	r.Header.Set("Authorization", "Bearer fixture-caller-OAuth")
	r.Header.Set("Content-Type", "application/json")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("fixture observation: %d", response.StatusCode)
	}
}

func TestDesktopRelayValidatesAndBoundsManagementJSON(t *testing.T) {
	blockDesktopControlEgress(t)
	m, _ := newDesktopControlManager(t)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := store.New(t.TempDir())
	for _, a := range []store.Account{{ID: "claude-a", Provider: "claude"}, {ID: "codex-a", Provider: "codex"}} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	api := &API{Store: st, DesktopRelay: m, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/desktop-relay/scopes", `{`, 400},
		{"POST", "/api/desktop-relay/scopes", `null`, 400},
		{"POST", "/api/desktop-relay/scopes", `{}`, 400},
		{"POST", "/api/desktop-relay/scopes", `{"label":"ok","unknown":true}`, 400},
		{"POST", "/api/desktop-relay/scopes", `{"label":"ok"} {}`, 400},
		{"POST", "/api/desktop-relay/scopes", `{"label":"one","label":"two"}`, 400},
		{"POST", "/api/desktop-relay/scopes", `{"label":"` + strings.Repeat("x", 9000) + `"}`, 400},
		{"POST", "/api/desktop-relay/scopes", `{"label":"` + strings.Repeat("x", 257) + `"}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"../claude-a","revision":0}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"claude-a"}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"claude-a","revision":-1}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"claude-a","revision":0,"revision":1}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"codex-a","revision":0}`, 400},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"missing","revision":0}`, 404},
		{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"account_id":"claude-a","revision":0}`, 404},
		{"DELETE", "/api/desktop-relay/scopes/scope/sessions/session/account", `{}`, 400},
		{"DELETE", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"revision":null}`, 400},
		{"DELETE", "/api/desktop-relay/scopes/scope/sessions/session/account", `{"revision":0}`, 404},
		{"DELETE", "/api/desktop-relay/scopes/missing", ``, 404},
	} {
		w := desktopControlRequest(t, mux, tc.method, tc.path, tc.body, api.ManagementKey)
		if w.Code != tc.status || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), "fixture-access-secret") {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.path, tc.body, w.Code, w.Body.String())
		}
	}
	if len(m.Scopes()) != 0 || len(m.Sessions()) != 0 {
		t.Fatal("invalid management request mutated state")
	}
}

func TestDesktopRelayConversationBodyRequiresUnambiguousMemberCAS(t *testing.T) {
	blockDesktopControlEgress(t)
	m, _ := newDesktopControlManager(t)
	st := store.New(t.TempDir())
	for _, account := range []store.Account{{ID: "claude-a", Provider: "claude"}, {ID: "codex-a", Provider: "codex"}} {
		if err := st.Save(account); err != nil {
			t.Fatal(err)
		}
	}
	api := &API{Store: st, DesktopRelay: m, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	const conversation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const member = "11111111-1111-4111-8111-111111111111"
	path := "/api/desktop-relay/scopes/scope/conversations/" + conversation + "/account"
	for _, body := range []string{
		`null`, `[]`, `{`, `{}`, `{"revision":0}`, `{"revision":null,"member_revisions":{"` + member + `":0}}`,
		`{"revision":0,"member_revisions":null}`, `{"revision":0,"member_revisions":{}}`,
		`{"revision":0,"member_revisions":{"` + member + `":null}}`,
		`{"revision":0,"member_revisions":{"` + member + `":-1}}`,
		`{"revision":0,"member_revisions":{"` + member + `":1.5}}`,
		`{"revision":0,"member_revisions":{"` + member + `":18446744073709551616}}`,
		`{"revision":0,"member_revisions":{"` + member + `":0,"` + member + `":1}}`,
		`{"revision":0,"member_revisions":{"` + member + `":0},"member_revisions":{"` + member + `":1}}`,
		`{"revision":0,"Revision":1,"member_revisions":{"` + member + `":0}}`,
		`{"Revision":0,"member_revisions":{"` + member + `":0}}`,
		`{"revision":0,"Member_Revisions":{"` + member + `":0}}`,
		`{"revision":0,"member_revisions":{"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA":0}}`,
		`{"revision":0,"member_revisions":{"00000000-0000-0000-0000-000000000000":0}}`,
		`{"revision":0,"member_revisions":{"title":0}}`,
		`{"revision":0,"member_revisions":{"../` + member + `":0}}`,
		`{"revision":0,"member_revisions":{"` + strings.ReplaceAll(member, "-", "_") + `":0}}`,
		`{"revision":0,"member_revisions":{"` + member + `":0},"title":"anything"}`,
		`{"revision":0,"member_revisions":{"` + member + `":0},"path":"/fixture/private"}`,
		`{"revision":0,"member_revisions":{"` + member + `":0}} {}`,
		`{"revision":0,"member_revisions":{"` + member + `":0},"unknown":"` + strings.Repeat("x", 8192) + `"}`,
	} {
		for _, method := range []string{"POST", "DELETE"} {
			w := desktopControlRequest(t, mux, method, path, body, api.ManagementKey)
			if w.Code != 400 || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("%s ambiguous body %s: %d %s", method, body, w.Code, w.Body.String())
			}
		}
	}
	validMembers := `,"revision":0,"member_revisions":{"` + member + `":0}}`
	for _, account := range []string{"../claude-a", "codex-a", ""} {
		w := desktopControlRequest(t, mux, "POST", path, `{"account_id":`+strconv.Quote(account)+validMembers, api.ManagementKey)
		if w.Code != 400 {
			t.Fatalf("invalid account %q: %d", account, w.Code)
		}
	}
	w := desktopControlRequest(t, mux, "POST", path, `{"account_id":"missing"`+validMembers, api.ManagementKey)
	if w.Code != 404 {
		t.Fatalf("missing account: %d %s", w.Code, w.Body.String())
	}
	w = desktopControlRequest(t, mux, "DELETE", path, `{"account_id":"claude-a"`+validMembers, api.ManagementKey)
	if w.Code != 400 {
		t.Fatal("reset accepted a selection-only field")
	}
	if len(m.Scopes()) != 0 || len(m.Sessions()) != 0 || len(m.ConversationBindings()) != 0 {
		t.Fatal("invalid grouped request changed relay state")
	}
}

func TestDesktopRelayConversationRouteWhitelistIsExact(t *testing.T) {
	blockDesktopControlEgress(t)
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	api := &API{Settings: prefs, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := LocalOnly(9123, (&AuthGate{Store: prefs, DesktopManagementKey: api.ManagementKey}).Wrap(mux))
	const base = "/api/desktop-relay/scopes/scope/"
	const conversation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for _, suffix := range []string{
		"conversations/" + conversation + "/account", "conversation/" + conversation + "/account",
		"conversations/" + conversation + "/accounts", "conversations/" + conversation + "/account/extra",
		"conversations/" + conversation + "/account/", "conversations/local_" + conversation + "/account",
		"conversations/not-a-uuid/account", "conversations/00000000-0000-0000-0000-000000000000/account",
		"conversations/AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA/account",
		"conversations/" + conversation + "%2fextra/account", "conversations/%2e%2e/account",
		"conversations/" + conversation + "%5c/account",
	} {
		path := base + suffix
		want := 401
		if suffix == "conversations/"+conversation+"/account" {
			want = 503 // Recognized route reaches the unavailable fixture manager.
		}
		for _, method := range []string{"POST", "DELETE"} {
			w := desktopControlRequest(t, handler, method, path, `{}`, api.ManagementKey)
			if w.Code != want {
				t.Fatalf("%s %s: %d, want %d", method, path, w.Code, want)
			}
		}
	}
	for _, scope := range []string{"../scope", "scope_extra", "scope%2fextra", "scope%5c", "scope%20"} {
		path := "/api/desktop-relay/scopes/" + scope + "/conversations/" + conversation + "/account"
		if w := desktopControlRequest(t, handler, "POST", path, `{}`, api.ManagementKey); w.Code != 401 {
			t.Fatalf("unsafe scope %s: %d", scope, w.Code)
		}
	}
}

func TestDesktopRelayConversationViewKeepsOwnedOrphan(t *testing.T) {
	binding := desktoprelay.ConversationBinding{ScopeID: "11111111-1111-4111-8111-111111111111",
		ConversationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", AccountID: "claude-a", Revision: 7}
	groups := desktopRelayConversations(nil, []desktoprelay.ConversationBinding{binding})
	if len(groups) != 1 || groups[0].ConversationBinding != binding || groups[0].SessionIDs == nil ||
		len(groups[0].SessionIDs) != 0 || groups[0].MemberRevisions == nil || len(groups[0].MemberRevisions) != 0 || groups[0].AssociationVerified {
		t.Fatal("GET group DTO dropped saved ownership without currently visible members")
	}
}

func TestDesktopRelaySessionViewUsesOnlyScopedSourceProof(t *testing.T) {
	const scope = "11111111-1111-4111-8111-111111111111"
	const alias = "22222222-2222-4222-8222-222222222222"
	const conversation = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const different = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for _, tc := range []struct {
		name, stored, display, proof, want   string
		available, owned, verified, conflict bool
	}{
		{"owned sparse proof despite matching display", conversation, conversation, "", conversation, true, true, false, false},
		{"unowned sparse proof despite matching display", conversation, conversation, "", "", true, false, false, false},
		{"fresh display alone", "", conversation, "", "", true, false, false, false},
		{"owned binding cannot prove an unknown alias", "", conversation, "", "", true, true, false, false},
		{"fresh scoped proof with display", "", conversation, conversation, conversation, true, false, true, false},
		{"fresh scoped proof without display", "", "", conversation, conversation, true, false, true, false},
		{"known matching scoped proof", conversation, conversation, conversation, conversation, true, true, true, false},
		{"historical scoped proof without display", conversation, "", conversation, conversation, true, true, true, false},
		{"source disagrees with saved identity", conversation, conversation, different, conversation, true, true, false, true},
		{"display disagrees with scoped proof", conversation, different, conversation, conversation, true, true, false, true},
		{"fresh display and scoped proof disagree", "", different, conversation, "", true, false, false, true},
		{"source error with matching display", conversation, conversation, "", conversation, false, true, false, false},
		{"source error discards partial proof", conversation, conversation, conversation, conversation, false, true, false, false},
		{"local prefix is not canonical source proof", conversation, conversation, "local_" + conversation, conversation, true, true, false, false},
		{"uppercase is not canonical source proof", conversation, conversation, strings.ToUpper(conversation), conversation, true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owned := map[desktopRelayConversationIdentity]bool{{scope, conversation}: tc.owned}
			session := desktoprelay.Session{ScopeID: scope, SessionID: alias, ConversationID: tc.stored, Revision: 2, AccountID: "claude-a"}
			metadata := sessionmeta.Info{ConversationID: tc.display, Title: "Display title", Project: "fixture-project", ClientKind: "desktop"}
			view := desktopRelaySessionAssociation(session, metadata, tc.proof, tc.available, owned)
			if view.ConversationID != tc.want || view.AssociationVerified != tc.verified || view.AssociationConflict != tc.conflict {
				t.Fatalf("association view = id %q, verified %t, conflict %t", view.ConversationID, view.AssociationVerified, view.AssociationConflict)
			}
			if view.Info != metadata || view.Session.ConversationID != tc.stored || view.Session.Revision != session.Revision || view.Session.AccountID != session.AccountID {
				t.Fatal("association decoration changed display metadata or captured session state")
			}
		})
	}
	view := desktopRelaySessionAssociation(desktoprelay.Session{ScopeID: scope, SessionID: alias, ConversationID: conversation},
		sessionmeta.Info{ConversationID: conversation}, "", true, map[desktopRelayConversationIdentity]bool{{different, conversation}: true})
	if view.ConversationID != "" || view.AssociationVerified {
		t.Fatal("missing scoped proof borrowed saved group ownership from another scope")
	}
}

func TestDesktopRelayBindingCASPreservesPeerTasksAndScopes(t *testing.T) {
	blockDesktopControlEgress(t)
	upstream := desktopControlTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.anthropic.com" {
			return nil, errors.New("fixture rejects unexpected upstream")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	m, _ := newDesktopControlManager(t, upstream)
	st := store.New(t.TempDir())
	for _, id := range []string{"claude-a", "claude-b", "claude-busy", "claude-broken"} {
		if err := st.Save(store.Account{ID: id, Provider: "claude", Token: store.Token{AccessToken: "fixture-access-secret", RefreshToken: "fixture-refresh-secret", Extra: map[string]any{"user_id": "fixture-private-user-metadata"}}}); err != nil {
			t.Fatal(err)
		}
	}
	api := &API{Store: st, DesktopRelay: m, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	request := func(method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := desktopControlRequest(t, mux, method, path, body, api.ManagementKey)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		for _, secret := range []string{"fixture-access-secret", "fixture-refresh-secret", "fixture-private-user-metadata", "fixture-provider-body-secret"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("control response disclosed credential or provider data")
			}
		}
		return w
	}
	request("POST", "/api/desktop-relay/start", "", 200)
	create := func(label string) desktoprelay.ScopeSetup {
		t.Helper()
		w := request("POST", "/api/desktop-relay/scopes", `{"label":"`+label+`"}`, 201)
		var scope desktoprelay.ScopeSetup
		if err := json.Unmarshal(w.Body.Bytes(), &scope); err != nil {
			t.Fatal(err)
		}
		return scope
	}
	own, peer := create("own tasks"), create("peer tasks")
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	observeDesktopControlTask(t, m, own, first)
	observeDesktopControlTask(t, m, own, second)
	observeDesktopControlTask(t, m, peer, first)
	var observed struct {
		Sessions []desktoprelay.Session `json:"sessions"`
	}
	if err := json.Unmarshal(request("GET", "/api/desktop-relay", "", 200).Body.Bytes(), &observed); err != nil {
		t.Fatal(err)
	}
	revisions := map[string]uint64{}
	for _, session := range observed.Sessions {
		revisions[session.ScopeID+"/"+session.SessionID] = session.Revision
	}
	if len(revisions) != 3 {
		t.Fatal("fixture tasks were not observed")
	}
	initial := revisions[own.ID+"/"+first]
	peerInitial := revisions[peer.ID+"/"+first]
	body := func(account string, revision uint64) string {
		return fmt.Sprintf(`{"account_id":%q,"revision":%d}`, account, revision)
	}
	path := func(scope, session string) string {
		return "/api/desktop-relay/scopes/" + scope + "/sessions/" + session + "/account"
	}
	request("POST", path(own.ID, first), body("claude-a", initial), 200)
	request("POST", path(peer.ID, first), body("claude-a", peerInitial), 200)
	request("POST", path(own.ID, first), body("claude-b", initial), 409)
	request("POST", path(own.ID, first), body("claude-busy", initial+1), 503)
	request("POST", path(own.ID, first), body("claude-broken", initial+1), 503)
	request("POST", path(own.ID, first), body("claude-b", initial+1), 200)
	assertTasks := func(primaryAccount string, revision uint64) {
		t.Helper()
		w := request("GET", "/api/desktop-relay", "", 200)
		var view struct {
			Sessions []desktoprelay.Session `json:"sessions"`
			Scopes   []desktoprelay.Scope   `json:"scopes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Sessions) != 3 || len(view.Scopes) != 2 {
			t.Fatal("management selection changed peer visibility")
		}
		for _, s := range view.Sessions {
			switch {
			case s.ScopeID == own.ID && s.SessionID == first:
				if s.AccountID != primaryAccount || s.Revision != revision {
					t.Fatal("CAS did not preserve the acknowledged binding")
				}
			case s.ScopeID == own.ID && s.SessionID == second:
				if s.AccountID != "" || s.Revision != revisions[own.ID+"/"+second] {
					t.Fatal("binding affected an unselected peer task")
				}
			case s.ScopeID == peer.ID && s.SessionID == first:
				if s.AccountID != "claude-a" || s.Revision != peerInitial+1 {
					t.Fatal("binding affected another admission scope")
				}
			default:
				t.Fatal("unexpected observed session")
			}
		}
		for _, setup := range []desktoprelay.ScopeSetup{own, peer} {
			u, _ := url.Parse(setup.ProxyURL)
			secret, _ := u.User.Password()
			if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "proxy_url") {
				t.Fatal("GET disclosed scope admission secret")
			}
		}
	}
	assertTasks("claude-b", initial+2)
	request("DELETE", path(own.ID, first), fmt.Sprintf(`{"revision":%d}`, initial+1), 409)
	assertTasks("claude-b", initial+2)
	request("DELETE", path(own.ID, first), fmt.Sprintf(`{"revision":%d}`, initial+2), 200)
	assertTasks("", initial+3)
	// A CONNECT admission credential grants no account-management authority.
	u, _ := url.Parse(own.ProxyURL)
	secret, _ := u.User.Password()
	r := httptest.NewRequest("POST", "http://127.0.0.1:9123"+path(own.ID, first), strings.NewReader(`{"account_id":"claude-a","revision":3}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.SetBasicAuth(u.User.Username(), secret)
	r.Header.Set("Proxy-Authorization", r.Header.Get("Authorization"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("scope admission authorized account selection")
	}
	request("DELETE", "/api/desktop-relay/scopes/"+own.ID, "", 200)
	w = request("GET", "/api/desktop-relay", "", 200)
	var remaining struct {
		Sessions []desktoprelay.Session `json:"sessions"`
		Scopes   []desktoprelay.Scope   `json:"scopes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &remaining); err != nil || len(remaining.Scopes) != 1 || len(remaining.Sessions) != 1 || remaining.Sessions[0].ScopeID != peer.ID || remaining.Sessions[0].AccountID != "claude-a" {
		t.Fatal("scope revocation affected a peer scope")
	}
}

func desktopControlRequest(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:9123"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	if key != "" {
		r.Header.Set(desktopControlHeader, key)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// UI contract examples, all protected by cookie plus CSRF, device bearer, or
// X-Switcher-Desktop-Control from local state.hub_management_key:
// GET /api/desktop-relay => {"status":{"enabled":false,"listening":false,
// "validation":"fixture_tested"},"scopes":[],"sessions":[]}
// POST /start or /stop => {"status":{...}}
// POST /scopes {"label":"future task"} => 201 {"id":"...","label":"future task",
// "proxy_url":"http://scope:SECRET@127.0.0.1:PORT","ca_path":"...",
// "env":{"HTTPS_PROXY":"...","NODE_EXTRA_CA_CERTS":"..."}}
// Copy setup from that response once. GET views never return admission secrets.
// POST /scopes/{scope}/sessions/{session}/account {"account_id":"claude-a","revision":1}
// and DELETE that route {"revision":2} => {"session":{...}}.
// Always send the revision from the observed session, not a default counter.
// 409 means reload the observed revision. 404 means an account/task/scope was
// removed or not observed. 503 means the relay or credential owner is unavailable.
func TestDesktopRelayDisabledSetupAndSecretDisclosure(t *testing.T) {
	blockDesktopControlEgress(t)
	m, root := newDesktopControlManager(t)
	if err := m.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := store.New(t.TempDir())
	pm, err := proxy.New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	api := &API{Store: st, Proxy: pm, Settings: prefs, DesktopRelay: m, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := LocalOnly(9123, (&AuthGate{Store: prefs}).Wrap(mux))
	w := desktopControlRequest(t, handler, "GET", "/api/desktop-relay", "", api.ManagementKey)
	var view struct {
		Status   desktoprelay.Status    `json:"status"`
		Scopes   []desktoprelay.Scope   `json:"scopes"`
		Sessions []desktoprelay.Session `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || view.Status.Enabled || view.Status.Listening || view.Status.Validation != "fixture_tested" || view.Scopes == nil || view.Sessions == nil {
		t.Fatalf("disabled view: %d %s, %v", w.Code, w.Body.String(), err)
	}
	if w := desktopControlRequest(t, handler, "POST", "/api/desktop-relay/start", "", ""); w.Code != 401 {
		t.Fatal("ambient local request enabled the relay")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("disabled setup touched relay data: %v", err)
	}
	if w := desktopControlRequest(t, handler, "POST", "/api/desktop-relay/start", "", api.ManagementKey); w.Code != 200 {
		t.Fatalf("explicit start: %d %s", w.Code, w.Body.String())
	}
	w = desktopControlRequest(t, handler, "POST", "/api/desktop-relay/scopes", `{"label":"future task"}`, api.ManagementKey)
	var setup desktoprelay.ScopeSetup
	if err := json.Unmarshal(w.Body.Bytes(), &setup); err != nil || w.Code != 201 || setup.ID == "" || setup.Label != "future task" {
		t.Fatalf("scope setup: %d %s, %v", w.Code, w.Body.String(), err)
	}
	proxyURL, err := url.Parse(setup.ProxyURL)
	if err != nil || proxyURL.User == nil {
		t.Fatal("scope setup omitted proxy admission")
	}
	secret, ok := proxyURL.User.Password()
	if !ok || secret == "" || setup.Env["HTTPS_PROXY"] != setup.ProxyURL || setup.Env["NODE_EXTRA_CA_CERTS"] != setup.CAPath {
		t.Fatal("setup is missing process-local environment instructions")
	}
	for _, path := range []string{"/api/desktop-relay", "/api/state", "/api/auth/status"} {
		w := desktopControlRequest(t, handler, "GET", path, "", api.ManagementKey)
		if w.Code != 200 || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), setup.ProxyURL) || strings.Contains(w.Body.String(), "fixture-access-secret") || strings.Contains(w.Body.String(), "refresh_token") {
			t.Fatalf("secret disclosure at %s: %d", path, w.Code)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if path == "/api/state" {
			var status desktoprelay.Status
			if err := json.Unmarshal(body["desktop_relay"], &status); err != nil || !status.Listening {
				t.Fatal("state omitted public relay status")
			}
		}
		if path == "/api/auth/status" && body["desktop_relay"] != nil {
			t.Fatal("auth status included relay metadata")
		}
	}
	if w := desktopControlRequest(t, handler, "POST", "/api/desktop-relay/stop", "", api.ManagementKey); w.Code != 200 || m.Status().Enabled || m.Status().Listening {
		t.Fatalf("explicit stop: %d", w.Code)
	}
}

func TestDesktopRelayPublishesUnavailableWithoutPrivateErrors(t *testing.T) {
	blockDesktopControlEgress(t)
	m, root := newDesktopControlManager(t)
	if err := os.WriteFile(root, []byte("fixture-private-store-body"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(context.Background()); err == nil {
		t.Fatal("fixture invalid store was accepted")
	}
	api := &API{DesktopRelay: m, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	w := desktopControlRequest(t, mux, "GET", "/api/desktop-relay", "", api.ManagementKey)
	var view struct {
		Status desktoprelay.Status `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != 200 || view.Status.Condition != "unavailable" || view.Status.Listening || strings.Contains(w.Body.String(), "fixture-private-store-body") {
		t.Fatalf("unavailable status: %d %s", w.Code, w.Body.String())
	}
	w = desktopControlRequest(t, mux, "POST", "/api/desktop-relay/start", "", api.ManagementKey)
	if w.Code != 503 || strings.Contains(w.Body.String(), root) || strings.Contains(w.Body.String(), "fixture-private-store-body") {
		t.Fatal("failed start disclosed a private store error")
	}
}

func TestDesktopRelayStateKeyIsLocalBrowserOnly(t *testing.T) {
	blockDesktopControlEgress(t)
	st := store.New(t.TempDir())
	pm, err := proxy.New(st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	session, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Store: st, Proxy: pm, Settings: prefs, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).Wrap(mux))
	for _, tc := range []struct {
		name, host, remote, cookie, bearer string
		reveal                             bool
	}{
		{"local browser", "127.0.0.1:9123", "127.0.0.1:1234", session, "", true},
		{"LAN browser", "192.0.2.5:9123", "192.0.2.6:1234", session, "", false},
		{"spoofed LAN browser", "127.0.0.1:9123", "192.0.2.6:1234", session, "", false},
		{"local device", "127.0.0.1:9123", "127.0.0.1:1234", "", "Bearer " + device, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+tc.host+"/api/state", nil)
			r.RemoteAddr = tc.remote
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			r.Header.Set("Authorization", tc.bearer)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
				t.Fatalf("state: %d %v", w.Code, err)
			}
			if (body["hub_management_key"] != nil) != tc.reveal {
				t.Fatal("state disclosed control key outside a local browser")
			}
		})
	}
}

func TestDesktopRelayKeepsExistingHostAndBrowserGuards(t *testing.T) {
	blockDesktopControlEgress(t)
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	api := &API{Settings: prefs, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	mux.HandleFunc("GET /claude/v1/messages", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	handler := LocalOnly(9123, (&AuthGate{Store: prefs, DesktopManagementKey: api.ManagementKey}).Wrap(mux))
	for _, tc := range []struct {
		path, host string
		status     int
	}{
		{"/api/auth/status", "attacker.example@localhost:9123", 403},
		{"/api/desktop-relay", "127.0.0.1:9123@attacker.example", 403},
		{"/api/desktop-relay", "attacker.example@localhost:9123", 403},
		{"/app.js", "127.0.0.1:9123", 401},
		{"/api/state", "127.0.0.1:9123", 401},
		{"/claude/v1/messages", "127.0.0.1:9123", 200},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:9123"+tc.path, nil)
		r.Host, r.RemoteAddr = tc.host, "127.0.0.1:1234"
		r.Header.Set(desktopControlHeader, api.ManagementKey)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("existing guard for %s host %s: %d", tc.path, tc.host, w.Code)
		}
	}
}

func blockDesktopControlEgress(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = desktopControlBlockedTransport{}
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func TestDesktopRelayRequiresIndependentLocalAuthority(t *testing.T) {
	blockDesktopControlEgress(t)
	prefs := settings.New(t.TempDir())
	api := &API{Settings: prefs, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: prefs}).Wrap(mux))
	for _, tc := range []struct {
		name, host, remote, key, bearer, query string
		status                                 int
	}{
		{"no authority", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "", 401},
		{"wrong key", "127.0.0.1:9123", "127.0.0.1:1234", "wrong", "", "", 401},
		{"caller OAuth", "127.0.0.1:9123", "127.0.0.1:1234", "", "Bearer fixture-OAuth", "", 401},
		{"query key", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "?key=fixture-management-key", 401},
		{"local key", "localhost:9123", "127.0.0.1:1234", "fixture-management-key", "", "", 503},
		{"IPv6 key", "[::1]:9123", "[::1]:1234", "fixture-management-key", "", "", 503},
		{"spoofed LAN socket", "127.0.0.1:9123", "192.0.2.6:1234", "fixture-management-key", "", "", 403},
		{"LAN Host", "192.0.2.5:9123", "127.0.0.1:1234", "fixture-management-key", "", "", 403},
		{"DNS Host", "relay.example:9123", "127.0.0.1:1234", "fixture-management-key", "", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []struct{ method, path string }{
				{"GET", "/api/desktop-relay"},
				{"POST", "/api/desktop-relay/start"},
				{"POST", "/api/desktop-relay/stop"},
				{"POST", "/api/desktop-relay/scopes"},
				{"DELETE", "/api/desktop-relay/scopes/scope"},
				{"POST", "/api/desktop-relay/scopes/scope/sessions/session/account"},
				{"DELETE", "/api/desktop-relay/scopes/scope/sessions/session/account"},
				{"POST", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account"},
				{"DELETE", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account"},
			} {
				r := httptest.NewRequest(route.method, "http://"+tc.host+route.path+tc.query, strings.NewReader(`{}`))
				r.RemoteAddr = tc.remote
				r.Header.Set("X-Switcher-Desktop-Control", tc.key)
				r.Header.Set("Authorization", tc.bearer)
				r.Header.Set("X-Forwarded-For", "127.0.0.1")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "fixture-") {
					t.Fatal("management response disclosed supplied secrets")
				}
			}
		})
	}
}

func TestDesktopRelayUsesCookieCSRFDeviceOrExplicitKey(t *testing.T) {
	blockDesktopControlEgress(t)
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	cookie, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := prefs.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Settings: prefs, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	gate := &AuthGate{Store: prefs, DesktopManagementKey: api.ManagementKey}
	handler := LocalOnly(9123, gate.Wrap(mux))
	for _, tc := range []struct {
		name, method, path, cookie, csrf, bearer, key, remote string
		status                                                int
	}{
		{"anonymous", "POST", "/api/desktop-relay/stop", "", "", "", "", "127.0.0.1:1234", 401},
		{"cookie no CSRF", "POST", "/api/desktop-relay/stop", cookie, "", "", "", "127.0.0.1:1234", 403},
		{"cookie wrong CSRF", "POST", "/api/desktop-relay/stop", cookie, "wrong", "", "", "127.0.0.1:1234", 403},
		{"cookie with CSRF", "POST", "/api/desktop-relay/stop", cookie, prefs.CSRFToken(), "", "", "127.0.0.1:1234", 503},
		{"cookie read", "GET", "/api/desktop-relay", cookie, "", "", "", "127.0.0.1:1234", 503},
		{"device", "POST", "/api/desktop-relay/stop", "", "", "Bearer " + device, "", "127.0.0.1:1234", 503},
		{"OAuth", "POST", "/api/desktop-relay/stop", "", "", "Bearer fixture-OAuth", "", "127.0.0.1:1234", 401},
		{"independent key", "POST", "/api/desktop-relay/stop", "", "", "", api.ManagementKey, "127.0.0.1:1234", 503},
		{"key preserves cookie CSRF", "POST", "/api/desktop-relay/stop", cookie, "", "", api.ManagementKey, "127.0.0.1:1234", 403},
		{"key cannot open state", "GET", "/api/state", "", "", "", api.ManagementKey, "127.0.0.1:1234", 401},
		{"key cannot open unknown route", "GET", "/api/desktop-relay/unknown", "", "", "", api.ManagementKey, "127.0.0.1:1234", 401},
		{"cookie LAN socket", "POST", "/api/desktop-relay/stop", cookie, prefs.CSRFToken(), "", "", "192.0.2.6:1234", 403},
		{"device LAN socket", "POST", "/api/desktop-relay/stop", "", "", "Bearer " + device, "", "192.0.2.6:1234", 403},
		{"group cookie no CSRF", "POST", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", cookie, "", "", "", "127.0.0.1:1234", 403},
		{"group cookie with CSRF", "POST", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", cookie, prefs.CSRFToken(), "", "", "127.0.0.1:1234", 503},
		{"group device reset", "DELETE", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", "", "", "Bearer " + device, "", "127.0.0.1:1234", 503},
		{"group independent key", "POST", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", "", "", "", api.ManagementKey, "127.0.0.1:1234", 503},
		{"group key preserves cookie CSRF", "DELETE", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", cookie, "", "", api.ManagementKey, "127.0.0.1:1234", 403},
		{"group key LAN socket", "POST", "/api/desktop-relay/scopes/scope/conversations/aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa/account", "", "", "", api.ManagementKey, "192.0.2.6:1234", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:9123"+tc.path, nil)
			r.RemoteAddr = tc.remote
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			r.Header.Set(csrfHeader, tc.csrf)
			r.Header.Set("Authorization", tc.bearer)
			r.Header.Set(desktopControlHeader, tc.key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
