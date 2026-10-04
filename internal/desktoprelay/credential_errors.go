package desktoprelay

import (
	"errors"
	"net/http"
)

// CredentialError classifies a selected-account credential failure without
// exposing the source error, which may carry provider text or tokens.
type CredentialError struct {
	Code string `json:"error_code"`
	kind error
}

func (e *CredentialError) Error() string {
	if e.Code == "account_not_found" {
		return "Switcher: the selected account no longer exists"
	}
	return "Switcher could not get a valid credential for the selected account; check the account in Switcher"
}
func (e *CredentialError) Unwrap() error     { return e.kind }
func (e *CredentialError) ErrorCode() string { return e.Code }

func credentialFailure(err error) *CredentialError {
	if errors.Is(err, ErrNotFound) {
		return &CredentialError{Code: "account_not_found", kind: ErrNotFound}
	}
	return &CredentialError{Code: "credential_unavailable", kind: ErrUnavailable}
}

func replyCredentialFailure(w http.ResponseWriter, err error) {
	e := credentialFailure(err)
	status, kind := http.StatusServiceUnavailable, "api_error"
	if errors.Is(e, ErrNotFound) {
		status, kind = http.StatusNotFound, "not_found_error"
	}
	relayError(w, status, kind, e.Error(), e.Code)
}
