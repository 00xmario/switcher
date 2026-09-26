package claude

import (
	"net/http"
	"strings"
	"testing"

	"switcher/internal/store"
)

func TestClaudeProxyPathAndOAuthHeaders(t *testing.T) {
	p := New()
	for _, tc := range []struct{ path, want string }{
		{"/v1/messages", "https://api.anthropic.com/v1/messages"},
		{"/v1/messages/count_tokens", "https://api.anthropic.com/v1/messages/count_tokens"},
		{"/messages", "https://api.anthropic.com/messages"},
	} {
		if got := p.UpstreamURL(tc.path); got != tc.want {
			t.Fatalf("UpstreamURL(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer old-client-token")
	r.Header.Set("X-Api-Key", "old-client-api-key")
	r.Header.Add("Anthropic-Beta", "feature-one, oauth-2025-04-20")
	r.Header.Add("Anthropic-Beta", "feature-two, feature-one")
	r.Header.Set("Anthropic-Version", "2023-06-01")
	r.Header.Set("X-Claude-Code-Feature", "preserve-me")
	if err := p.ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "switcher-token"}}); err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("Authorization") != "Bearer switcher-token" || r.Header.Get("X-Api-Key") != "" ||
		r.Header.Get("Anthropic-Beta") != "feature-one,oauth-2025-04-20,feature-two" ||
		r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("X-Claude-Code-Feature") != "preserve-me" {
		t.Fatalf("Claude proxy changed headers incorrectly: %v", r.Header)
	}
}

func TestClaudeBetaOverflowFailsWithoutEchoingValue(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	r.Header.Set("Anthropic-Beta", "private-"+strings.Repeat("x", 9<<10))
	err := New().ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "secret-token"}})
	if err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("oversized betas were not safely rejected: %v", err)
	}
}
