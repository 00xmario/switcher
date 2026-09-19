package codexcfg

import (
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
