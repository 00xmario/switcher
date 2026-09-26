package codexcfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestInstallIntoEmptyConfig(t *testing.T) {
	path := writeFile(t, "")
	if err := Install(path); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	if !strings.Contains(out, `model_provider = "switcher"`) {
		t.Errorf("missing model_provider line:\n%s", out)
	}
	if !strings.Contains(out, blockHeader) {
		t.Errorf("missing provider block:\n%s", out)
	}
}

func TestInstallReplacesExistingProvider(t *testing.T) {
	path := writeFile(t, `model_provider = "openai"`+"\n")
	if err := Install(path); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	if strings.Contains(out, `model_provider = "openai"`) {
		t.Errorf("old provider selection survived:\n%s", out)
	}
	if !strings.Contains(out, `model_provider = "switcher"`) {
		t.Errorf("missing model_provider line:\n%s", out)
	}
}

func TestInstallInsertsBeforeFirstTable(t *testing.T) {
	path := writeFile(t, "model_reasoning_effort = \"high\"\n\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n")
	if err := Install(path); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	providerIdx := strings.Index(out, providerLine)
	tableIdx := strings.Index(out, "[projects.")
	if providerIdx < 0 || tableIdx < 0 || providerIdx > tableIdx {
		t.Errorf("model_provider must be inserted before the first table:\n%s", out)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	path := writeFile(t, `model_provider = "switcher"`+"\n\n"+block)
	before := read(t, path)
	if err := Install(path); err != nil {
		t.Fatal(err)
	}
	after := read(t, path)
	if strings.Count(after, blockHeader) != 1 {
		t.Errorf("install duplicated the block:\n%s", after)
	}
	if strings.Count(after, providerLine) != 1 {
		t.Errorf("install duplicated model_provider:\n%s", after)
	}
	if strings.TrimRight(before, "\n") != strings.TrimRight(after, "\n") {
		t.Errorf("idempotency violated:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	path := writeFile(t, "model_reasoning_effort = \"high\"\n\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n")
	if err := Install(path); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	out := read(t, path)
	if strings.Contains(out, "switcher") || strings.Contains(out, "127.0.0.1:8787") {
		t.Errorf("switcher residue left behind:\n%s", out)
	}
	if !strings.Contains(out, "[projects.\"/tmp\"]") {
		t.Errorf("unrelated content was damaged:\n%s", out)
	}
}

func TestUninstallIsANoOpWithoutSwitcher(t *testing.T) {
	path := writeFile(t, "model_reasoning_effort = \"high\"\n")
	if err := Uninstall(path); err != nil {
		t.Fatal(err)
	}
	if out := read(t, path); !strings.Contains(out, "model_reasoning_effort") {
		t.Errorf("uninstall changed an unrelated file:\n%s", out)
	}
}

func TestCheckStatusesAndRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.toml")
	if got := Check(path, 9123).Condition; got != "missing" {
		t.Fatalf("missing: %s", got)
	}
	if err := InstallAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	if got := Check(path, 9123).Condition; got != "ready" {
		t.Fatalf("new install: %s", got)
	}
	if got := Check(path, 8787).Condition; got != "misconfigured" {
		t.Fatalf("wrong listener: %s", got)
	}
	for _, tc := range []struct{ content, want string }{
		{`model_provider = "other"`, "other_provider"},
		{`[projects."/tmp"]` + "\n" + `model_provider = "switcher"`, "missing"},
		{`model_provider = "other"` + "\n\n" + block, "not_selected"},
		{`model_provider = "switcher"` + "\n\n" + block, "misconfigured"},
		{`model_provider = "switcher"` + "\n\n" + strings.Replace(block, `wire_api = "responses"`, `wire_api = "chat"`, 1), "conflict"},
		{`model_provider = "switcher"` + "\n\n" + block + "\n" + `base_url = "duplicate"`, "invalid"},
		{`model_provider = "switcher"` + "\n\n[broken", "invalid"},
	} {
		casePath := writeFile(t, tc.content)
		if got := Check(casePath, 9123).Condition; got != tc.want {
			t.Fatalf("check %q = %s, want %s", tc.content, got, tc.want)
		}
	}
	nestedPath := writeFile(t, "[projects.\"/tmp\"]\nmodel_provider = \"other\"\n")
	if err := InstallAt(nestedPath, 9123); err != nil {
		t.Fatal(err)
	}
	if got := Check(nestedPath, 9123).Condition; got != "ready" {
		t.Fatalf("nested key repair: %s", got)
	}
	if !strings.Contains(read(t, nestedPath), `[projects."/tmp"]`+"\n"+`model_provider = "other"`) {
		t.Fatal("installer overwrote nested model_provider")
	}
}

func TestInstallRejectsInvalidConfigAndKeepsOriginalBackup(t *testing.T) {
	path := writeFile(t, "model = [invalid\n")
	before := read(t, path)
	if got := Check(path, 8787).Condition; got != "invalid" {
		t.Fatalf("invalid TOML status: %s", got)
	}
	if err := InstallAt(path, 8787); err == nil {
		t.Fatal("invalid input accepted")
	}
	if read(t, path) != before {
		t.Fatal("invalid file changed")
	}
	if _, err := os.Stat(path + ".switcher-backup"); !os.IsNotExist(err) {
		t.Fatal("invalid file backed up as valid")
	}
	valid := "model = \"gpt-5.5\"\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallAt(path, 8787); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path+".switcher-backup"); got != valid {
		t.Fatalf("backup = %q", got)
	}
	if err := InstallAt(path, 9123); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path+".switcher-backup"); got != valid {
		t.Fatal("repeat repair overwrote initial backup")
	}
	if got := Check(path, 9123).Condition; got != "ready" {
		t.Fatalf("repaired config: %s", got)
	}
}

func TestInstallRejectsNestedSwitcherProviderTables(t *testing.T) {
	path := writeFile(t, `model_provider = "switcher"`+"\n\n"+block+
		"\n[model_providers.switcher.http_headers]\nX-Old = \"value\"\n\n[projects.\"/tmp\"]\ntrust_level = \"trusted\"\n")
	before := read(t, path)
	if err := InstallAt(path, 9123); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("nested Switcher table should be left untouched: %v", err)
	}
	if out := read(t, path); out != before {
		t.Fatal("conflicting Switcher table was modified")
	}
}
