package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switcher/internal/claudesync"
	"switcher/internal/settings"
	"switcher/internal/store"
)

func TestClaudeSyncGuardsAndNames(t *testing.T) {
	st := store.New(t.TempDir())
	if err := st.Save(store.Account{ID: "claude-test", Provider: "claude", Email: "account@example.com", Token: store.Token{AccountID: "desktop-uuid"}}); err != nil {
		t.Fatal(err)
	}
	prefs := settings.New(t.TempDir())
	if err := prefs.SetPassword("test-password"); err != nil {
		t.Fatal(err)
	}
	token, err := prefs.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	a := &API{Store: st, Settings: prefs, syncClaudeForTest: func(ctx context.Context, labels map[string]string) (claudesync.Result, error) {
		calls++
		if labels["desktop-uuid"] != "account@example.com" {
			t.Fatal("account UUID was not mapped to a label")
		}
		return claudesync.Result{Added: 2, Backup: "fixture-backup", Accounts: []claudesync.AccountResult{{ID: "desktop-uuid", Label: labels["desktop-uuid"], Added: 2, Total: 4}}}, nil
	}}
	mux := http.NewServeMux()
	a.Register(mux)
	gate := (&AuthGate{Store: prefs}).Wrap(mux)
	for _, tc := range []struct {
		name, remote, cookie, csrf string
		status                     int
	}{
		{"unauthenticated", "127.0.0.1:8888", "", "", 401},
		{"missing CSRF", "127.0.0.1:8888", token, "", 403},
		{"LAN spoofing Host", "192.168.1.2:8888", token, prefs.CSRFToken(), 403},
		{"local authorized", "127.0.0.1:8888", token, prefs.CSRFToken(), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/claude/sync", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Switcher-CSRF", tc.csrf)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "switcher_session", Value: tc.cookie})
			}
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if w.Code == 200 {
				var body struct {
					Result claudesync.Result `json:"result"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Result.Added != 2 {
					t.Fatalf("counts missing: %v", err)
				}
				if strings.Contains(w.Body.String(), "access_token") {
					t.Fatal("token disclosure")
				}
			}
		})
	}
	if calls != 1 {
		t.Fatalf("runner called %d times", calls)
	}
}
