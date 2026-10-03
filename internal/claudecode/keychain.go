package claudecode

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

type Keychain interface {
	Read(context.Context, string) ([]byte, bool, error)
	Write(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

type SystemKeychain struct {
	// Only tests replace Runner; production pins Apple's system executable.
	Runner func(context.Context, []string, []byte) ([]byte, error)
}

func keychainUser() string {
	if value := os.Getenv("USER"); value != "" {
		return value
	}
	if current, err := user.Current(); err == nil {
		return current.Username
	}
	return "claude-code-user"
}

func (k SystemKeychain) run(ctx context.Context, args []string, input []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if k.Runner != nil {
		return k.Runner(ctx, args, input)
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdin = bytes.NewReader(input)
	return cmd.Output()
}

func missingKey(err error) bool {
	var exit interface{ ExitCode() int }
	return errors.As(err, &exit) && exit.ExitCode() == 44
}

func (k SystemKeychain) Read(ctx context.Context, service string) ([]byte, bool, error) {
	value, err := k.run(ctx, []string{"find-generic-password", "-a", keychainUser(), "-s", service, "-w"}, nil)
	if missingKey(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("Claude Code Keychain is locked, denied, or unavailable; no credential fallback was used")
	}
	return bytes.TrimSuffix(value, []byte{'\n'}), true, nil
}

func securityQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func keychainWriteLine(service string, value []byte) (string, error) {
	if strings.ContainsAny(service+keychainUser(), "\n\r\x00") {
		return "", errors.New("invalid native Keychain item identifier")
	}
	line := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n", securityQuote(keychainUser()), securityQuote(service), hex.EncodeToString(value))
	// Unlike upstream's long-payload fallback, never put OAuth/MCP secrets in
	// process arguments. Refuse before changing anything if security -i would
	// truncate its fixed-size line buffer.
	if len(line) > 4032 {
		return "", errors.New("native credentials exceed the secure Keychain stdin limit; use Claude Code login for this profile")
	}
	return line, nil
}

func (k SystemKeychain) Validate(service string, value []byte) error {
	_, err := keychainWriteLine(service, value)
	return err
}

func (k SystemKeychain) Write(ctx context.Context, service string, value []byte) error {
	line, err := keychainWriteLine(service, value)
	if err != nil {
		return err
	}
	_, err = k.run(ctx, []string{"-i"}, []byte(line))
	if err != nil {
		return errors.New("could not write Claude Code Keychain credentials")
	}
	got, exists, err := k.Read(ctx, service)
	if err != nil || !exists || !bytes.Equal(got, value) {
		return errors.New("Claude Code Keychain write did not verify")
	}
	return nil
}

func (k SystemKeychain) Delete(ctx context.Context, service string) error {
	_, err := k.run(ctx, []string{"delete-generic-password", "-a", keychainUser(), "-s", service}, nil)
	if err != nil && !missingKey(err) {
		return errors.New("could not clear conflicting native Claude API key")
	}
	return nil
}
