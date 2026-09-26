package config

import (
	"path/filepath"
	"testing"
)

func TestCodexConfigPathRespectsCodexHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if got, want := CodexConfigPath(), filepath.Join(root, "config.toml"); got != want {
		t.Fatalf("Codex config path = %q, want %q", got, want)
	}
}
