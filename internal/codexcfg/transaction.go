package codexcfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const journalName = "config.toml.switcher-state.json"
const journalVersion = 1

type journal struct {
	Version        int    `json:"version"`
	Path           string `json:"path"`
	Phase          string `json:"phase"`     // prepared or active
	Operation      string `json:"operation"` // install, update, restore, or reselect
	SourceDigest   string `json:"source_digest,omitempty"`
	TargetDigest   string `json:"target_digest,omitempty"`
	SourceExists   bool   `json:"source_exists,omitempty"`
	TargetExists   bool   `json:"target_exists,omitempty"`
	OriginalLine   string `json:"original_line,omitempty"`
	OwnedLine      string `json:"owned_line"`
	InsertedPrefix string `json:"inserted_prefix,omitempty"`
	BlockSeparator string `json:"block_separator,omitempty"`
	OwnedBlock     string `json:"owned_block"`
	InstalledPort  int    `json:"installed_port"`
	TargetPort     int    `json:"target_port,omitempty"`
	CreatedConfig  bool   `json:"created_config,omitempty"`
}

type textSpan struct{ start, end int }

func (s textSpan) valid() bool { return s.end > s.start }

type tomlLayout struct {
	selection  textSpan
	block      textSpan
	firstTable int
	selected   string
	blockPort  int
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseLayout(raw []byte) (tomlLayout, error) {
	var layout tomlLayout
	layout.firstTable = len(raw)
	if bytes.Contains(raw, []byte(`"""`)) || bytes.Contains(raw, []byte(`'''`)) {
		return layout, fmt.Errorf("%w: multiline TOML requires manual configuration", ErrConfigConflict)
	}
	if err := validateTOML(raw); err != nil {
		return layout, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	// The validated TOML is also decoded by Check. Source spans below are
	// conservative: when a valid TOML form is not unambiguously editable,
	// return a conflict instead of rewriting a user's config.
	section := ""
	for start := 0; start < len(raw); {
		end := start + bytes.IndexByte(raw[start:], '\n') + 1
		if end <= start {
			end = len(raw)
		}
		trimmed := strings.TrimSpace(string(raw[start:end]))
		if strings.HasPrefix(trimmed, "[") {
			closing := strings.Index(trimmed, "]")
			if closing < 0 {
				return layout, fmt.Errorf("%w: unsupported table header", ErrConfigConflict)
			}
			header := trimmed[:closing+1]
			if strings.HasPrefix(header, "[[model_providers.switcher") || strings.HasPrefix(header, "[model_providers.switcher.") {
				return layout, fmt.Errorf("%w: nested Switcher table is not owned by this setup", ErrConfigConflict)
			}
			if layout.block.valid() && layout.block.end == len(raw) {
				layout.block.end = start
			}
			if layout.firstTable == len(raw) {
				layout.firstTable = start
			}
			section = strings.Trim(header, "[]")
			if header == blockHeader {
				if layout.block.valid() {
					return layout, fmt.Errorf("%w: duplicate Switcher table", ErrConfigConflict)
				}
				layout.block = textSpan{start, len(raw)}
			}
		} else if section == "" && strings.HasPrefix(trimmed, "model_provider") {
			key, _, ok := strings.Cut(trimmed, "=")
			if ok && strings.TrimSpace(key) == "model_provider" {
				if layout.selection.valid() {
					return layout, fmt.Errorf("%w: duplicate model_provider", ErrConfigConflict)
				}
				layout.selection = textSpan{start, end}
			}
		}
		start = end
	}
	var parsed map[string]any
	if err := toml.Unmarshal(raw, &parsed); err != nil {
		return layout, err
	}
	layout.selected, _ = parsed["model_provider"].(string)
	if _, exists := parsed["model_provider"]; exists && !layout.selection.valid() {
		return layout, fmt.Errorf("%w: quoted or dotted provider selection is unsupported", ErrConfigConflict)
	}
	providers, _ := parsed["model_providers"].(map[string]any)
	if block, ok := providers["switcher"].(map[string]any); ok {
		if !layout.block.valid() {
			return layout, fmt.Errorf("%w: inline Switcher table is unsupported", ErrConfigConflict)
		}
		base, _ := block["base_url"].(string)
		if u, err := url.Parse(base); err == nil && u.Scheme == "http" && u.Hostname() == "127.0.0.1" && u.Path == "/codex/v1" {
			layout.blockPort, _ = strconv.Atoi(u.Port())
		}
	}
	return layout, nil
}

func eol(raw []byte) string {
	if bytes.Contains(raw, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

func replaceSpan(raw []byte, span textSpan, replacement string) []byte {
	out := append([]byte(nil), raw[:span.start]...)
	out = append(out, replacement...)
	return append(out, raw[span.end:]...)
}

func generatedBlock(port int, newline string) string {
	return strings.ReplaceAll(blockForPort(port), "\n", newline)
}

func createInstall(raw []byte, exists bool, port int, path string) ([]byte, journal, error) {
	layout, err := parseLayout(raw)
	if err != nil {
		return nil, journal{}, err
	}
	if layout.block.valid() {
		return nil, journal{}, fmt.Errorf("%w: an untracked Switcher table already exists", ErrConfigConflict)
	}
	newline := eol(raw)
	j := journal{Version: journalVersion, Path: path, Phase: "active", OwnedLine: providerLine + newline,
		OriginalLine: "", OwnedBlock: generatedBlock(port, newline), InstalledPort: port, CreatedConfig: !exists}
	out := append([]byte(nil), raw...)
	if layout.selection.valid() {
		j.OriginalLine = string(raw[layout.selection.start:layout.selection.end])
		out = replaceSpan(out, layout.selection, j.OwnedLine)
	} else {
		j.InsertedPrefix = j.OwnedLine + newline
		out = append([]byte(j.InsertedPrefix), out...)
	}
	if len(out) > 0 && !bytes.HasSuffix(out, []byte(newline+newline)) {
		if bytes.HasSuffix(out, []byte(newline)) {
			j.BlockSeparator = newline
		} else {
			j.BlockSeparator = newline + newline
		}
	}
	out = append(out, j.BlockSeparator...)
	out = append(out, j.OwnedBlock...)
	if err := validateTOML(out); err != nil {
		return nil, journal{}, fmt.Errorf("%w: generated Codex config: %v", ErrConfigConflict, err)
	}
	return out, j, nil
}

func ownedBlockSpan(raw []byte, layout tomlLayout, j journal) (textSpan, error) {
	if !layout.block.valid() || !bytes.HasPrefix(raw[layout.block.start:layout.block.end], []byte(j.OwnedBlock)) ||
		strings.TrimSpace(string(raw[layout.block.start+len(j.OwnedBlock):layout.block.end])) != "" {
		return textSpan{}, fmt.Errorf("%w: Switcher provider block changed", ErrConfigConflict)
	}
	return textSpan{layout.block.start, layout.block.start + len(j.OwnedBlock)}, nil
}

func updateInstall(raw []byte, j journal, port int, reselect bool) ([]byte, journal, error) {
	layout, err := parseLayout(raw)
	if err != nil {
		return nil, j, err
	}
	blockSpan, err := ownedBlockSpan(raw, layout, j)
	if err != nil {
		return nil, j, err
	}
	if layout.selected != "switcher" || !layout.selection.valid() || string(raw[layout.selection.start:layout.selection.end]) != j.OwnedLine {
		if !reselect {
			return nil, j, fmt.Errorf("%w: %w", ErrConfigConflict, ErrSelectionChanged)
		}
		if !layout.selection.valid() {
			return nil, j, fmt.Errorf("%w: cannot reselect without a top-level selection", ErrConfigConflict)
		}
		j.OriginalLine = string(raw[layout.selection.start:layout.selection.end])
		j.InsertedPrefix = ""
		raw = replaceSpan(raw, layout.selection, j.OwnedLine)
		layout, err = parseLayout(raw)
		if err != nil {
			return nil, j, err
		}
		blockSpan, err = ownedBlockSpan(raw, layout, j)
		if err != nil {
			return nil, j, err
		}
	}
	newBlock := generatedBlock(port, strings.TrimPrefix(j.OwnedLine, providerLine))
	out := replaceSpan(raw, blockSpan, newBlock)
	j.OwnedBlock, j.InstalledPort = newBlock, port
	if err := validateTOML(out); err != nil {
		return nil, j, err
	}
	return out, j, nil
}

func restoreInstall(raw []byte, j journal) ([]byte, error) {
	layout, err := parseLayout(raw)
	if err != nil {
		return nil, err
	}
	span, err := ownedBlockSpan(raw, layout, j)
	if err != nil {
		return nil, err
	}
	start := span.start
	if j.BlockSeparator != "" && start >= len(j.BlockSeparator) && string(raw[start-len(j.BlockSeparator):start]) == j.BlockSeparator {
		start -= len(j.BlockSeparator)
	}
	out := replaceSpan(raw, textSpan{start, span.end}, "")
	layout, err = parseLayout(out)
	if err != nil {
		return nil, err
	}
	if layout.selected == "switcher" {
		if !layout.selection.valid() || string(out[layout.selection.start:layout.selection.end]) != j.OwnedLine {
			return nil, fmt.Errorf("%w: Switcher selection line changed", ErrConfigConflict)
		}
		if j.InsertedPrefix != "" {
			span := layout.selection
			blank := strings.TrimPrefix(j.InsertedPrefix, j.OwnedLine)
			if bytes.HasPrefix(out[span.end:], []byte(blank)) {
				span.end += len(blank)
			}
			out = replaceSpan(out, span, "")
		} else {
			out = replaceSpan(out, layout.selection, j.OriginalLine)
		}
	}
	if err := validateTOML(out); err != nil {
		return nil, err
	}
	return out, nil
}

func knownLegacy(raw []byte, layout tomlLayout) (journal, bool) {
	if !layout.block.valid() || layout.blockPort <= 0 {
		return journal{}, false
	}
	for _, newline := range []string{"\n", "\r\n"} {
		owned := generatedBlock(layout.blockPort, newline)
		if !bytes.HasPrefix(raw[layout.block.start:layout.block.end], []byte(owned)) ||
			strings.TrimSpace(string(raw[layout.block.start+len(owned):layout.block.end])) != "" {
			continue
		}
		ownedLine := providerLine + newline
		if layout.selected == "switcher" && layout.selection.valid() {
			line := string(raw[layout.selection.start:layout.selection.end])
			if line == providerLine+"\n" || line == providerLine+"\r\n" {
				ownedLine = line
			}
		}
		return journal{Version: journalVersion, OwnedLine: ownedLine, OwnedBlock: owned,
			InstalledPort: layout.blockPort}, true
	}
	return journal{}, false
}

func readJournal(f *configFiles) (journal, bool, error) {
	raw, present, err := f.read(journalName, true)
	if err != nil || !present {
		return journal{}, present, err
	}
	var j journal
	if json.Unmarshal(raw, &j) != nil || j.Version != journalVersion || j.Path != f.path ||
		(j.Phase != "active" && j.Phase != "prepared") || j.OwnedLine == "" || j.OwnedBlock == "" ||
		j.InstalledPort < 1 || j.InstalledPort > 65535 {
		return journal{}, true, fmt.Errorf("%w: untrusted Codex ownership record", ErrConfigConflict)
	}
	newline := strings.TrimPrefix(j.OwnedLine, providerLine)
	if (newline != "\n" && newline != "\r\n") || j.OwnedBlock != generatedBlock(j.InstalledPort, newline) ||
		(j.Phase == "active" && (j.Operation != "" || j.TargetPort != 0 || j.SourceDigest != "" || j.TargetDigest != "" || j.SourceExists || j.TargetExists)) {
		return journal{}, true, fmt.Errorf("%w: malformed Codex ownership record", ErrConfigConflict)
	}
	if j.Phase == "prepared" && (j.SourceDigest == "" || j.TargetDigest == "" ||
		j.TargetPort < 1 || j.TargetPort > 65535 ||
		(j.Operation != "install" && j.Operation != "update" && j.Operation != "restore" && j.Operation != "reselect")) {
		return journal{}, true, fmt.Errorf("%w: incomplete Codex transaction", ErrConfigConflict)
	}
	return j, true, nil
}

func writeJournal(f *configFiles, j journal) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return f.replace(journalName, raw)
}

func pendingOutput(raw []byte, j journal) ([]byte, journal, error) {
	switch j.Operation {
	case "install":
		return createInstall(raw, !j.CreatedConfig, j.TargetPort, j.Path)
	case "update", "reselect":
		return updateInstall(raw, j, j.TargetPort, j.Operation == "reselect")
	case "restore":
		out, err := restoreInstall(raw, j)
		return out, j, err
	default:
		return nil, j, fmt.Errorf("%w: unknown operation", ErrConfigConflict)
	}
}

func finalizeJournal(f *configFiles, j journal) error {
	if j.Operation == "restore" {
		return f.remove(journalName)
	}
	j.Phase, j.Operation, j.SourceDigest, j.TargetDigest = "active", "", "", ""
	j.SourceExists, j.TargetExists = false, false
	j.InstalledPort, j.TargetPort = j.TargetPort, 0
	j.OwnedBlock = generatedBlock(j.InstalledPort, strings.TrimPrefix(j.OwnedLine, providerLine))
	return writeJournal(f, j)
}

func reconcileJournal(f *configFiles, j journal) (journal, bool, error) {
	if j.Phase == "active" {
		return j, true, nil
	}
	raw, exists, err := f.read(f.base, false)
	if err != nil {
		return j, true, err
	}
	actual := digest(raw)
	if actual == j.SourceDigest && exists == j.SourceExists {
		out, _, err := pendingOutput(raw, j)
		if err != nil || digest(out) != j.TargetDigest {
			return j, true, fmt.Errorf("%w: pending operation changed", ErrConfigConflict)
		}
		if j.Operation == "restore" && j.CreatedConfig && len(out) == 0 {
			if err = f.remove(f.base, expectedFile{j.SourceDigest, j.SourceExists}); err != nil {
				return j, true, err
			}
		} else if err = f.replace(f.base, out, expectedFile{j.SourceDigest, j.SourceExists}); err != nil {
			return j, true, err
		}
	} else if actual != j.TargetDigest || exists != j.TargetExists {
		return j, true, fmt.Errorf("%w: config changed during pending operation", ErrConfigConflict)
	}
	if saved, present, err := f.read(f.base, false); err != nil || present != j.TargetExists || digest(saved) != j.TargetDigest {
		return j, true, fmt.Errorf("%w: pending config write was changed", ErrConfigConflict)
	}
	if err := finalizeJournal(f, j); err != nil {
		return j, true, err
	}
	if j.Operation == "restore" {
		return journal{}, false, nil
	}
	j.Phase, j.Operation, j.InstalledPort = "active", "", j.TargetPort
	j.OwnedBlock = generatedBlock(j.InstalledPort, strings.TrimPrefix(j.OwnedLine, providerLine))
	j.TargetPort, j.SourceDigest, j.TargetDigest = 0, "", ""
	j.SourceExists, j.TargetExists = false, false
	return j, true, nil
}

func applyTransaction(f *configFiles, source []byte, sourceExists bool, target []byte, j journal, operation string) error {
	j.Phase, j.Operation = "prepared", operation
	j.SourceDigest, j.TargetDigest = digest(source), digest(target)
	j.SourceExists = sourceExists
	j.TargetExists = !(operation == "restore" && j.CreatedConfig && len(target) == 0)
	if err := writeJournal(f, j); err != nil {
		return err
	}
	if err := codexFault("journal_prepared"); err != nil {
		return err
	}
	current, present, err := f.read(f.base, false)
	if err != nil || present != j.SourceExists || digest(current) != j.SourceDigest {
		return fmt.Errorf("%w: config changed before write", ErrConfigConflict)
	}
	if err := codexFault("before_config_replace"); err != nil {
		return err
	}
	current, present, err = f.read(f.base, false)
	if err != nil || present != j.SourceExists || digest(current) != j.SourceDigest {
		return fmt.Errorf("%w: config changed before rename", ErrConfigConflict)
	}
	if operation == "restore" && j.CreatedConfig && len(target) == 0 {
		if err = f.remove(f.base, expectedFile{j.SourceDigest, j.SourceExists}); err != nil {
			return err
		}
	} else if err = f.replace(f.base, target, expectedFile{j.SourceDigest, j.SourceExists}); err != nil {
		return err
	}
	if err := codexFault("config_replaced"); err != nil {
		return err
	}
	if saved, present, err := f.read(f.base, false); err != nil || present != j.TargetExists || digest(saved) != j.TargetDigest {
		return fmt.Errorf("%w: config changed after write", ErrConfigConflict)
	}
	return finalizeJournal(f, j)
}

// codexFault is replaced by transaction tests to simulate an interrupted
// process between durable writes. Production always uses the no-op.
var codexFault = func(string) error { return nil }

func conflict(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrConfigConflict) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrConfigConflict, err)
}

func installAt(path string, port int, reselect bool) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid listener port")
	}
	f, err := openConfigFiles(path, true, true)
	if err != nil {
		return err
	}
	defer f.close()
	j, tracked, err := readJournal(f)
	if err != nil {
		return err
	}
	if tracked {
		j, tracked, err = reconcileJournal(f, j)
		if err != nil {
			return err
		}
	}
	raw, exists, err := f.read(f.base, false)
	if err != nil {
		return err
	}
	if !tracked {
		layout, err := parseLayout(raw)
		if err != nil {
			return err
		}
		if layout.block.valid() {
			legacy, known := knownLegacy(raw, layout)
			if known && layout.selected == "switcher" &&
				layout.selection.valid() && string(raw[layout.selection.start:layout.selection.end]) == legacy.OwnedLine && legacy.InstalledPort == port {
				return nil // a matching untracked legacy install is left as-is
			}
			if known {
				return ErrLegacyOwnershipUnknown
			}
			return fmt.Errorf("%w: Switcher provider ID is already in use", ErrConfigConflict)
		}
		out, pending, err := createInstall(raw, exists, port, f.path)
		if err != nil {
			return err
		}
		pending.TargetPort = port
		if exists {
			if err := f.backupOnce(raw); err != nil {
				return err
			}
		}
		return applyTransaction(f, raw, exists, out, pending, "install")
	}
	if !exists {
		return fmt.Errorf("%w: tracked Codex config disappeared", ErrConfigConflict)
	}
	layout, err := parseLayout(raw)
	if err != nil {
		return err
	}
	if _, err := ownedBlockSpan(raw, layout, j); err != nil {
		return err
	}
	selectionChanged := layout.selected != "switcher" || !layout.selection.valid() ||
		string(raw[layout.selection.start:layout.selection.end]) != j.OwnedLine
	if selectionChanged && layout.selected == "switcher" {
		return fmt.Errorf("%w: owned provider selection line was edited", ErrConfigConflict)
	}
	if selectionChanged && !reselect {
		return fmt.Errorf("%w: %w", ErrConfigConflict, ErrSelectionChanged)
	}
	out, next, err := updateInstall(raw, j, port, reselect)
	if err != nil {
		return err
	}
	if bytes.Equal(out, raw) {
		return nil
	}
	j.TargetPort = port
	if selectionChanged {
		j.OriginalLine, j.InsertedPrefix = next.OriginalLine, next.InsertedPrefix
	}
	operation := "update"
	if selectionChanged {
		operation = "reselect"
	}
	return applyTransaction(f, raw, true, out, j, operation)
}

func uninstallAt(path string, allowLegacy bool) error {
	f, err := openConfigFiles(path, false, true)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.close()
	j, tracked, err := readJournal(f)
	if err != nil {
		return err
	}
	if tracked {
		j, tracked, err = reconcileJournal(f, j)
		if err != nil {
			return err
		}
	}
	raw, exists, err := f.read(f.base, false)
	if err != nil {
		return err
	}
	if !exists {
		if tracked {
			return fmt.Errorf("%w: tracked Codex config disappeared", ErrConfigConflict)
		}
		return nil
	}
	if tracked {
		out, err := restoreInstall(raw, j)
		if err != nil {
			return err
		}
		j.TargetPort = j.InstalledPort
		return applyTransaction(f, raw, true, out, j, "restore")
	}
	layout, err := parseLayout(raw)
	if err != nil {
		return err
	}
	if !layout.block.valid() {
		if layout.selected == "switcher" {
			return ErrLegacyOwnershipUnknown
		}
		return nil
	}
	legacy, known := knownLegacy(raw, layout)
	if !known {
		return fmt.Errorf("%w: untracked Switcher table changed", ErrConfigConflict)
	}
	if !allowLegacy {
		return ErrLegacyOwnershipUnknown
	}
	out, err := restoreInstall(raw, legacy)
	if err != nil {
		return err
	}
	if err := f.backupOnce(raw); err != nil {
		return err
	}
	return f.replace(f.base, out, expectedFile{digest(raw), true})
}

func checkAt(path string, port int, expected string) Status {
	status := Status{Condition: "missing", ExpectedURL: expected, Ownership: "none", InstallAction: "configure"}
	f, err := openConfigFiles(path, false, false)
	if errors.Is(err, os.ErrNotExist) {
		return status
	}
	if err != nil {
		status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
		return status
	}
	defer f.close()
	j, tracked, err := readJournal(f)
	if err != nil {
		status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
		return status
	}
	if tracked && j.Phase == "prepared" {
		status.Condition, status.Ownership, status.InstallAction = "pending", "pending", ""
		status.PendingOperation = j.Operation
		return status
	}
	raw, exists, err := f.read(f.base, false)
	if err != nil {
		status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
		return status
	}
	if !exists {
		if tracked {
			status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
		}
		return status
	}
	layout, err := parseLayout(raw)
	if err != nil {
		status.Condition, status.InstallAction = "invalid", ""
		if errors.Is(err, ErrConfigConflict) {
			status.Condition, status.Ownership = "conflict", "conflict"
		}
		return status
	}
	if tracked {
		if _, err := ownedBlockSpan(raw, layout, j); err != nil {
			status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
			return status
		}
		status.Ownership, status.RestoreAction = "tracked", "restore"
		if layout.selected == "switcher" && layout.selection.valid() && string(raw[layout.selection.start:layout.selection.end]) != j.OwnedLine {
			status.Condition, status.Ownership, status.InstallAction, status.RestoreAction = "conflict", "conflict", "", ""
			return status
		}
		if layout.selected != "switcher" || !layout.selection.valid() || string(raw[layout.selection.start:layout.selection.end]) != j.OwnedLine {
			status.Condition, status.InstallAction = "not_selected", ""
			if layout.selection.valid() {
				status.InstallAction = "reselect"
			}
			return status
		}
		if j.InstalledPort == port {
			status.Condition, status.InstallAction = "ready", ""
		} else {
			status.Condition, status.InstallAction = "misconfigured", "update"
		}
		return status
	}
	if layout.block.valid() {
		legacy, known := knownLegacy(raw, layout)
		if !known {
			status.Condition, status.Ownership, status.InstallAction = "conflict", "conflict", ""
			return status
		}
		status.Ownership, status.RestoreAction, status.InstallAction = "legacy_candidate", "remove_legacy", ""
		if layout.selected == "switcher" && (!layout.selection.valid() || string(raw[layout.selection.start:layout.selection.end]) != legacy.OwnedLine) {
			status.Condition, status.Ownership, status.RestoreAction = "conflict", "conflict", ""
			return status
		}
		if layout.selected == "switcher" && legacy.InstalledPort == port {
			status.Condition = "ready"
		} else {
			status.Condition = "not_selected"
		}
		if layout.selected == "switcher" && legacy.InstalledPort != port {
			status.Condition = "misconfigured"
		}
		return status
	}
	if layout.selected != "" && layout.selected != "switcher" {
		status.Condition = "other_provider"
	}
	if layout.selected == "switcher" {
		status.Condition = "misconfigured"
	}
	return status
}
