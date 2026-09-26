// Package codexcfg installs and removes Switcher's model provider entry in
// the Codex CLI configuration file (~/.codex/config.toml). The edits are
// deliberately line-based and idempotent: installing twice must produce the
// same file, and uninstalling must leave the file without any Switcher
// trace.
package codexcfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Block is the provider definition appended to the config file.
const (
	providerID  = "switcher"
	blockHeader = "[model_providers." + providerID + "]"
	block       = blockHeader + `
name = "Switcher"
base_url = "http://127.0.0.1:8787/codex/v1"
wire_api = "responses"
requires_openai_auth = true
`
	providerLine = `model_provider = "switcher"`
)

// Install points the codex CLI at Switcher. It is safe to run repeatedly.
func Install(path string) error {
	return InstallAt(path, 8787)
}

// InstallAt configures Codex for the running loopback listener.
func InstallAt(path string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid listener port")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create codex directory: %w", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return writeAtomic(path, []byte(providerLine+"\n\n"+blockForPort(port)))
		}
		return fmt.Errorf("read codex config: %w", err)
	}
	if err := validateTOML(original); err != nil {
		return fmt.Errorf("codex config is invalid; fix it before connecting: %w", err)
	}
	out := applyAt(original, port)
	if err := validateTOML([]byte(out)); err != nil {
		return fmt.Errorf("updated codex config would be invalid: %w", err)
	}
	if err := backupOnce(path, original); err != nil {
		return fmt.Errorf("backup codex config: %w", err)
	}
	if err := writeAtomic(path, []byte(out)); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	return nil
}

func blockForPort(port int) string {
	return strings.Replace(block, ":8787/codex/v1", ":"+strconv.Itoa(port)+"/codex/v1", 1)
}

// Uninstall removes Switcher from the codex config. When Switcher was the
// selected provider, the model_provider line is dropped so codex falls back
// to its built-in OpenAI provider.
func Uninstall(path string) error {
	original, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read codex config: %w", err)
	}
	out := remove(original)
	if out == string(original) {
		return nil
	}
	if err := backupOnce(path, original); err != nil {
		return fmt.Errorf("backup codex config: %w", err)
	}
	return writeAtomic(path, []byte(out))
}

// apply performs the full install transformation.
func apply(original []byte) string {
	return applyAt(original, 8787)
}

func applyAt(original []byte, port int) string {
	lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")

	// 1. Drop any existing Switcher provider block.
	lines = dropBlock(lines)

	// 2. Point model_provider at switcher.
	modelProviderIdx := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			break
		}
		if isTopLevelKey(l, "model_provider") {
			modelProviderIdx = i
			break
		}
	}
	if modelProviderIdx >= 0 {
		lines[modelProviderIdx] = providerLine
	} else {
		lines = insertTopLevel(lines, providerLine)
	}

	// 3. Append the provider definition block.
	// Trim trailing blank lines so repeated installs stay byte-stable.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	out := strings.Join(lines, "\n") + "\n\n" + blockForPort(port)
	return out
}

// remove performs the uninstall transformation.
func remove(original []byte) string {
	lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")
	lines = dropBlock(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			break
		}
		if isTopLevelKey(l, "model_provider") && strings.TrimSpace(l) == providerLine {
			lines = append(lines[:i], lines[i+1:]...)
			break
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// Status is a read-only diagnosis of Codex's selected provider. Only ready
// means the selected provider points at this Switcher listener.
type Status struct {
	Condition   string `json:"condition"`
	ExpectedURL string `json:"expected_url"`
}

func Check(path string, port int) Status {
	expected := fmt.Sprintf("http://127.0.0.1:%d/codex/v1", port)
	status := Status{Condition: "missing", ExpectedURL: expected}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return status
	}
	if err != nil {
		status.Condition = "unreadable"
		return status
	}
	var values map[string]any
	if err := toml.Unmarshal(raw, &values); err != nil {
		status.Condition = "invalid"
		return status
	}
	selected, _ := values["model_provider"].(string)
	providers, _ := values["model_providers"].(map[string]any)
	block, _ := providers["switcher"].(map[string]any)
	if selected != "switcher" {
		if block != nil {
			status.Condition = "not_selected"
		} else if selected != "" {
			status.Condition = "other_provider"
		}
		return status
	}
	if block["base_url"] != expected || block["wire_api"] != "responses" || block["requires_openai_auth"] != true {
		status.Condition = "misconfigured"
		return status
	}
	status.Condition = "ready"
	return status
}

func validateTOML(raw []byte) error {
	var config map[string]any
	return toml.Unmarshal(raw, &config)
}

func backupOnce(path string, data []byte) error {
	backup := path + ".switcher-backup"
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(backup)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".switcher-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// dropBlock removes an existing [model_providers.switcher] section,
// including every key that belongs to it.
func dropBlock(lines []string) []string {
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == blockHeader {
			start = i
			break
		}
	}
	if start < 0 {
		return lines
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		section := strings.TrimSpace(lines[i])
		if strings.HasPrefix(section, "[") &&
			!strings.HasPrefix(section, "[model_providers.switcher.") &&
			!strings.HasPrefix(section, "[[model_providers.switcher.") {
			end = i
			break
		}
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, lines[end:]...)
	// Trim any doubled blank line left behind.
	cleaned := out[:0]
	prevBlank := false
	for _, l := range out {
		blank := strings.TrimSpace(l) == ""
		if blank && prevBlank {
			continue
		}
		cleaned = append(cleaned, l)
		prevBlank = blank
	}
	return cleaned
}

// isTopLevelKey matches `key = value` lines that sit outside any table.
// Line-based TOML parsing cannot know sections perfectly, so this accepts
// any unindented `model_provider = ...` line, which matches how the codex
// CLI writes its config.
func isTopLevelKey(line, key string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "#") {
		return false
	}
	return strings.HasPrefix(trimmed, key+" =") || strings.HasPrefix(trimmed, key+"=")
}

// insertTopLevel inserts a top-level key line before the first table
// header, since bare keys after a table header belong to that table.
func insertTopLevel(lines []string, entry string) []string {
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			out := append([]string{}, lines[:i]...)
			out = append(out, entry, "")
			return append(out, lines[i:]...)
		}
	}
	return append(lines, entry)
}
