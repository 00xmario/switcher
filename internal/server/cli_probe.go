package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"switcher/internal/proxy"
)

type codexProbeRecord struct {
	Version     int    `json:"version"`
	Fingerprint string `json:"fingerprint"`
	At          int64  `json:"at"`
	Method      string `json:"method"`
	Model       string `json:"model"`
	Route       string `json:"route"`
}

type cliSetupVerification struct {
	Condition   string `json:"condition"`
	Method      string `json:"method,omitempty"`
	LastSuccess int64  `json:"last_success,omitempty"`
	Model       string `json:"model,omitempty"`
	Route       string `json:"route,omitempty"`
}

func (a *API) codexProbePath() string {
	if a.Settings == nil {
		return ""
	}
	return filepath.Join(filepath.Dir(a.Settings.Path()), "cli-probe.json")
}

func (a *API) codexProbeFingerprint(proxyFingerprint string) string {
	source := proxyFingerprint + "|" + a.Version + "|" + strconv.Itoa(a.cliSetupPort()) + "|" + proxy.CodexProbeModel + "|" + proxy.CodexProbeRoute
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

func (a *API) codexProbeStatus() cliSetupVerification {
	status := cliSetupVerification{Condition: "not_tested"}
	path := a.codexProbePath()
	if path == "" {
		return status
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return status
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		status.Condition = "unavailable"
		return status
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 4096 {
		status.Condition = "unavailable"
		return status
	}
	var record codexProbeRecord
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.Method != "switcher_proxy" ||
		record.Model != proxy.CodexProbeModel || record.Route != proxy.CodexProbeRoute || record.At <= 0 || record.Fingerprint == "" {
		status.Condition = "unavailable"
		return status
	}
	status.Method, status.LastSuccess, status.Model, status.Route = record.Method, record.At, record.Model, record.Route
	status.Condition = "historical"
	if a.Proxy != nil {
		if current, _ := a.Proxy.ProbeFingerprint(); current != "" && record.Fingerprint == a.codexProbeFingerprint(current) {
			status.Condition = "last_success"
		}
	}
	return status
}

func (a *API) saveCodexProbe(record codexProbeRecord) error {
	path := a.codexProbePath()
	if path == "" {
		return os.ErrInvalid
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cli-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (a *API) handleCodexRouteTest(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	input, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(input) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "route test takes no options or request body"})
		return
	}
	if a.Proxy == nil || a.Settings == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "route test is unavailable"})
		return
	}
	var result proxy.ProbeCodexResult
	if a.probeCodexForTest != nil {
		result = a.probeCodexForTest(r.Context())
	} else {
		result = a.Proxy.ProbeCodex(r.Context())
	}
	if result.Outcome == "busy" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a Codex route test is already running"})
		return
	}
	if result.Outcome == "success" {
		current, accountID := a.Proxy.ProbeFingerprint()
		if current == "" || current != result.Fingerprint || accountID != result.AccountID {
			result.Outcome = "selection_changed"
		} else if err := a.saveCodexProbe(codexProbeRecord{
			Version: 1, Fingerprint: a.codexProbeFingerprint(result.Fingerprint),
			At: time.Now().Unix(), Method: result.Method, Model: result.Model, Route: result.Route,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "route test succeeded but verification could not be saved"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"outcome": result.Outcome, "method": "switcher_proxy", "model": proxy.CodexProbeModel,
		"route": proxy.CodexProbeRoute, "verification": a.codexProbeStatus(),
	})
}
