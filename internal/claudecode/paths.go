// Package claudecode ports the native-login switch mechanism studied in
// realiti4/claude-swap. See THIRD_PARTY_NOTICES.md and docs/claude-swap-analysis.md.
package claudecode

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/text/unicode/norm"
)

type Paths struct {
	Home, ConfigHome, ConfigFile, CredentialsFile, Service string
	Keychain                                               bool
}

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return pathsFor(home, os.Getenv("CLAUDE_CONFIG_DIR"), os.LookupEnv, runtime.GOOS)
}

func pathsFor(home, configDir string, lookup func(string) (string, bool), platform string) (Paths, error) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if value, set := lookup(key); set && value != "" {
			return Paths{}, fmt.Errorf("%s overrides native Claude credentials; unset it before using native account switching", key)
		}
	}
	configHome := filepath.Join(home, ".claude")
	service := "Claude Code-credentials"
	if configDir != "" {
		if !filepath.IsAbs(configDir) {
			return Paths{}, fmt.Errorf("CLAUDE_CONFIG_DIR must be absolute")
		}
		configHome = filepath.Clean(configDir)
		digest := sha256.Sum256([]byte(norm.NFC.String(configDir)))
		service = fmt.Sprintf("Claude Code-credentials-%x", digest[:4])
	}
	if secure, set := lookup("CLAUDE_SECURESTORAGE_CONFIG_DIR"); set {
		// A split identity/secure-storage profile cannot safely be switched as
		// one login. Refuse rather than writing another profile's keychain.
		if (secure == "" && configDir != "") || (secure != "" && secure != configDir) {
			return Paths{}, fmt.Errorf("Claude config and secure-storage profiles differ; unset CLAUDE_SECURESTORAGE_CONFIG_DIR before native switching")
		}
		if secure == "" {
			service = "Claude Code-credentials"
		}
	}
	base := home
	if configDir != "" {
		base = configHome
	}
	configFile := filepath.Join(base, ".claude.json")
	legacy := filepath.Join(configHome, ".config.json")
	if _, err := os.Lstat(legacy); err == nil {
		configFile = legacy
	} else if !os.IsNotExist(err) {
		return Paths{}, err
	}
	return Paths{Home: home, ConfigHome: configHome, ConfigFile: configFile, CredentialsFile: filepath.Join(configHome, ".credentials.json"), Service: service, Keychain: platform == "darwin"}, nil
}
