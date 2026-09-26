package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestRefreshCredentialRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"unauthorized", http.StatusUnauthorized, `not json`, true},
		{"client rejected", http.StatusUnauthorized, `{"error":"invalid_client"}`, false},
		{"client unauthorized", http.StatusUnauthorized, `{"error":{"code":"unauthorized_client"}}`, false},
		{"invalid request", http.StatusUnauthorized, `{"error":{"type":"invalid_request"}}`, false},
		{"client auth failed", http.StatusUnauthorized, `{"error":"client_authentication_failed"}`, false},
		{"temporary 401", http.StatusUnauthorized, `{"error":"temporarily_unavailable"}`, false},
		{"server 401", http.StatusUnauthorized, `{"error":{"code":"server_error"}}`, false},
		{"client error 400", http.StatusBadRequest, `{"error":"invalid_client"}`, false},
		{"unknown 401", http.StatusUnauthorized, `{"error":"unknown_code"}`, false},
		{"unknown nested 401", http.StatusUnauthorized, `{"error":{"type":"unknown_code"}}`, false},
		{"grant rejected 401", http.StatusUnauthorized, `{"error":"invalid_grant"}`, true},
		{"description is not a code", http.StatusUnauthorized, `{"error_description":"invalid_client"}`, true},
		{"invalid grant", http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{"reused", http.StatusBadRequest, `{"error":"refresh_token_reused"}`, true},
		{"expired", http.StatusBadRequest, `{"error":"expired_token"}`, true},
		{"token expired", http.StatusUnauthorized, `{"error":"token_expired"}`, true},
		{"reused code", http.StatusUnauthorized, `{"error":"reused"}`, true},
		{"nested code", http.StatusBadRequest, `{"error":{"code":"invalid_grant"}}`, true},
		{"nested type", http.StatusBadRequest, `{"error":{"type":"invalid_grant_error"}}`, true},
		{"unrelated 400", http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{"text 400", http.StatusBadRequest, `invalid_grant`, false},
		{"description only", http.StatusBadRequest, `{"error_description":"invalid_grant"}`, false},
		{"forbidden", http.StatusForbidden, `{"error":"invalid_grant"}`, false},
		{"server error", http.StatusInternalServerError, `{"error":"invalid_grant"}`, false},
		{"ok", http.StatusOK, `{"error":"invalid_grant"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefreshCredentialRejected(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("RefreshCredentialRejected(%d, %q) = %t, want %t", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestUsageStatusError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		err := UsageStatusError(status)
		if !errors.Is(err, ErrUsageUnavailable) || errors.Is(err, ErrUsageAuthRequired) != (status == http.StatusUnauthorized) || errors.Is(err, ErrUsageRateLimited) != (status == http.StatusTooManyRequests) || errors.Is(err, ErrReloginRequired) {
			t.Fatalf("http %d: unexpected error classification: %v", status, err)
		}
	}
}

func TestRefreshCredentialRejectedDoesNotClassifyOversizedBody(t *testing.T) {
	body := `{"error":"invalid_grant","padding":"` + strings.Repeat("x", 8<<10) + `"}`
	if RefreshCredentialRejected(http.StatusUnauthorized, []byte(body)) {
		t.Fatal("oversized response should not mark an account as needing relogin")
	}
}
