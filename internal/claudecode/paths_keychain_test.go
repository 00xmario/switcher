package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"switcher/internal/store"
	"testing"
)

func TestProfilePathResolutionMatchesClaudeSwap(t *testing.T) {
	home := t.TempDir()
	lookup := func(string) (string, bool) { return "", false }
	p, err := pathsFor(home, "", lookup, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != filepath.Join(home, ".claude.json") || p.Service != "Claude Code-credentials" || !p.Keychain {
		t.Fatal("default profile paths diverged")
	}
	custom := filepath.Join(home, "profile") + "/./"
	p, err = pathsFor(home, custom, lookup, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(custom))
	if p.Service != "Claude Code-credentials-"+hex.EncodeToString(digest[:4]) {
		t.Fatal("service hash normalized the raw path")
	}
	if p.ConfigFile != filepath.Join(filepath.Clean(custom), ".claude.json") {
		t.Fatal("custom config path wrong")
	}
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(home, ".claude", ".config.json")
	if err := os.WriteFile(legacy, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = pathsFor(home, "", lookup, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigFile != legacy || p.Keychain {
		t.Fatal("legacy config/file backend not selected")
	}
	if _, err := pathsFor(home, "relative", lookup, "darwin"); err == nil {
		t.Fatal("relative config profile accepted")
	}
	secure := func(string) (string, bool) { return "other-profile", true }
	if _, err := pathsFor(home, custom, secure, "darwin"); err == nil {
		t.Fatal("divergent secure profile accepted")
	}
	decomposed := filepath.Join(home, "e\u0301")
	composed := filepath.Join(home, "\u00e9")
	a, _ := pathsFor(home, decomposed, lookup, "darwin")
	b, _ := pathsFor(home, composed, lookup, "darwin")
	if a.Service != b.Service {
		t.Fatal("Keychain service derivation omitted NFC normalization")
	}
}

type exitCode int

func (e exitCode) Error() string { return "fixture command exit" }
func (e exitCode) ExitCode() int { return int(e) }

func TestKeychainWritesUseStdinAndVerify(t *testing.T) {
	t.Setenv("USER", "fixture-user")
	secret := []byte(`{"claudeAiOauth":{"accessToken":"fixture-sensitive","refreshToken":"fixture-grant"}}`)
	var stored []byte
	k := SystemKeychain{Runner: func(ctx context.Context, args []string, input []byte) ([]byte, error) {
		for _, arg := range args {
			if strings.Contains(arg, "fixture-sensitive") || strings.Contains(arg, hex.EncodeToString(secret)) {
				t.Fatal("credential leaked into process arguments")
			}
		}
		if len(args) == 1 && args[0] == "-i" {
			line := string(input)
			encodedValue := strings.Fields(line)[len(strings.Fields(line))-1]
			var err error
			stored, err = hex.DecodeString(encodedValue)
			if err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		return append(append([]byte(nil), stored...), '\n'), nil
	}}
	if err := k.Write(context.Background(), "Claude Code-credentials", secret); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(secret) {
		t.Fatal("round-trip corrupted credential")
	}
}

func TestKeychainAbsenceIsNotAReadFailure(t *testing.T) {
	for _, tc := range []struct {
		err     error
		wantErr bool
	}{{exitCode(44), false}, {exitCode(1), true}, {errors.New("fixture timeout"), true}} {
		k := SystemKeychain{Runner: func(context.Context, []string, []byte) ([]byte, error) { return nil, tc.err }}
		_, exists, err := k.Read(context.Background(), "Claude Code-credentials")
		if exists || (err != nil) != tc.wantErr {
			t.Fatalf("Keychain verdict incorrect: exists=%v err=%v", exists, err)
		}
	}
}

func TestOversizedKeychainPayloadNeverFallsBackToSecretArgv(t *testing.T) {
	calls := 0
	k := SystemKeychain{Runner: func(context.Context, []string, []byte) ([]byte, error) { calls++; return nil, nil }}
	if err := k.Write(context.Background(), "Claude Code-credentials", []byte(strings.Repeat("a", 3000))); err == nil {
		t.Fatal("oversized stdin payload accepted")
	}
	if calls != 0 {
		t.Fatal("overflow attempted a credential write")
	}
}

func TestSameUserOrganizationsAreDistinctNativeIdentities(t *testing.T) {
	a := nativeAccount("a")
	b := a
	other := identityFor("a")
	other.OrganizationUUID = "different-org"
	b.ID = "claude-other-org"
	b.ClaudeCode = &store.ClaudeCodeLogin{Credentials: rawCredential("b"), OAuthAccount: encoded(other)}
	if owns(other, a) || !owns(other, b) {
		t.Fatal("organization identity collapsed to email/UUID alone")
	}
}
