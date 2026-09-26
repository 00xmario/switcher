// Package codexcfg installs and removes Switcher's model provider entry in
// the Codex CLI configuration while preserving the preceding provider
// selection and unrelated settings. Its journal makes interrupted edits
// recoverable without restoring an old full-file backup.
package codexcfg

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

var (
	// ErrConfigConflict means Switcher cannot prove that an edit is safe.
	ErrConfigConflict = errors.New("Codex configuration conflict")
	// ErrInvalidConfig means the existing TOML needs manual repair first.
	ErrInvalidConfig = errors.New("invalid Codex configuration")
	// ErrLegacyOwnershipUnknown means an untracked Switcher-looking block
	// cannot be removed without the explicit legacy-removal action.
	ErrLegacyOwnershipUnknown = errors.New("legacy Switcher configuration has no ownership record")
	// ErrSelectionChanged means a user switched away from Switcher after setup.
	ErrSelectionChanged = errors.New("Codex provider selection changed")
)

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

// Install configures the default Switcher listener in the Codex user file.
func Install(path string) error { return InstallAt(path, 8787) }

// InstallAt configures the given listener, but never silently takes over a
// selection the user changed after an earlier Switcher installation.
func InstallAt(path string, port int) error { return installAt(path, port, false) }

// Uninstall restores the recorded provider selection when ownership is
// tracked. An untracked legacy block requires UninstallLegacy explicitly.
func Uninstall(path string) error { return uninstallAt(path, false) }

// UninstallLegacy removes a recognized older Switcher block. Its former
// provider selection cannot be recovered from a legacy configuration.
func UninstallLegacy(path string) error { return uninstallAt(path, true) }

// ReselectAt explicitly makes Switcher active after a user chose another
// provider. Ordinary InstallAt never takes over that selection silently.
func ReselectAt(path string, port int) error { return installAt(path, port, true) }

func blockForPort(port int) string {
	return strings.Replace(block, ":8787/codex/v1", ":"+strconv.Itoa(port)+"/codex/v1", 1)
}

// Status inspects a user file, not a native Codex login or live request.
type Status struct {
	Condition        string `json:"condition"`
	ExpectedURL      string `json:"expected_url"`
	Ownership        string `json:"ownership,omitempty"`
	InstallAction    string `json:"install_action,omitempty"`
	RestoreAction    string `json:"restore_action,omitempty"`
	PendingOperation string `json:"pending_operation,omitempty"`
}

// Check is read-only. Interrupted transitions are reported as pending until
// a later explicit InstallAt or Uninstall reconciles the journal.
func Check(path string, port int) Status {
	expected := fmt.Sprintf("http://127.0.0.1:%d/codex/v1", port)
	return checkAt(path, port, expected)
}

func validateTOML(raw []byte) error {
	var config map[string]any
	return toml.Unmarshal(raw, &config)
}
