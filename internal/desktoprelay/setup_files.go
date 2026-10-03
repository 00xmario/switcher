package desktoprelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var errSetupMissing = errors.New("setup path missing")

// Existing native directories can be 0755. Only newly created parents are
// private, and no existing directory is chmodded. All opens are descriptor
// relative, including reads, locks, backups, temp files and final renames.
func openSetupDirectory(path string, create bool) (*os.File, error) {
	if !setupPath(path) {
		return nil, ErrUnavailable
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e == unix.ENOENT && create {
			e = unix.Mkdirat(fd, part, 0700)
			if e == nil || e == unix.EEXIST {
				e = unix.Fsync(fd)
				if e == nil {
					next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				}
			}
		}
		unix.Close(fd)
		if e != nil {
			if e == unix.ENOENT {
				return nil, errSetupMissing
			}
			return nil, e
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || (st.Uid != 0 && st.Uid != uint32(os.Getuid())) || (st.Mode&0022 != 0 && !(st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)) || (i == len(parts)-1 && st.Uid != uint32(os.Getuid())) {
			unix.Close(fd)
			return nil, ErrUnavailable
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func setupDirectoryCurrent(dir *os.File) bool {
	current, err := openSetupDirectory(dir.Name(), false)
	if err != nil {
		return false
	}
	defer current.Close()
	a, err := dir.Stat()
	if err != nil {
		return false
	}
	b, err := current.Stat()
	return err == nil && os.SameFile(a, b)
}

type setupFile struct {
	data   []byte
	exists bool
	info   os.FileInfo
}

func readSetupFile(dir *os.File, name string) (setupFile, error) {
	var out setupFile
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == unix.ENOENT {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || st.Mode&0022 != 0 || st.Size > maxSetupSettings {
		return out, ErrUnavailable
	}
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSetupSettings+1))
	if err != nil || len(data) > maxSetupSettings {
		return out, ErrUnavailable
	}
	end, err := f.Stat()
	if err != nil || !os.SameFile(info, end) || info.Size() != end.Size() || !info.ModTime().Equal(end.ModTime()) {
		return out, setupConflict()
	}
	var named unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || named.Dev != st.Dev || named.Ino != st.Ino || named.Uid != uint32(os.Getuid()) || named.Mode&0022 != 0 || named.Nlink != 1 {
		return out, setupConflict()
	}
	return setupFile{data: data, exists: true, info: info}, nil
}

func setupSameFile(a, b setupFile) bool {
	if a.exists != b.exists || !bytes.Equal(a.data, b.data) {
		return false
	}
	return !a.exists || (os.SameFile(a.info, b.info) && a.info.Mode() == b.info.Mode() && a.info.ModTime().Equal(b.info.ModTime()))
}

type setupLease struct {
	dir  *os.File
	file *os.File
	name string
	info os.FileInfo
	mu   sync.Mutex
	lost bool
	stop chan struct{}
	done chan struct{}
}

func acquireSetupLease(ctx context.Context, dir *os.File, name string) (*setupLease, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !setupDirectoryCurrent(dir) {
			return nil, setupConflict()
		}
		created := time.Now()
		err := unix.Mkdirat(int(dir.Fd()), name, 0700)
		if err == nil {
			fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return nil, setupConflict()
			}
			f := os.NewFile(uintptr(fd), name)
			info, err := f.Stat()
			if err != nil || time.Since(created) >= 10*time.Second {
				f.Close()
				return nil, setupConflict()
			}
			l := &setupLease{dir: dir, file: f, name: name, info: info, stop: make(chan struct{}), done: make(chan struct{})}
			if !l.check() {
				f.Close()
				return nil, setupConflict()
			}
			go l.heartbeat()
			return l, nil
		}
		if err != unix.EEXIST {
			return nil, setupFailure(err)
		}
		// Match proper-lockfile's settings lock and 10 second stale interval.
		// Reclaim only an empty, owned real directory after inode/time fences.
		fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e == unix.ENOENT {
			continue
		}
		if e != nil {
			return nil, setupConflict()
		}
		f := os.NewFile(uintptr(fd), name)
		info, e := f.Stat()
		var st, named unix.Stat_t
		safe := e == nil && unix.Fstat(fd, &st) == nil && st.Uid == uint32(os.Getuid()) && st.Mode&0022 == 0
		if !safe {
			f.Close()
			return nil, setupConflict()
		}
		if time.Since(info.ModTime()) > 10*time.Second && unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) == nil && named.Dev == st.Dev && named.Ino == st.Ino {
			end, e := f.Stat()
			if e == nil && end.ModTime().Equal(info.ModTime()) && setupDirectoryCurrent(dir) && unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR) == nil {
				f.Close()
				continue
			}
		}
		f.Close()
		if time.Now().After(deadline) {
			return nil, setupError("setup_lock_busy", ErrBusy)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (l *setupLease) checkLocked() bool {
	var st, named unix.Stat_t
	info, err := l.file.Stat()
	if l.lost || err != nil || !os.SameFile(info, l.info) || time.Since(info.ModTime()) > 10*time.Second || !setupDirectoryCurrent(l.dir) || unix.Fstat(int(l.file.Fd()), &st) != nil || st.Uid != uint32(os.Getuid()) || st.Mode&0022 != 0 || unix.Fstatat(int(l.dir.Fd()), l.name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || named.Dev != st.Dev || named.Ino != st.Ino {
		l.lost = true
		return false
	}
	return true
}
func (l *setupLease) check() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.checkLocked() }
func (l *setupLease) heartbeat() {
	defer close(l.done)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.checkLocked() {
				now := unix.NsecToTimeval(time.Now().UnixNano())
				if unix.Futimes(int(l.file.Fd()), []unix.Timeval{now, now}) != nil {
					l.lost = true
				}
			}
			l.mu.Unlock()
		}
	}
}
func (l *setupLease) close() {
	close(l.stop)
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.checkLocked() {
		_ = unix.Unlinkat(int(l.dir.Fd()), l.name, unix.AT_REMOVEDIR)
	}
	_ = l.file.Close()
}

func (m *Manager) writeSetupFile(ctx context.Context, dir *os.File, name string, lease *setupLease, before setupFile, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, err := randomHex(16)
	if err != nil {
		return ErrUnavailable
	}
	root := &privateStore{dir: dir}
	tmp := ".relay-setup-" + token
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return setupFailure(err)
	}
	defer f.Close()
	defer root.Remove(tmp)
	var tempStat unix.Stat_t
	if unix.Fstat(int(f.Fd()), &tempStat) != nil {
		return setupFailure(ErrUnavailable)
	}
	_, err = f.Write(data)
	if err == nil {
		if m.cfg.SyncSetupFile != nil {
			err = m.cfg.SyncSetupFile(f)
		} else {
			err = f.Sync()
		}
	}
	if err == nil && !setupTempCurrent(dir, tmp, tempStat) {
		err = setupConflict()
	}
	if err != nil {
		return setupFailure(err)
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !lease.check() {
		return setupConflict()
	}
	current, err := readSetupFile(dir, name)
	if err != nil || !setupSameFile(before, current) {
		return setupConflict()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !setupTempCurrent(dir, tmp, tempStat) {
		return setupConflict()
	}
	if err = root.Rename(tmp, name); err != nil {
		return setupFailure(err)
	}
	if err = f.Close(); err != nil {
		return m.setupCommitUncertainLocked()
	}
	if m.cfg.SyncDirectory != nil {
		err = m.cfg.SyncDirectory(dir)
	} else {
		err = dir.Sync()
	}
	if err != nil {
		return m.setupCommitUncertainLocked()
	}
	// A cooperating writer cannot change this while our lease is held. Fence
	// noncooperating writers and parent swaps before acknowledging the journal.
	if !lease.check() {
		return setupConflict()
	}
	current, err = readSetupFile(dir, name)
	if err != nil || !current.exists || !bytes.Equal(current.data, data) || !setupTempCurrent(dir, name, tempStat) {
		return setupConflict()
	}
	return nil
}

func (m *Manager) setupCommitUncertainLocked() error {
	m.failed = true
	m.condition = "store_error"
	if m.run != nil {
		m.run.cancel()
	}
	return errors.Join(setupError("setup_unavailable", ErrUnavailable), errCommitUncertain)
}

func setupTempCurrent(dir *os.File, name string, expected unix.Stat_t) bool {
	var current unix.Stat_t
	return unix.Fstatat(int(dir.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW) == nil && current.Dev == expected.Dev && current.Ino == expected.Ino && current.Mode&unix.S_IFMT == unix.S_IFREG && current.Mode&0777 == 0600 && current.Uid == uint32(os.Getuid()) && current.Nlink == 1
}

func (m *Manager) setupBackupDirectory(create bool) (*privateStore, error) {
	if create {
		err := unix.Mkdirat(int(m.store.dir.Fd()), "backups", 0700)
		if err != nil && err != unix.EEXIST {
			return nil, err
		}
		if err == nil {
			if err = m.store.dir.Sync(); err != nil {
				return nil, err
			}
		}
	}
	fd, err := unix.Openat(int(m.store.dir.Fd()), "backups", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(m.cfg.DataRoot, "backups"))
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Getuid()) || st.Mode&0777 != 0700 {
		f.Close()
		return nil, ErrUnavailable
	}
	return &privateStore{dir: f, syncDirectory: m.cfg.SyncDirectory}, nil
}
func (m *Manager) backupSetupLocked(r setupRecord, data []byte) error {
	root, err := m.setupBackupDirectory(true)
	if err != nil {
		return err
	}
	defer root.Close()
	return atomicPrivate(root, r.BackupName, data)
}
func (m *Manager) validateSetupBackupLocked(r setupRecord) error {
	_, err := m.loadSetupBaselineLocked(r)
	return err
}

func (m *Manager) loadSetupBaselineLocked(r setupRecord) (setupDocument, error) {
	root, err := m.setupBackupDirectory(false)
	if err != nil {
		return setupDocument{}, setupFailure(err)
	}
	defer root.Close()
	b, err := readPrivate(root, r.BackupName)
	if err != nil || setupDigest(b, r.FileExisted) != r.OriginalDigest {
		return setupDocument{}, setupError("setup_unavailable", ErrUnavailable)
	}
	doc, err := parseSetupDocument(b, r.FileExisted)
	if err != nil || doc.shape != r.EnvShape || !doc.matches(r.OriginalEnv) {
		return setupDocument{}, setupError("setup_unavailable", ErrUnavailable)
	}
	return doc, nil
}
func (m *Manager) readSetupState() (diskState, error) {
	dir, err := openSetupDirectory(m.cfg.DataRoot, false)
	if errors.Is(err, errSetupMissing) {
		return diskState{}, nil
	}
	if err != nil {
		return diskState{}, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil || info.Mode().Perm() != 0700 {
		return diskState{}, ErrUnavailable
	}
	s, _, err := loadState(&privateStore{dir: dir})
	return s, err
}
