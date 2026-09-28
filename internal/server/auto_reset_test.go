package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/settings"
	"switcher/internal/store"
)

// autoResetHarness wires a store, a banked-reset-capable provider, and the
// routing manager exactly like main.go does.
type autoResetHarness struct {
	api    *API
	store  *store.Store
	mux    *http.ServeMux
	global *settings.Store
}

func newAutoResetHarness(t *testing.T) *autoResetHarness {
	t.Helper()
	prefs := settings.New(t.TempDir())
	accountStore := store.New(t.TempDir())
	prov := &resetCreditTestProvider{&recheckProvider{}}
	proxyManager, err := proxy.New(accountStore, map[string]provider.Provider{prov.ID(): prov}, []string{prov.ID()})
	if err != nil {
		t.Fatal(err)
	}
	proxyManager.SetAutoUseResetPolicy(func(a store.Account) bool {
		return a.AutoUseResetEnabled(prefs.Load().AutoUseReset)
	})
	a := &API{Settings: prefs, Store: accountStore, Proxy: proxyManager, Providers: map[string]provider.Provider{prov.ID(): prov}}
	mux := http.NewServeMux()
	a.Register(mux)
	return &autoResetHarness{api: a, store: accountStore, mux: mux, global: prefs}
}

func (h *autoResetHarness) patch(t *testing.T, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/accounts/"+id, bytes.NewBufferString(body))
	req.RemoteAddr, req.Host = "127.0.0.1:1234", "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func TestAccountAutoUseResetOverrideRoundTrip(t *testing.T) {
	h := newAutoResetHarness(t)
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		mode          string
		wantMode      string
		wantEffective bool
	}{
		{`{"auto_use_reset":"on"}`, "on", true},
		{`{"auto_use_reset":"off"}`, "off", false},
		{`{"auto_use_reset":"global"}`, "global", false},
	} {
		rec := h.patch(t, "fake-aaaa", tc.mode)
		if rec.Code != http.StatusOK {
			t.Fatalf("PATCH %s: %d %s", tc.mode, rec.Code, rec.Body.String())
		}
		var body struct {
			Account accountView `json:"account"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Account.AutoUseReset != tc.wantMode || body.Account.AutoUseResetEffective != tc.wantEffective {
			t.Fatalf("PATCH %s -> mode %q effective %t, want %q %t",
				tc.mode, body.Account.AutoUseReset, body.Account.AutoUseResetEffective, tc.wantMode, tc.wantEffective)
		}
		if !body.Account.SupportsBankedResets {
			t.Fatal("account with a banking provider must report supports_banked_resets")
		}
	}

	// An explicit override must survive a round trip through the store and
	// beat the global preference when the state is read back.
	yes := true
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com", AutoUseReset: &yes}); err != nil {
		t.Fatal(err)
	}
	if err := h.global.Update(func(st *settings.Settings) error {
		st.AutoUseReset = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rec := h.patch(t, "fake-aaaa", `{"auto_use_reset":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear override: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := h.store.Get("fake-aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AutoUseReset != nil {
		t.Fatal("setting the mode to global must clear the per-account override")
	}
}

func TestAccountAutoUseResetRejectsUnknownMode(t *testing.T) {
	h := newAutoResetHarness(t)
	if err := h.store.Save(store.Account{ID: "fake-aaaa", Provider: "fake", Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"auto_use_reset":"maybe"}`, `{"auto_use_reset":true}`, `not json`} {
		if rec := h.patch(t, "fake-aaaa", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("PATCH %s: %d, want 400", body, rec.Code)
		}
	}
	if rec := h.patch(t, "missing-account", `{"auto_use_reset":"on"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account: %d, want 404", rec.Code)
	}
	if rec := h.patch(t, "BAD-ID", `{"auto_use_reset":"on"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid account id: %d, want 400", rec.Code)
	}
}

func TestGlobalAutoUseResetSettingIsAcceptedWithoutRestart(t *testing.T) {
	h := newAutoResetHarness(t)
	previous := restartForTest
	restarted := false
	restartForTest = func() { restarted = true }
	defer func() { restartForTest = previous }()

	req := httptest.NewRequest(http.MethodPatch, "/api/settings", bytes.NewBufferString(`{"auto_use_reset":true}`))
	req.RemoteAddr, req.Host = "127.0.0.1:1234", "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH settings: %d %s", rec.Code, rec.Body.String())
	}
	if restarted {
		t.Fatal("a routing preference must not restart the listeners")
	}
	if !h.global.Load().AutoUseReset {
		t.Fatal("global auto_use_reset was not persisted")
	}

	get := httptest.NewRecorder()
	h.mux.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var payload struct {
		AutoUseReset bool `json:"auto_use_reset"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.AutoUseReset {
		t.Fatal("GET settings did not report auto_use_reset")
	}

	// With the global on, an account that follows it must report effective.
	if err := h.store.Save(store.Account{ID: "fake-bbbb", Provider: "fake", Email: "b@example.com"}); err != nil {
		t.Fatal(err)
	}
	state := httptest.NewRecorder()
	h.mux.ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var statePayload struct {
		Accounts []accountView `json:"accounts"`
	}
	if err := json.Unmarshal(state.Body.Bytes(), &statePayload); err != nil {
		t.Fatal(err)
	}
	for _, acc := range statePayload.Accounts {
		if acc.ID == "fake-bbbb" {
			if acc.AutoUseReset != "global" || !acc.AutoUseResetEffective {
				t.Fatalf("following account: mode %q effective %t, want global true",
					acc.AutoUseReset, acc.AutoUseResetEffective)
			}
			return
		}
	}
	t.Fatal("account missing from /api/state")
}
