package desktoprelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"switcher/internal/desktoprelay"
)

func setupFixture(t *testing.T) (*desktoprelay.Manager, desktoprelay.Config, string) {
	t.Helper()
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m, cfg, filepath.Join(filepath.Dir(cfg.DataRoot), "native", "settings.json")
}

func TestSetupRestoreRetainsExternalSettingsAndOriginalBaseline(t *testing.T) {
	m, _, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"model":"before","env":{"HTTPS_PROXY":"https://corporate.example","NODE_EXTRA_CA_CERTS":null,"KEEP":"before"}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.DeleteScope(s.ScopeID); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatal("managed scope deletion did not require restore")
	}
	var own *desktoprelay.SetupError
	if !errors.As(err, &own) || own.ErrorCode() != "setup_owned" {
		t.Fatal("missing safe owned-setup error")
	}
	if err = m.Stop(context.Background()); !errors.Is(err, desktoprelay.ErrConflict) || !m.Status().Listening {
		t.Fatal("stop stranded managed configuration")
	}
	b := setupRead(t, path)
	b = bytes.Replace(b, []byte(`"model":"before"`), []byte(`"model":"external","future":1e+99`), 1)
	b = bytes.Replace(b, []byte(`"KEEP":"before"`), []byte(`"KEEP":"external","ADDED":"keep"`), 1)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := m.RestoreSetup(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Condition != "not_configured" || !r.RestartRequired || r.Configured || !m.Status().Listening {
		t.Fatalf("restore: %+v", r)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(setupRead(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(doc["env"], &env); err != nil {
		t.Fatal(err)
	}
	if string(env["HTTPS_PROXY"]) != `"https://corporate.example"` || string(env["NODE_EXTRA_CA_CERTS"]) != "null" || string(env["KEEP"]) != `"external"` || string(env["ADDED"]) != `"keep"` || string(doc["model"]) != `"external"` || string(doc["future"]) != "1e+99" {
		t.Fatal("restore overwrote unrelated edits or lost baseline")
	}
	if !bytes.Equal(original, setupRead(t, s.BackupPath)) {
		t.Fatal("restore modified private backup")
	}
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if err = m.DeleteScope(s.ScopeID); err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func setupRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func setupEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	var doc struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(setupRead(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Env
}

func TestSetupOneClickMergesAndReusesPrivateScope(t *testing.T) {
	m, cfg, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"permissions":{"allow":["Read"]},"model":"custom","hooks":{"x":[]},"mcpServers":{},"unknown":900719925474099312345,"raw":{"a" : [ 1e+09, 2 ]},"env":{"KEEP":"value","HTTPS_PROXY":"https://foreign.example","NODE_EXTRA_CA_CERTS":"foreign.pem"}}`)
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	s, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Condition != "configured" || !s.Configured || !s.RestartRequired || s.ScopeID == "" || !m.Status().Listening {
		t.Fatalf("setup did not configure: %+v", s)
	}
	if got := setupRead(t, s.BackupPath); !bytes.Equal(got, original) {
		t.Fatal("backup changed original bytes")
	}
	for _, p := range []string{path, s.BackupPath} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file mode: %v", err)
		}
	}
	info, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0755 {
		t.Fatal("changed native directory permissions")
	}
	b := setupRead(t, path)
	if !bytes.Contains(b, []byte(`900719925474099312345`)) || !bytes.Contains(b, []byte(`{"a" : [ 1e+09, 2 ]}`)) {
		t.Fatal("unrelated raw values changed")
	}
	env := setupEnv(t, path)
	if env["KEEP"] != "value" || !strings.Contains(env["HTTPS_PROXY"], "@"+m.Status().Address) || env["NODE_EXTRA_CA_CERTS"] != filepath.Join(cfg.DataRoot, "ca.pem") {
		t.Fatal("incorrect env merge")
	}
	public, _ := json.Marshal(s)
	if bytes.Contains(public, []byte(env["HTTPS_PROXY"])) || bytes.Contains(public, []byte("foreign.example")) {
		t.Fatal("setup exposed a capability or baseline")
	}
	again, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ScopeID != s.ScopeID || again.BackupPath != s.BackupPath || len(m.Scopes()) != 1 || !bytes.Equal(b, setupRead(t, path)) {
		t.Fatal("repeat setup replaced baseline, scope, or settings")
	}
	if m.SetupStatus(path).Condition != "configured" {
		t.Fatal("read-only status disagrees")
	}
}

func TestSetupRestoreEnvShapesMissingFileAndDeletedFile(t *testing.T) {
	for _, tc := range []struct{ name, initial, expected string }{
		{"null", `{"env":null}`, `{"env":null}`},
		{"absent", `{"model":"keep"}`, `{"model":"keep"}`},
		{"empty", `{"env":{}}`, `{"env":{}}`},
		{"missing", "", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, path := setupFixture(t)
			if tc.initial != "" {
				if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.initial), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, err := m.Configure(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.RestoreSetup(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(setupRead(t, path))) != tc.expected {
				t.Fatal("restore lost original env shape")
			}
			if s.BackupPath == "" || string(setupRead(t, s.BackupPath)) != tc.initial {
				t.Fatal("baseline backup missing")
			}
			if _, err = m.Configure(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err = m.RestoreSetup(context.Background(), path); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("restore resurrected externally deleted settings")
			}
		})
	}
}

func TestSetupCreatedFileRestoreKeepsNewSettingsAndOtherProfiles(t *testing.T) {
	m, _, path := setupFixture(t)
	s, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.CreateScope("other profile")
	if err != nil {
		t.Fatal(err)
	}
	b := bytes.Replace(setupRead(t, path), []byte(`{"env":`), []byte(`{"permissions":{"allow":["Edit"]},"env":`), 1)
	b = bytes.Replace(b, []byte(`"HTTPS_PROXY":`), []byte(`"NEW":"value","HTTPS_PROXY":`), 1)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(setupRead(t, path))); got != `{"env":{"NEW":"value"},"permissions":{"allow":["Edit"]}}` {
		t.Fatal("created settings were blindly removed")
	}
	if err = m.DeleteScope(s.ScopeID); err != nil {
		t.Fatal(err)
	}
	if len(m.Scopes()) != 1 || m.Scopes()[0].ID != other.ID || !m.Status().Listening {
		t.Fatal("restore disrupted another profile")
	}
}

func TestSetupForeignOwnedChangeIsNeverClobbered(t *testing.T) {
	for _, key := range []string{"HTTPS_PROXY", "NODE_EXTRA_CA_CERTS"} {
		t.Run(key, func(t *testing.T) {
			m, _, path := setupFixture(t)
			s, err := m.Configure(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			env := setupEnv(t, path)
			old, _ := json.Marshal(env[key])
			b := bytes.Replace(setupRead(t, path), old, []byte(`"foreign-token-not-for-status"`), 1)
			if err = os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if got := m.SetupStatus(path); got.Condition != "changed" || got.Configured {
				t.Fatal("foreign change not detected")
			}
			for _, operation := range []func(context.Context, string) (desktoprelay.SetupStatus, error){m.Configure, m.RestoreSetup} {
				got, err := operation(context.Background(), path)
				if !errors.Is(err, desktoprelay.ErrConflict) || !bytes.Equal(b, setupRead(t, path)) {
					t.Fatal("foreign owned value clobbered")
				}
				public, _ := json.Marshal(got)
				if bytes.Contains(public, []byte("foreign-token")) || strings.Contains(err.Error(), "foreign-token") {
					t.Fatal("foreign env leaked")
				}
			}
			if len(m.Scopes()) != 1 || m.Scopes()[0].ID != s.ScopeID {
				t.Fatal("conflict leaked scopes")
			}
		})
	}
}

func TestSetupKeepsLocalServersOffTheRelayAndRestoresNoProxy(t *testing.T) {
	m, _, path := setupFixture(t)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"env":{"NO_PROXY":"corp.example"},"theme":"dark"}` + "\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Configure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if got := setupEnv(t, path)["NO_PROXY"]; got != "corp.example,localhost,127.0.0.1,::1" {
		t.Fatalf("NO_PROXY after connect = %q", got)
	}
	if _, err := m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	var before, after any
	json.Unmarshal(original, &before)
	json.Unmarshal(setupRead(t, path), &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("disconnect left %s", setupRead(t, path))
	}
}
