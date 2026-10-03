package proxy

import (
	"maps"
	"strings"

	"switcher/internal/googleauth"
	"switcher/internal/store"
)

func preserveAccountMetadata(a *store.Account, existing store.Account) {
	a.AutoUseReset = existing.AutoUseReset
	if existing.CreatedAt != 0 {
		a.CreatedAt = existing.CreatedAt
	}
	if a.Provider != existing.Provider || !strings.EqualFold(a.Email, existing.Email) ||
		(a.Provider != "gemini" && a.Provider != "antigravity") ||
		(existing.Token.AccountID != "" && a.Token.AccountID != "" && existing.Token.AccountID != a.Token.AccountID) {
		return
	}
	project, _ := a.Token.Extra["project_id"].(string)
	saved, _ := existing.Token.Extra["project_id"].(string)
	status, _ := a.Token.Extra[googleauth.ProjectDiscoveryStatusKey].(string)
	// Missing status supports pre-metadata login results already in flight.
	// A discovered result remains authoritative, including an explicit clear.
	if project == "" && saved != "" && (status == "" || status == googleauth.ProjectDiscoveryFailed) {
		a.Token.Extra = maps.Clone(a.Token.Extra)
		if a.Token.Extra == nil {
			a.Token.Extra = make(map[string]any)
		}
		a.Token.Extra["project_id"] = saved
	}
}
