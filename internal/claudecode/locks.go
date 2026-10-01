package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var ErrConflict = errors.New("Claude native credentials changed during switching; retry after Claude Code finishes refreshing")
var ErrNativeOwned = errors.New("Claude Code manages this login; use or reopen Claude Code to refresh it")

// Match proper-lockfile directory locks in Claude Code's own order. Never
// steal a live lock. Heartbeats retain ownership while Keychain I/O is active.
func (m *Manager) locks(ctx context.Context) (func(), error) {
	paths := []struct {
		path  string
		stale time.Duration
	}{
		{filepath.Join(m.paths.ConfigHome, ".oauth_refresh.lock"), time.Minute},
		{m.paths.ConfigHome + ".lock", time.Minute},
		{m.paths.ConfigFile + ".lock", 10 * time.Second},
	}
	releases := []func(){}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, lock := range paths {
		unlock, err := acquireDirectory(ctx, lock.path, lock.stale, m.lockWait)
		if err != nil {
			release()
			return nil, err
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

func acquireDirectory(ctx context.Context, path string, stale, wait time.Duration) (func(), error) {
	if err := safeDirectory(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := os.Mkdir(path, 0700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return nil, err
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() {
			return nil, ErrConflict
		}
		if time.Since(info.ModTime()) > stale {
			if os.Remove(path) == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("Claude Code is holding its credential/config lock; retry shortly")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	owned, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				current, err := os.Lstat(path)
				if err != nil || !os.SameFile(owned, current) {
					return
				}
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			if current, err := os.Lstat(path); err == nil && os.SameFile(current, owned) {
				_ = os.Remove(path)
			}
		})
	}, nil
}
