package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"switcher/internal/codexcfg"
	"switcher/internal/settings"
	"switcher/internal/store"
)

func setupRequest(method, path, remote string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host, r.RemoteAddr = "127.0.0.1:9123", remote
	return r
}

func TestCLISetupReportsInspectedConfigAndStoredAccountsSeparately(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := codexcfg.InstallAt(configPath, 9123); err != nil {
		t.Fatal(err)
	}
	st := store.New(t.TempDir())
	for _, acc := range []store.Account{
		{ID: "codex-one", Provider: "codex", Email: "one@example.test", Token: store.Token{AccessToken: "secret-first"}},
		{ID: "codex-two", Provider: "codex", Email: "two@example.test", Token: store.Token{AccessToken: "secret-second"}},
		{ID: "claude-one", Provider: "claude", Email: "three@example.test", Token: store.Token{RefreshToken: "secret-third"}},
	} {
		if err := st.Save(acc); err != nil {
			t.Fatal(err)
		}
	}
	a := &API{Store: st, CodexConfigPath: configPath, Port: 9123}
	w := httptest.NewRecorder()
	a.handleCLISetup(w, setupRequest(http.MethodGet, "/api/cli-setup", "127.0.0.1:1234"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET setup: %d %s", w.Code, w.Body.String())
	}
	var body struct {
		Codex   codexcfg.Status  `json:"codex"`
		Clients []cliSetupClient `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Codex.Condition != "ready" || body.Codex.ExpectedURL != "http://127.0.0.1:9123/codex/v1" || len(body.Clients) != 7 {
		t.Fatalf("legacy Codex status or client list changed: %+v", body)
	}
	byID := map[string]cliSetupClient{}
	for _, client := range body.Clients {
		byID[client.ID] = client
		if client.Installation.Evidence != "not_checked" {
			t.Fatalf("claimed native CLI installation without checking: %+v", client)
		}
	}
	if got := byID["codex"]; got.Capability != "configure_user_file" || got.Configuration.Condition != "ready" ||
		got.Configuration.Scope != "inspected_user_file" || got.Accounts.Evidence != "known" || got.Accounts.Count == nil || *got.Accounts.Count != 2 {
		t.Fatalf("Codex evidence conflated config and accounts: %+v", got)
	}
	if got := byID["claude-code"]; got.Capability != "information_only" || got.Configuration.Condition != "not_checked" ||
		got.Accounts.Evidence != "known" || got.Accounts.Count == nil || *got.Accounts.Count != 1 {
		t.Fatalf("Claude was implied to be configured: %+v", got)
	}
	if got := byID["copilot-cli"]; got.Accounts.Count == nil || *got.Accounts.Count != 0 || got.Capability != "information_only" {
		t.Fatalf("empty stored accounts were misreported: %+v", got)
	}
	for id, reason := range map[string]string{
		"claude-code":     "oauth_coexistence_unverified",
		"opencode-go-v2":  "v2_account_model_transport_unverified",
		"grok-build":      "entitlement_and_relay_unverified",
		"copilot-cli":     "byok_is_different_billing",
		"gemini-cli":      "subscription_route_unverified",
		"antigravity-cli": "agy_protocol_unverified",
	} {
		client := byID[id]
		if client.ReasonCode != reason || client.NextStep == "" || client.Capability != "information_only" ||
			client.Configuration.Condition != "not_checked" || client.Configuration.InstallAction != "" {
			t.Fatalf("%s claimed unsupported CLI setup: %+v", id, client)
		}
	}
	for _, private := range []string{"secret-first", "secret-second", "secret-third", "@example.test"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("CLI setup leaked account data")
		}
	}
	denied := httptest.NewRecorder()
	a.handleCLISetup(denied, setupRequest(http.MethodGet, "/api/cli-setup", "192.168.1.20:1234"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("LAN GET status = %d, want 403", denied.Code)
	}
}

func TestCLISetupAccountCountUnknownDoesNotHideCodexStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := codexcfg.InstallAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st := store.New(root)
	if err := os.Remove(filepath.Join(root, "accounts")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &API{Store: st, CodexConfigPath: path, Port: 9123}
	w := httptest.NewRecorder()
	a.handleCLISetup(w, setupRequest(http.MethodGet, "/api/cli-setup", "127.0.0.1:1234"))
	var body struct {
		Codex   codexcfg.Status  `json:"codex"`
		Clients []cliSetupClient `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusOK || body.Codex.Condition != "ready" {
		t.Fatalf("lost Codex diagnosis when account list failed: %d, %v", w.Code, err)
	}
	for _, client := range body.Clients {
		if client.Accounts.Evidence != "unknown" || client.Accounts.Count != nil {
			t.Fatalf("unknown account count reported as zero: %+v", client)
		}
	}
	a.Store = nil
	w = httptest.NewRecorder()
	a.handleCLISetup(w, setupRequest(http.MethodGet, "/api/cli-setup", "127.0.0.1:1234"))
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Codex.Condition != "ready" {
		t.Fatalf("nil store hid Codex diagnosis: %v", err)
	}
	for _, client := range body.Clients {
		if client.Accounts.Evidence != "unknown" || client.Accounts.Count != nil {
			t.Fatalf("nil store account count reported as known: %+v", client)
		}
	}
}

func TestCLISetupMissingAlsoCoversExistingConfigWithoutSwitcherSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("model = \"gpt-5.5\"\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &API{CodexConfigPath: path, Port: 9123}
	w := httptest.NewRecorder()
	a.handleCLISetup(w, setupRequest(http.MethodGet, "/api/cli-setup", "127.0.0.1:1234"))
	var body struct {
		Codex   codexcfg.Status  `json:"codex"`
		Clients []cliSetupClient `json:"clients"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Codex.Condition != "missing" ||
		len(body.Clients) == 0 || body.Clients[0].Configuration.Condition != "missing" {
		t.Fatalf("existing config without selection misclassified: %d %v %+v", w.Code, err, body.Codex)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(original, after) {
		t.Fatalf("read-only setup check modified Codex config: %v", err)
	}
}

func TestCLISetupMutationKeepsExistingAuthAndTargetGate(t *testing.T) {
	preferences := settings.New(t.TempDir())
	if err := preferences.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	a := &API{Settings: preferences, CodexConfigPath: configPath, Port: 9123}
	mux := http.NewServeMux()
	a.Register(mux)
	secured := (&AuthGate{Store: preferences}).Wrap(mux)
	token, err := preferences.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path string, cookie, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		r := setupRequest(method, path, "127.0.0.1:1234")
		if cookie {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		}
		if csrf {
			r.Header.Set(csrfHeader, preferences.CSRFToken())
		}
		w := httptest.NewRecorder()
		secured.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, "/api/cli-setup", false, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET = %d", w.Code)
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/install", true, false); w.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d", w.Code)
	}
	if w := request(http.MethodPost, "/api/cli-setup/claude-code/install", true, true); w.Code != http.StatusNotFound {
		t.Fatalf("unsupported target install = %d", w.Code)
	}
	for _, id := range []string{"opencode-go-v2", "grok-build", "copilot-cli", "gemini-cli", "antigravity-cli"} {
		if w := request(http.MethodPost, "/api/cli-setup/"+id+"/install", true, true); w.Code != http.StatusNotFound {
			t.Fatalf("unsupported %s install = %d", id, w.Code)
		}
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/install", true, true); w.Code != http.StatusOK {
		t.Fatalf("authorized Codex install = %d: %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(configPath); err != nil || !bytes.Contains(raw, []byte("127.0.0.1:9123")) {
		t.Fatalf("Codex install did not use active port: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	userSelection := []byte(strings.Replace(string(raw), `model_provider = "switcher"`, `model_provider = "other"`, 1))
	if err := os.WriteFile(configPath, userSelection, 0o600); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/install", true, true); w.Code != http.StatusConflict {
		t.Fatalf("user selection silently overwritten: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(configPath); err != nil || !bytes.Equal(raw, userSelection) {
		t.Fatal("conflicting install changed the user's selection")
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/reselect", true, true); w.Code != http.StatusOK {
		t.Fatalf("explicit reselect: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/restore", true, true); w.Code != http.StatusOK {
		t.Fatalf("restore previous selection: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(configPath); err != nil || !bytes.Contains(raw, []byte(`model_provider = "other"`)) || bytes.Contains(raw, []byte("[model_providers.switcher]")) {
		t.Fatalf("restore did not preserve the user's provider: %v", err)
	}
	legacy := `model_provider = "switcher"

[model_providers.switcher]
name = "Switcher"
base_url = "http://127.0.0.1:9123/codex/v1"
wire_api = "responses"
requires_openai_auth = true
`
	if err := os.WriteFile(configPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/restore", true, true); w.Code != http.StatusConflict {
		t.Fatalf("untracked legacy setup was silently restored: %d", w.Code)
	}
	if w := request(http.MethodPost, "/api/cli-setup/codex/remove-legacy", true, true); w.Code != http.StatusOK {
		t.Fatalf("explicit legacy removal: %d %s", w.Code, w.Body.String())
	}
	if raw, err := os.ReadFile(configPath); err != nil || bytes.Contains(raw, []byte("switcher")) {
		t.Fatalf("explicit legacy removal left Switcher setup: %v", err)
	}
}
