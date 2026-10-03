package server

import (
	"errors"
	"net/http"
	"switcher/internal/claudesync"
)

func (a *API) handleClaudeSync(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	// Sync and relay restart both quit and reopen the same app. Keep their
	// machine-level operations from interleaving within this server.
	if !desktopAppOperation.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "Claude Desktop operation already running"})
		return
	}
	defer desktopAppOperation.Unlock()
	accounts, err := a.Store.List()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Could not read account names"})
		return
	}
	labels := map[string]string{}
	for _, account := range accounts {
		if account.Provider == "claude" && account.Token.AccountID != "" {
			labels[account.Token.AccountID] = account.Email
		}
	}
	run := claudesync.Run
	if a.syncClaudeForTest != nil {
		run = a.syncClaudeForTest
	}
	result, err := run(r.Context(), labels)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, claudesync.ErrNoAccounts) || errors.Is(err, claudesync.ErrBusy) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": "Claude session sync did not complete", "details": err.Error(), "result": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "message": "Claude sessions synced", "detail": claudesync.Describe(result), "result": result})
}
