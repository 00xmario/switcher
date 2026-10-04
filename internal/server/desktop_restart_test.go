package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDesktopRestartRequiresConfirmedOwnedSetup(t *testing.T) {
	f := newDesktopSetupFixture(t)
	for _, body := range []string{"", `{}`, `{"confirmed":false}`, `{"confirmed":null}`, `{"confirmed":"true"}`} {
		f.request(t, "POST", "/restart-desktop", body, 400)
	}
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 409)
	if f.restart.Load() != 0 {
		t.Fatal("unconfirmed/unconfigured request invoked restart hook")
	}
	f.request(t, "POST", "/configure", `{}`, 200)
	if f.restart.Load() != 0 {
		t.Fatal("configure restarted Desktop")
	}
	w := f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 200)
	if f.restart.Load() != 1 || !strings.Contains(w.Body.String(), `"status":"restart_requested"`) || strings.Contains(w.Body.String(), "connected") {
		t.Fatal("restart acknowledgement claimed traffic evidence or omitted request status")
	}
	f.api.RestartDesktopForTest = func(context.Context) error { return errors.New("fixture-provider-secret") }
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	f.request(t, "POST", "/restore", `{}`, 200)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	if f.sync.Load() != 0 {
		t.Fatal("restart invoked session-index sync")
	}
}

func TestDesktopRestartAfterRestoreRequiresRetainedOwnership(t *testing.T) {
	f := newDesktopSetupFixture(t)
	before := decodeDesktopSetupReply(t, f.request(t, "GET", "", "", 200))
	if before.Condition != "not_configured" || before.Configured || before.RestartRequired || before.ScopeID != "" || before.BackupPath != "" {
		t.Fatal("never-configured settings carried restart ownership evidence")
	}
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 409)
	// A no-op restore cannot create the ownership record required for restart.
	f.request(t, "POST", "/restore", `{}`, 200)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 409)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true,"scope_id":"fixture-nonce","backup_path":"/fixture/backup","restart_required":true}`, 409)
	if f.restart.Load() != 0 {
		t.Fatal("unowned or browser-forged setup invoked restart hook")
	}
	configured := decodeDesktopSetupReply(t, f.request(t, "POST", "/configure", `{}`, 200))
	if !configured.Configured || configured.ScopeID == "" || configured.BackupPath == "" {
		t.Fatal("configure did not record ownership")
	}
	restored := decodeDesktopSetupReply(t, f.request(t, "POST", "/restore", `{}`, 200))
	source := f.api.DesktopRelay.SetupStatus(f.api.DesktopSettingsPath)
	if restored.Condition != "not_configured" || restored.Configured || !restored.RestartRequired ||
		restored.SettingsPath != f.api.DesktopSettingsPath || restored.ScopeID != configured.ScopeID || restored.BackupPath != configured.BackupPath ||
		source.Condition != restored.Condition || source.Configured || !source.RestartRequired || source.ScopeID != restored.ScopeID || source.BackupPath != restored.BackupPath {
		t.Fatal("restored status omitted the Manager's retained ownership evidence")
	}
	if f.restart.Load() != 0 {
		t.Fatal("configure or restore automatically restarted Desktop")
	}
	f.request(t, "POST", "/restart-desktop", `{}`, 400)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":false}`, 400)
	w := f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 200)
	if f.restart.Load() != 1 || !strings.Contains(w.Body.String(), `"status":"restart_requested"`) || f.sync.Load() != 0 {
		t.Fatal("confirmed restored setup did not invoke exactly one restart without sync")
	}
}

func TestDesktopRestartChangedSettingsBlocksHook(t *testing.T) {
	f := newDesktopSetupFixture(t)
	f.request(t, "POST", "/configure", `{}`, 200)
	if err := os.WriteFile(f.api.DesktopSettingsPath, []byte(`{"env":{"HTTPS_PROXY":"https://other.fixture.test"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 409)
	f.request(t, "POST", "/restore", `{}`, 409)
	if f.restart.Load() != 0 {
		t.Fatal("changed setup was allowed to restart Desktop")
	}
}

func TestDesktopRestartAndSyncCannotOverlap(t *testing.T) {
	f := newDesktopSetupFixture(t)
	f.request(t, "POST", "/configure", `{}`, 200)
	entered, release := make(chan struct{}), make(chan struct{})
	f.api.RestartDesktopForTest = func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "http://127.0.0.1:9123/api/desktop-relay/restart-desktop", strings.NewReader(`{"confirmed":true}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set(desktopControlHeader, f.api.ManagementKey)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("fixture restart did not enter hook")
	}
	// Always unblock the fixture if an assertion fails.
	defer func() {
		close(release)
		if w := <-done; w.Code != 200 {
			t.Errorf("first restart failed: %d %s", w.Code, w.Body.String())
		}
	}()
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 409)
	f.request(t, "POST", "/configure", `{}`, 409)
	f.request(t, "POST", "/restore", `{}`, 409)
	f.request(t, "GET", "", "", 200)
	w := desktopControlRequest(t, f.mux, "POST", "/api/claude/sync", "", "")
	if w.Code != http.StatusConflict || f.sync.Load() != 0 {
		t.Fatal("sync overlapped the restart operation")
	}
}

func TestDesktopRestartValidationWithUnavailableManager(t *testing.T) {
	f := newDesktopSetupFixture(t)
	f.api.DesktopRelay = nil
	f.request(t, "POST", "/restart-desktop", `{}`, 400)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":false}`, 400)
	f.request(t, "POST", "/restart-desktop", `{"confirmed":true}`, 503)
	if f.restart.Load() != 0 {
		t.Fatal("unavailable manager invoked restart")
	}
}

func TestDesktopRestartOperationsWaitForExitAndSurviveDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name       string
		quitErr    error
		processErr error
		cancelQuit bool
		wantOpen   bool
	}{
		{"failed quit blocks open", errors.New("fixture quit failure"), nil, false, false},
		{"failed exit check blocks open", nil, errors.New("fixture process failure"), false, false},
		{"successful quit waits for actual exit", nil, nil, false, true},
		{"disconnect after quit still reopens", nil, nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queries, opened := 0, false
			err := runDesktopRestart(ctx, desktopRestartOperations{
				quit: func(ctx context.Context) error {
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("quit has no deadline")
					}
					if tc.cancelQuit {
						cancel()
					}
					return tc.quitErr
				},
				running: func(ctx context.Context) (bool, error) {
					queries++
					if ctx.Err() != nil {
						t.Fatal("exit wait inherited client cancellation")
					}
					return queries == 1, tc.processErr
				},
				open: func(ctx context.Context) error {
					if queries != 2 || ctx.Err() != nil {
						t.Fatal("opened before actual exit or inherited cancellation")
					}
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("reopen has no deadline")
					}
					opened = true
					return nil
				},
			})
			if opened != tc.wantOpen || (err == nil) != tc.wantOpen {
				t.Fatalf("open=%t error=%v", opened, err)
			}
		})
	}
}

func TestDesktopRestartWaitIsBoundedAndStoppedAppCanLaunch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := waitDesktopExit(ctx, func(context.Context) (bool, error) { return true, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exit wait did not honor deadline: %v", err)
	}
	opened := false
	if err := runDesktopRestart(context.Background(), desktopRestartOperations{
		quit:    func(context.Context) error { return nil },
		running: func(context.Context) (bool, error) { return false, nil },
		open:    func(context.Context) error { opened = true; return nil },
	}); err != nil || !opened {
		t.Fatal("explicit restart did not launch a stopped app")
	}
}
