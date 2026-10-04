package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
	"switcher/internal/sessionmeta"
	"switcher/internal/settings"
)

const (
	metaFirstID  = "11111111-1111-4111-8111-111111111111"
	metaSecondID = "22222222-2222-4222-8222-222222222222"
	metaThirdID  = "33333333-3333-4333-8333-333333333333"
)

type metaForbiddenCredentials struct{ calls atomic.Int64 }

func (s *metaForbiddenCredentials) Prepare(context.Context, string) (desktoprelay.Credential, error) {
	s.calls.Add(1)
	return desktoprelay.Credential{}, errors.New("fixture forbids credential preparation")
}

func (s *metaForbiddenCredentials) RefreshRejected(context.Context, string, string) (desktoprelay.Credential, error) {
	s.calls.Add(1)
	return desktoprelay.Credential{}, errors.New("fixture forbids credential refresh")
}

func newMetadataAPI(t *testing.T, metadata ...*sessionmeta.Index) (*API, *http.ServeMux, string) {
	t.Helper()
	blockDesktopControlEgress(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	source := &metaForbiddenCredentials{}
	cfg := desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Source: source,
		Transport: desktopControlTransportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture forbids network egress")
		}}
	var index *sessionmeta.Index
	if len(metadata) != 0 {
		index, cfg.Conversations = metadata[0], metadata[0]
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.Close(context.Background())
		if source.calls.Load() != 0 {
			t.Error("metadata status accessed credentials")
		}
	})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope, err := m.CreateScope("Fixture tasks")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{metaFirstID, metaSecondID, metaThirdID} {
		observeDesktopControlTask(t, m, scope, id)
	}
	api := &API{DesktopRelay: m, DesktopSessionMetadata: index, ManagementKey: "fixture-management-key"}
	mux := http.NewServeMux()
	api.Register(mux)
	return api, mux, root
}

func metadataFixture(t *testing.T, root, relative, raw string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionMetadataGETEnrichesOnlyResponseView(t *testing.T) {
	api, mux, root := newMetadataAPI(t)
	desktop, projects := filepath.Join(root, "desktop"), filepath.Join(root, "projects")
	first := metadataFixture(t, desktop, "account/workspace/local_unrelated.json", `{"cliSessionId":"`+metaFirstID+`","title":"Fix <b>relay cards</b>","cwd":"/fixture/private/switcher","lastActivityAt":1780000000000,"messages":["fixture-private-message"],"token":"fixture-private-token"}`)
	metadataFixture(t, projects, "project/sessions-index.json", `{"entries":[{"sessionId":"`+metaSecondID+`","customTitle":"Saved CLI title","projectPath":"/fixture/private/other","firstPrompt":"fixture-private-first-prompt"}]}`)
	api.DesktopSessionMetadata = sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop, ProjectsRoot: projects})
	before := api.DesktopRelay.Sessions()
	statePath := filepath.Join(root, "relay/state.json")
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	w := desktopControlRequest(t, mux, "GET", "/api/desktop-relay", "", api.ManagementKey)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Sessions []struct {
			desktoprelay.Session
			sessionmeta.Info
		} `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Sessions) != 3 {
		t.Fatalf("sessions response = %s, %v", w.Body.String(), err)
	}
	want := map[string]sessionmeta.Info{
		metaFirstID:  {Title: "Fix <b>relay cards</b>", Project: "switcher", TitleSource: "desktop", ClientKind: "desktop"},
		metaSecondID: {Title: "Saved CLI title", Project: "other", TitleSource: "cli", ClientKind: "cli"},
		metaThirdID:  {},
	}
	for _, session := range response.Sessions {
		if session.Info != want[session.SessionID] {
			t.Fatalf("display metadata = %#v", session)
		}
	}
	// Existing typed consumers must still decode every network Session field.
	var compatible struct {
		Sessions []desktoprelay.Session `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &compatible); err != nil || !reflect.DeepEqual(compatible.Sessions, before) || !reflect.DeepEqual(api.DesktopRelay.Sessions(), before) {
		t.Fatal("enrichment changed network session state or compatibility")
	}
	for _, secret := range []string{"fixture-private-message", "fixture-private-token", "fixture-private-first-prompt", "/fixture/private", `"cwd"`, `"record"`, `"firstPrompt"`, `"messages"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("GET exposed %q", secret)
		}
	}
	var fields struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"scope_id": true, "session_id": true, "account_id": true, "revision": true, "last_seen": true, "requests": true, "in_flight": true, "agent_id": true, "parent_session_id": true, "model": true, "last_response": true, "conversation_id": true, "association_verified": true, "association_conflict": true, "title": true, "project": true, "title_source": true, "client_kind": true}
	for _, session := range fields.Sessions {
		for field := range session {
			if !allowed[field] {
				t.Fatalf("unexpected session JSON field %q", field)
			}
		}
	}
	if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(state, after) {
		t.Fatal("GET persisted display metadata to relay state")
	}
	if after, err := os.ReadFile(first); err != nil || !bytes.Equal(metadata, after) {
		t.Fatal("GET modified saved session metadata")
	}
}

func TestSessionMetadataAuthorityGuardsRunBeforeLookup(t *testing.T) {
	api, mux, root := newMetadataAPI(t)
	api.Settings = settings.New(filepath.Join(root, "preferences"))
	if err := api.Settings.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	cookie, err := api.Settings.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	device, err := api.Settings.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	handler := LocalOnlyWith(LocalOptions{Port: 9123, LANHost: "192.0.2.5"}, (&AuthGate{Store: api.Settings, DesktopManagementKey: api.ManagementKey}).Wrap(mux))
	for _, tc := range []struct {
		name, host, remote, key, bearer, cookie string
		status                                  int
	}{
		{"no authority", "127.0.0.1:9123", "127.0.0.1:1234", "", "", "", 401},
		{"wrong key", "127.0.0.1:9123", "127.0.0.1:1234", "wrong", "", "", 401},
		{"caller OAuth", "127.0.0.1:9123", "127.0.0.1:1234", "", "Bearer fixture-caller-auth", "", 401},
		{"scope credential", "127.0.0.1:9123", "127.0.0.1:1234", "", "Basic Zml4dHVyZTpzY29wZS10b2tlbg==", "", 401},
		{"LAN cookie", "192.0.2.5:9123", "192.0.2.6:1234", "", "", cookie, 403},
		{"LAN device", "127.0.0.1:9123", "192.0.2.6:1234", "", "Bearer " + device, "", 403},
		{"LAN key", "127.0.0.1:9123", "192.0.2.6:1234", api.ManagementKey, "", "", 403},
		{"DNS host", "attacker.example:9123", "127.0.0.1:1234", api.ManagementKey, "", "", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desktop := filepath.Join(root, "metadata-"+strings.ReplaceAll(tc.name, " ", "-"))
			path := metadataFixture(t, desktop, "account/workspace/local_first.json", `{"cliSessionId":"`+metaFirstID+`","title":"Before rejected request"}`)
			api.DesktopSessionMetadata = sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop})
			r := httptest.NewRequest("GET", "http://127.0.0.1:9123/api/desktop-relay", nil)
			r.Host, r.RemoteAddr = tc.host, tc.remote
			r.Header.Set(desktopControlHeader, tc.key)
			r.Header.Set("Authorization", tc.bearer)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "Before rejected request") {
				t.Fatalf("rejected request = %d %s", w.Code, w.Body.String())
			}
			if err := os.WriteFile(path, []byte(`{"cliSessionId":"`+metaFirstID+`","title":"After rejected request"}`), 0600); err != nil {
				t.Fatal(err)
			}
			w = desktopControlRequest(t, handler, "GET", "/api/desktop-relay", "", api.ManagementKey)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "After rejected request") {
				t.Fatalf("rejected request warmed the lookup cache: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSessionMetadataNilAndCanceledGETKeepNetworkSessions(t *testing.T) {
	api, mux, root := newMetadataAPI(t)
	metadataFixture(t, root, "Library/Application Support/Claude/claude-code-sessions/account/workspace/local_first.json", `{"cliSessionId":"`+metaFirstID+`","title":"Forbidden HOME fallback"}`)
	metadataFixture(t, root, ".claude/projects/project/sessions-index.json", `{"entries":[{"sessionId":"`+metaSecondID+`","summary":"Forbidden CLI fallback"}]}`)
	for _, canceled := range []bool{false, true} {
		if canceled {
			api.DesktopSessionMetadata = sessionmeta.New(sessionmeta.Config{DesktopRoot: filepath.Join(root, "Library/Application Support/Claude/claude-code-sessions")})
		}
		r := httptest.NewRequest("GET", "http://127.0.0.1:9123/api/desktop-relay", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set(desktopControlHeader, api.ManagementKey)
		if canceled {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			r = r.WithContext(ctx)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		var response struct {
			Sessions []desktoprelay.Session `json:"sessions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != 200 || !reflect.DeepEqual(response.Sessions, api.DesktopRelay.Sessions()) {
			t.Fatalf("nil/canceled response lost network sessions: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"title"`) || strings.Contains(w.Body.String(), `"project"`) || strings.Contains(w.Body.String(), `"client_kind"`) {
			t.Fatal("nil/canceled lookup fabricated metadata")
		}
	}
}

func TestSessionMetadataSetupAndGlobalStateDoNotScan(t *testing.T) {
	f := newDesktopSetupFixture(t)
	desktop := filepath.Join(f.root, "metadata")
	path := metadataFixture(t, desktop, "account/workspace/local_first.json", `{"cliSessionId":"`+metaFirstID+`","title":"Before other routes"}`)
	f.api.DesktopSessionMetadata = sessionmeta.New(sessionmeta.Config{DesktopRoot: desktop})
	f.request(t, "POST", "/configure", `{}`, 200)
	f.request(t, "POST", "/restore", `{}`, 200)
	w := desktopControlRequest(t, f.mux, "GET", "/api/state", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Before other routes") || strings.Contains(w.Body.String(), `"title_source"`) {
		t.Fatalf("global state included metadata: %d %s", w.Code, w.Body.String())
	}
	if err := os.WriteFile(path, []byte(`{"cliSessionId":"`+metaFirstID+`","title":"After other routes"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := f.api.DesktopSessionMetadata.Lookup(context.Background(), []string{metaFirstID}); got[metaFirstID].Title != "After other routes" {
		t.Fatalf("other route or constructor scanned metadata: %#v", got)
	}
	if f.restart.Load() != 0 || f.sync.Load() != 0 {
		t.Fatal("metadata wiring invoked app control or session sync")
	}
}
