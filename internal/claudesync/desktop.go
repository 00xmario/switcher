package claudesync

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

var ErrBusy = errors.New("Claude session sync is already running")
var desktopSync sync.Mutex

// Run is the single machine-level entry point used by both UIs. It does not
// accept paths or shell commands from the browser.
func Run(ctx context.Context, labels map[string]string) (Result, error) {
	if runtime.GOOS != "darwin" {
		return Result{}, errors.New("Claude Desktop session sync requires macOS")
	}
	if !desktopSync.TryLock() {
		return Result{}, ErrBusy
	}
	defer desktopSync.Unlock()
	root, err := SessionIndexRoot()
	if err != nil {
		return Result{}, err
	}
	backup, err := DefaultBackupDir()
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return Sync(ctx, Options{
		Root: root, BackupDir: backup, Labels: labels,
		Quit: func() error {
			// Do not launch a closed app just to quit it. A failed quit must
			// stop the sync rather than copy while Desktop is still writing.
			quitCtx, done := context.WithTimeout(ctx, 15*time.Second)
			defer done()
			if err := exec.CommandContext(quitCtx, "/usr/bin/osascript", "-e",
				`if application "Claude" is running then tell application "Claude" to quit`).Run(); err != nil {
				return err
			}
			for {
				err := exec.CommandContext(quitCtx, "/usr/bin/pgrep", "-x", "Claude").Run()
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() == 1 {
					return nil
				}
				if err != nil {
					return fmt.Errorf("check Claude exit: %w", err)
				}
				select {
				case <-quitCtx.Done():
					return quitCtx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
		},
		Reopen: func() error {
			// Cleanup must survive client cancellation or a merge failure.
			cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			return exec.CommandContext(cleanup, "/usr/bin/open", "-a", "Claude").Run()
		},
	})
}
