package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func TestPasswordRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if s.VerifyPassword("hunter22") {
		t.Fatal("verify must fail without a password set")
	}
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if !s.HasPassword() {
		t.Fatal("password hash missing after set")
	}
	if !s.VerifyPassword("hunter22") {
		t.Fatal("correct password rejected")
	}
	if s.VerifyPassword("wrongpassword") {
		t.Fatal("wrong password accepted")
	}
}

func TestPasswordTooShort(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("short"); err == nil {
		t.Fatal("8-character minimum not enforced")
	}
}

func TestChangePasswordRequiresCurrent(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangePassword("wrong", "newpassword"); err == nil {
		t.Fatal("wrong current password accepted")
	}
	if err := s.ChangePassword("hunter22", "newerpass99"); err != nil {
		t.Fatal(err)
	}
	if !s.VerifyPassword("newerpass99") || s.VerifyPassword("hunter22") {
		t.Fatal("password change did not take effect")
	}
}

func TestSessionsLifecycle(t *testing.T) {
	s := New(t.TempDir())
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidateSession(token) {
		t.Fatal("fresh session rejected")
	}
	// A client-supplied value must never be adopted as a session.
	if s.ValidateSession("attacker-controlled-value") {
		t.Fatal("arbitrary string validated as a session")
	}
	s.DeleteSession(token)
	if s.ValidateSession(token) {
		t.Fatal("deleted session still valid")
	}
}

func TestSessionsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// A fresh Store instance simulates a server restart (the updater swaps
	// the process image; sessions must survive).
	reborn := New(dir)
	if !reborn.ValidateSession(token) {
		t.Fatal("session lost across restart")
	}
}

func TestDeviceTokenRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	token, err := s.EnsureDeviceToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 {
		t.Fatalf("device token length %d, want 64 hex chars", len(token))
	}
	again, err := s.EnsureDeviceToken()
	if err != nil || again != token {
		t.Fatal("device token not stable across reads")
	}
	info, err := os.Stat(filepath.Join(s.Path(), "..", "local-token"))
	_ = info
	if err != nil {
		t.Fatalf("device token file missing: %v", err)
	}
	if rotated, _ := s.RotateDeviceToken(); rotated == token {
		t.Fatal("rotation returned the same token")
	}
}

func TestRateLimiterLocksOut(t *testing.T) {
	s := New(t.TempDir())
	for i := 0; i < maxFailures; i++ {
		if err := s.CheckLockout(); err != nil {
			t.Fatalf("locked out after %d failures", i)
		}
		s.RecordFailure()
	}
	if err := s.CheckLockout(); err == nil {
		t.Fatal("lockout did not engage after max failures")
	}
	s.ResetFailures()
	if err := s.CheckLockout(); err != nil {
		t.Fatalf("lockout still active after reset: %v", err)
	}
}

func TestDisableAuthClearsEverything(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.SetPassword("hunter22"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureDeviceToken(); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableAuth(); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() || s.HasPassword() {
		t.Fatal("auth still enabled after disable")
	}
	if s.HasDeviceToken() {
		t.Fatal("device token survived disable")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); !os.IsNotExist(err) {
		t.Fatal("sessions file survived disable")
	}
}

func TestCorruptSettingsFailOpen(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{not json"), 0o600)
	s := New(dir)
	if s.Enabled() || s.HasPassword() {
		t.Fatal("corrupt settings must fail open to the historic behavior")
	}
}
