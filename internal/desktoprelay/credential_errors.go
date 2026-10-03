package desktoprelay

import (
	"errors"
	"net/http"
)

// CredentialError exposes a stable classification without retaining a source
// error, its message, or its chain. Unwrap returns only a module sentinel.
type CredentialError struct {
	Code string `json:"error_code"`
	kind error
}

func (e *CredentialError) Error() string {
	switch e.Code {
	case "account_not_found":
		return "desktop relay selected account no longer exists"
	case "credential_busy":
		return "desktop relay credential source busy"
	default:
		return "desktop relay selected credential unavailable"
	}
}
func (e *CredentialError) Unwrap() error     { return e.kind }
func (e *CredentialError) ErrorCode() string { return e.Code }

func credentialFailure(err error) *CredentialError {
	switch {
	case errors.Is(err, ErrNotFound):
		return &CredentialError{Code: "account_not_found", kind: ErrNotFound}
	case errors.Is(err, ErrBusy) || errors.Is(err, ErrCredentialBusy):
		return &CredentialError{Code: "credential_busy", kind: ErrCredentialBusy}
	default:
		return &CredentialError{Code: "credential_unavailable", kind: ErrUnavailable}
	}
}

func replyCredentialFailure(w http.ResponseWriter, err error) {
	e := credentialFailure(err)
	status, kind := http.StatusServiceUnavailable, "api_error"
	if errors.Is(e, ErrNotFound) {
		status, kind = http.StatusNotFound, "not_found_error"
	}
	relayError(w, status, kind, e.Error(), e.Code)
}
