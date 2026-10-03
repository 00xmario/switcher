package claudecode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var ErrConflict = errors.New("Claude native credentials changed during switching; retry after Claude Code finishes refreshing")
var ErrNativeOwned = errors.New("Claude Code manages this login; use or reopen Claude Code to refresh it")

// Match proper-lockfile directory locks in Claude Code's own order. Never
// steal a live lock. Heartbeats retain ownership while Keychain I/O is active.
type lockContextKey struct{}

type lockSet struct {
	leases []*directoryLease
	cancel context.CancelCauseFunc
}

func checkLocks(ctx context.Context) error {
	if set, ok := ctx.Value(lockContextKey{}).(*lockSet); ok {
		for _, lease := range set.leases {
			if err := lease.check(); err != nil {
				set.cancel(err)
				return err
			}
		}
	}
	return ctx.Err()
}

func writeLocked(ctx context.Context, path string, value fileValue) error {
	if err := checkLocks(ctx); err != nil {
		return err
	}
	if err := writePrivate(path, value); err != nil {
		return err
	}
	return checkLocks(ctx)
}

func (m *Manager) locks(ctx context.Context) (context.Context, func(), error) {
	locked, cancel := context.WithCancelCause(ctx)
	set := &lockSet{cancel: cancel}
	locked = context.WithValue(locked, lockContextKey{}, set)
	paths := []struct {
		path  string
		stale time.Duration
	}{
		{filepath.Join(m.paths.ConfigHome, ".oauth_refresh.lock"), time.Minute},
		{m.paths.ConfigHome + ".lock", time.Minute},
		{m.paths.ConfigFile + ".lock", 10 * time.Second},
	}
	release := func() {
		for i := len(set.leases) - 1; i >= 0; i-- {
			set.leases[i].release()
		}
		cancel(context.Canceled)
	}
	for _, lock := range paths {
		lease, err := acquireDirectory(locked, lock.path, lock.stale, m.lockWait, cancel)
		if err != nil {
			release()
			return nil, nil, err
		}
		set.leases = append(set.leases, lease)
	}
	if err := checkLocks(locked); err != nil {
		release()
		return nil, nil, err
	}
	return locked, release, nil
}

type directoryLease struct {
	mu       sync.Mutex
	root     *os.Root
	file     *os.File
	name     string
	path     string
	owned    os.FileInfo
	stale    time.Duration
	stop     chan struct{}
	done     chan struct{}
	lost     error
	released bool
	once     sync.Once
}

func (l *directoryLease) check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.checkLocked()
}

func (l *directoryLease) checkLocked() error {
	if l.lost != nil {
		return l.lost
	}
	current, err := l.root.Lstat(l.name)
	if l.released || err != nil || !os.SameFile(l.owned, current) || time.Since(current.ModTime()) > l.stale {
		l.lost = ErrConflict
		return l.lost
	}
	// Native reads reopen the resolved profile path. A pinned old parent is
	// insufficient if that directory was renamed or replaced underneath us.
	current, err = os.Lstat(l.path)
	if err != nil || !os.SameFile(l.owned, current) {
		l.lost = ErrConflict
		return l.lost
	}
	return nil
}

func (l *directoryLease) release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		l.mu.Lock()
		defer l.mu.Unlock()
		if current, err := l.root.Lstat(l.name); err == nil && os.SameFile(current, l.owned) {
			_ = l.root.Remove(l.name)
		}
		l.released = true
		_ = l.file.Close()
		_ = l.root.Close()
	})
}

func acquireDirectory(ctx context.Context, path string, stale, wait time.Duration, compromise context.CancelCauseFunc) (*directoryLease, error) {
	root, err := openDirectory(filepath.Dir(path), true)
	if err != nil {
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			_ = root.Close()
		}
	}()
	name := filepath.Base(path)
	deadline := time.Now().Add(wait)
	var createdAt time.Time
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		createdAt = time.Now()
		err := root.Mkdir(name, 0700)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return nil, err
		}
		info, err := root.Lstat(name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() {
			return nil, ErrConflict
		}
		if time.Since(info.ModTime()) > stale {
			// Recheck the inode and lease time before stale reclamation.
			current, err := root.Lstat(name)
			if err == nil && os.SameFile(info, current) && time.Since(current.ModTime()) > stale && root.Remove(name) == nil {
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
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	owned, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if time.Since(createdAt) >= stale {
		// A suspended creator can resume after Code reclaimed its directory.
		// Do not claim or remove whichever inode was opened after that gap.
		_ = file.Close()
		return nil, ErrConflict
	}
	l := &directoryLease{root: root, file: file, name: name, path: path, owned: owned, stale: stale, stop: make(chan struct{}), done: make(chan struct{})}
	retained = true
	go func() {
		defer close(l.done)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-ticker.C:
				l.mu.Lock()
				err := l.checkLocked()
				if err == nil {
					now := unix.NsecToTimeval(time.Now().UnixNano())
					err = unix.Futimes(int(l.file.Fd()), []unix.Timeval{now, now})
					if err != nil {
						l.lost = ErrConflict
					}
				}
				l.mu.Unlock()
				if err != nil {
					compromise(ErrConflict)
					return
				}
			}
		}
	}()
	return l, nil
}
