package claude

import (
	"context"
	"errors"
	"switcher/internal/provider"
	"switcher/internal/store"
	"testing"
)

func TestNativeSetupFailureDoesNotRestoreLegacyRefresh(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/fixture/config")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/fixture/other")
	p := New()
	if err := p.ConfigureNative(store.New(t.TempDir()), t.TempDir()); err == nil {
		t.Fatal("split storage was accepted")
	}
	a := store.Account{Provider: "claude", Token: store.Token{AccessToken: "fixture", RefreshToken: "fixture"}}
	if err := p.Refresh(context.Background(), &a); !errors.Is(err, provider.ErrNativeCredentialBusy) {
		t.Fatalf("legacy refresh was re-enabled: %v", err)
	}
	if _, err := p.ImportFromKeychain(context.Background()); err == nil {
		t.Fatal("failed profile setup fell back to default Keychain capture")
	}
	if p.NativeStatus().Available || !p.NativeEnabled() {
		t.Fatal("native setup failure was not an explicit guarded state")
	}
}

func TestOnlyExplicitOAuthRejectionsAreKnownUnconsumed(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":"invalid_grant"}`, true}, {401, `{"error":"invalid_client"}`, true},
		{502, `{"error":"invalid_grant"}`, false}, {504, `{}`, false}, {500, `{"error":"server_error"}`, false},
		{401, `gateway response`, false}, {403, `{"error":"unexpected"}`, false},
	} {
		if got := definitiveGrantRejection(tc.status, []byte(tc.body)); got != tc.want {
			t.Fatalf("status %d definitive=%v", tc.status, got)
		}
	}
}
