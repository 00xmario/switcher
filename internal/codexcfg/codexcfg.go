// Package codexcfg installs and removes Switcher's model provider entry in
// the Codex CLI configuration file (~/.codex/config.toml). The edits are
// deliberately line-based and idempotent: installing twice must produce the
// same file, and uninstalling must leave the file without any Switcher
// trace.
package codexcfg

import (
	"fmt"
	"os"
	"strings"
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
	original, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(path, []byte(providerLine+"\n\n"+block), 0o600)
		}
		return fmt.Errorf("read codex config: %w", err)
	}
	if err := os.WriteFile(path+".switcher-backup", original, 0o600); err != nil {
		return fmt.Errorf("backup codex config: %w", err)
	}
	out := apply(original)
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	return nil
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
	if err := os.WriteFile(path+".switcher-backup", original, 0o600); err != nil {
		return fmt.Errorf("backup codex config: %w", err)
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// apply performs the full install transformation.
func apply(original []byte) string {
	lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")

	// 1. Drop any existing Switcher provider block.
	lines = dropBlock(lines)

	// 2. Point model_provider at switcher.
	modelProviderIdx := -1
	for i, l := range lines {
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
	out := strings.Join(lines, "\n") + "\n\n" + block
	return out
}

// remove performs the uninstall transformation.
func remove(original []byte) string {
	lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")
	lines = dropBlock(lines)
	for i, l := range lines {
		if isTopLevelKey(l, "model_provider") && strings.TrimSpace(l) == providerLine {
			lines = append(lines[:i], lines[i+1:]...)
			break
		}
	}
	return strings.Join(lines, "\n") + "\n"
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
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
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
