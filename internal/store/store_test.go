package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	a := Account{ID: "codex-abcd", Provider: "codex", Email: "me@example.com",
		Token: Token{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 42}}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("codex-abcd")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token.RefreshToken != "rt" || got.Email != "me@example.com" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if err := s.Delete("codex-abcd"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("codex-abcd"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestTokenFilePermissions(t *testing.T) {
	s := New(t.TempDir())
	a := Account{ID: "x", Provider: "codex", Email: "x@example.com"}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.accountsDir, "x.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %o, want 600", perm)
	}
}

func TestStateRoundTripAndList(t *testing.T) {
	s := New(t.TempDir())
	for _, id := range []string{"b", "a"} {
		if err := s.Save(Account{ID: id, Provider: "codex", Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	// List is sorted by email for deterministic UI rendering.
	if len(accounts) != 2 || accounts[0].ID != "a" || accounts[1].ID != "b" {
		t.Fatalf("unexpected list: %+v", accounts)
	}

	if err := s.SaveState(State{Active: map[string]string{"codex": "a"}, Exhausted: map[string]int64{"b": 123}}); err != nil {
		t.Fatal(err)
	}
	state, err := s.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active["codex"] != "a" || state.Exhausted["b"] != 123 {
		t.Fatalf("state mismatch: %+v", state)
	}
}

func TestDeleteMissingIsIdempotent(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Delete("ghost"); err != nil {
		t.Fatalf("delete of missing account must not fail: %v", err)
	}
}
