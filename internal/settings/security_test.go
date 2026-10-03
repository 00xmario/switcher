package settings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSettingsErrorsRetainSecurityStateAndRejectWrites(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("original-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *Settings) error { st.BindLAN, st.TLS = true, true; return nil }); err != nil {
		t.Fatal(err)
	}
	good := s.Load()
	valid, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, damage := range []string{"corrupt", "nonregular", "missing"} {
		t.Run(damage, func(t *testing.T) {
			if err := os.Remove(s.Path()); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "corrupt":
				if err := os.WriteFile(s.Path(), []byte("{not json"), 0600); err != nil {
					t.Fatal(err)
				}
			case "nonregular":
				if err := os.Mkdir(s.Path(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			st, err := s.Snapshot()
			if err == nil || !s.Enabled() || !st.AuthEnabled || !st.BindLAN || !st.TLS || st.PasswordHash != good.PasswordHash || st.CSRFToken != good.CSRFToken {
				t.Fatalf("security snapshot lost after %s: %+v, %v", damage, st, err)
			}
			if s.VerifyPassword("original-password") {
				t.Fatal("unreadable settings authorized fresh credentials")
			}
			if err := s.Update(func(st *Settings) error { st.CompactAccounts = true; return nil }); err == nil {
				t.Fatal("a preference write replaced damaged security settings")
			}
			if damage != "missing" {
				if err := os.Remove(s.Path()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(s.Path(), valid, 0600); err != nil {
				t.Fatal(err)
			}
			if !s.VerifyPassword("original-password") {
				t.Fatal("repaired settings did not recover")
			}
		})
	}
}

func TestInitialSettingsErrorsStayClosedUntilRepairOrNewStore(t *testing.T) {
	for _, raw := range []string{"{broken", "null"} {
		t.Run(raw, func(t *testing.T) {
			root := t.TempDir()
			s := New(root)
			if err := os.WriteFile(s.Path(), []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Snapshot(); err == nil || !s.Enabled() {
				t.Fatal("initial damaged settings were treated as a new install")
			}
			if err := os.Remove(s.Path()); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Snapshot(); err == nil || !s.Enabled() {
				t.Fatal("deletion after a read error removed authentication")
			}
			if err := s.SetPassword("replacement-password"); err == nil {
				t.Fatal("damaged security state was silently overwritten")
			}
			if New(root).Enabled() {
				t.Fatal("explicit delete and fresh-store recovery lost its default")
			}
		})
	}
}

func TestInitialSetupCannotReplaceAConcurrentPassword(t *testing.T) {
	s := New(t.TempDir())
	start := make(chan struct{})
	results := make(chan struct {
		password string
		err      error
	}, 2)
	for _, password := range []string{"first-password", "second-password"} {
		go func(password string) {
			<-start
			results <- struct {
				password string
				err      error
			}{password, s.SetPassword(password)}
		}(password)
	}
	close(start)
	winner, successes := "", 0
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err == nil {
			winner, successes = result.password, successes+1
		} else if !errors.Is(result.err, ErrPasswordAlreadySet) {
			t.Fatal(result.err)
		}
	}
	if successes != 1 || !s.VerifyPassword(winner) {
		t.Fatal("concurrent initial setup did not preserve exactly one password")
	}
}

func TestVerifiedLoginCannotCrossPasswordRotation(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SetPassword("old-password"); err != nil {
		t.Fatal(err)
	}
	oldSession, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	oldDisk, err := os.ReadFile(s.sessionsPath())
	if err != nil {
		t.Fatal(err)
	}
	verified, resume := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		proof, ok := s.VerifyPasswordForSession("old-password")
		if !ok {
			result <- errors.New("old password was not verified")
			close(verified)
			return
		}
		close(verified)
		<-resume
		token, csrf, err := s.NewVerifiedSession(proof)
		if token != "" || csrf != "" {
			result <- errors.New("stale verification minted credentials")
			return
		}
		result <- err
	}()
	<-verified
	if err := s.ChangePassword("old-password", "new-password"); err != nil {
		close(resume)
		t.Fatal(err)
	}
	close(resume)
	if err := <-result; !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale login: %v", err)
	}
	if s.ValidateSession(oldSession) {
		t.Fatal("old session survived rotation")
	}
	// Even restoration of a pre-rotation file cannot revive its sessions.
	if err := os.WriteFile(s.sessionsPath(), oldDisk, 0600); err != nil {
		t.Fatal(err)
	}
	if New(filepath.Dir(s.Path())).ValidateSession(oldSession) {
		t.Fatal("old disk session survived restart")
	}
	proof, ok := s.VerifyPasswordForSession("new-password")
	if !ok {
		t.Fatal("new password rejected")
	}
	token, csrf, err := s.NewVerifiedSession(proof)
	if err != nil || csrf != s.CSRFToken() || !New(filepath.Dir(s.Path())).ValidateSession(token) {
		t.Fatalf("new generation did not persist: %v", err)
	}
}

func TestAtomicCredentialsIgnoreOldTempSymlinksAndRefuseRedirects(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(victim, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "sessions.json", "local-token"} {
		path := filepath.Join(dir, name)
		if err := os.Symlink(victim, path+".tmp"); err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(path, []byte("private")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("unsafe published credential: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, path); err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(path, []byte("clobber")); err == nil {
			t.Fatal("symlink target accepted")
		}
		if _, err := readPrivateFile(path); err == nil {
			t.Fatal("symlink credential was read")
		}
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(filepath.Dir(victim), alias); err != nil {
		t.Fatal(err)
	}
	if err := New(alias).Save(Settings{}); err == nil {
		t.Fatal("redirected credential directory accepted")
	}
	raw, err := os.ReadFile(victim)
	if err != nil || string(raw) != "untouched" {
		t.Fatal("atomic credential write modified another file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switcher-settings-") {
			t.Fatal("temporary credential leaked")
		}
	}
}

func TestConcurrentDeviceTokenCreationAndRotation(t *testing.T) {
	s := New(t.TempDir())
	var wg sync.WaitGroup
	results := make(chan string, 12)
	errs := make(chan error, 12)
	run := func(rotate bool) {
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var token string
				var err error
				if rotate {
					token, err = s.RotateDeviceToken()
				} else {
					token, err = s.EnsureDeviceToken()
				}
				results <- token
				errs <- err
			}()
		}
		wg.Wait()
	}
	run(false)
	original := ""
	for i := 0; i < 12; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		token := <-results
		if original == "" {
			original = token
		}
		if token != original {
			t.Fatal("concurrent ensure returned inconsistent tokens")
		}
	}
	run(true)
	rotated := map[string]bool{}
	for i := 0; i < 12; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		rotated[<-results] = true
	}
	current, err := s.ReadDeviceToken()
	if err != nil || current == original || !rotated[current] || len(rotated) != 12 {
		t.Fatalf("concurrent rotation lost its published token: %v", err)
	}
}

func TestUnboundLegacySessionsRequireFreshLogin(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.sessionsPath())
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "credential_generation")
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.sessionsPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if New(root).ValidateSession(token) {
		t.Fatal("a session with unknown password generation was adopted")
	}
}

func TestDeleteSessionReportsWriteFailureAndPersistsRevokedRetry(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	path := s.sessionsPath()
	backup := filepath.Join(root, "readable-sessions.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(token); err == nil {
		t.Fatal("session deletion acknowledged a blocked persistent write")
	}
	if s.ValidateSession(token) {
		t.Fatal("failed persistence restored the in-memory session")
	}
	if !s.CanRetrySessionDeletion(token) {
		t.Fatal("failed deletion lost its narrowly scoped retry proof")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	if !New(root).ValidateSession(token) {
		t.Fatal("fixture did not preserve the readable old session on disk")
	}
	if s.ValidateSession(token) || !s.CanRetrySessionDeletion(token) {
		t.Fatal("repaired storage must allow deletion retry without restoring in-memory access")
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if New(root).ValidateSession(token) || s.CanRetrySessionDeletion(token) {
		t.Fatal("successful retry did not durably revoke the session")
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatalf("durable deletion was not idempotent: %v", err)
	}
}

func TestDeleteSessionRetainsRevocationAfterSettingsReadFailure(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(token); err == nil {
		t.Fatal("unreadable security settings were ignored during logout")
	}
	if err := os.WriteFile(s.Path(), good, 0600); err != nil {
		t.Fatal(err)
	}
	if s.ValidateSession(token) || !s.CanRetrySessionDeletion(token) {
		t.Fatal("repair restored in-memory access or lost deletion retry")
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if New(root).ValidateSession(token) {
		t.Fatal("retry after settings repair did not durably revoke the session")
	}
}

func TestDeleteSessionRetryProofDoesNotDependOnOldDiskEntry(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.SetPassword("fixture-password"); err != nil {
		t.Fatal(err)
	}
	token, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	path, backup := s.sessionsPath(), filepath.Join(root, "readable-sessions.json")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, path); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(token); err == nil {
		t.Fatal("fixture did not block the persistent deletion")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Model an already-published empty file whose durability is still unknown.
	// Retry must not require the old session entry to remain in that file.
	empty, err := json.Marshal(sessionFile{Sessions: map[string]int64{}, Generation: credentialGeneration(s.Load())})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, empty); err != nil {
		t.Fatal(err)
	}
	if !s.CanRetrySessionDeletion(token) || s.ValidateSession(token) {
		t.Fatal("missing disk entry lost retry authorization or restored session access")
	}
	if err := s.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if s.CanRetrySessionDeletion(token) || New(root).ValidateSession(token) {
		t.Fatal("durable retry retained its proof or left the session alive")
	}
}
