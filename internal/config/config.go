// Package config resolves Switcher's filesystem layout and default settings.
package config

import (
	"os"
	"path/filepath"
)

// Defaults for the HTTP server and the OAuth callback listener.
const (
	// DefaultPort is the port the Switcher server (UI + proxy) listens on.
	DefaultPort = 8787
	// CallbackPort must match the redirect URI registered for the Codex
	// OAuth client; changing it breaks the login flow.
	CallbackPort = 1455
	// ClaudeCallbackPort must match the redirect URI registered for the
	// Claude Code OAuth client.
	ClaudeCallbackPort = 54545
	// AntigravityCallbackPort must match the redirect URI registered for
	// the Antigravity Google OAuth client.
	AntigravityCallbackPort = 51121
	// GeminiCallbackPort must match the redirect URI registered for the
	// Gemini CLI Google OAuth client.
	GeminiCallbackPort = 51122
)

// Dir returns the Switcher data directory, creating it if needed.
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory is unrecoverable for a tool whose entire
		// purpose is storing per-user credentials.
		panic("switcher: cannot determine home directory: " + err.Error())
	}
	dir := filepath.Join(home, ".switcher")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// CodexConfigPath returns the user's codex CLI configuration file.
func CodexConfigPath() string {
	if root := os.Getenv("CODEX_HOME"); root != "" {
		return filepath.Join(root, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		panic("switcher: cannot determine home directory: " + err.Error())
	}
	return filepath.Join(home, ".codex", "config.toml")
}
