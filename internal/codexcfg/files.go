package codexcfg

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// configFiles pins the directory for the duration of an operation. All
// child opens and renames are relative to that descriptor, never to a path
// that could have been swapped after validation.
type configFiles struct {
	dir  int
	lock int
	base string
	path string
}

type expectedFile struct {
	digest string
	exists bool
}

func canonicalSystemAlias(path string) string {
	for _, alias := range []string{"/var", "/tmp", "/etc"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			if resolved, err := filepath.EvalSymlinks(alias); err == nil {
				return filepath.Join(resolved, strings.TrimPrefix(strings.TrimPrefix(path, alias), "/"))
			}
		}
	}
	return path
}

func openConfigFiles(path string, create, locked bool) (*configFiles, error) {
	if !filepath.IsAbs(path) || filepath.Base(path) != "config.toml" {
		return nil, fmt.Errorf("%w: unsupported Codex config path", ErrConfigConflict)
	}
	path = canonicalSystemAlias(filepath.Clean(path))
	directory := filepath.Dir(path)
	dir, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(directory, "/"), "/") {
		if part == "" {
			continue
		}
		child, openErr := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && create {
			if err = unix.Mkdirat(dir, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(dir)
				return nil, err
			}
			if err == nil {
				_ = unix.Fsync(dir)
			}
			child, openErr = unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(dir)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return nil, os.ErrNotExist
			}
			return nil, fmt.Errorf("%w: Codex config directory: %v", ErrConfigConflict, openErr)
		}
		dir = child
	}
	var info unix.Stat_t
	if err := unix.Fstat(dir, &info); err != nil || int(info.Uid) != os.Getuid() {
		unix.Close(dir)
		return nil, fmt.Errorf("%w: Codex config directory is not owned by this user", ErrConfigConflict)
	}
	f := &configFiles{dir: dir, lock: -1, base: filepath.Base(path), path: path}
	if locked {
		f.lock, err = unix.Openat(dir, ".switcher.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			err = f.checkFile(f.lock, true)
		}
		if err == nil {
			err = unix.Flock(f.lock, unix.LOCK_EX)
		}
		if err != nil {
			f.close()
			return nil, fmt.Errorf("%w: Codex config lock: %v", ErrConfigConflict, err)
		}
	}
	return f, nil
}

func (f *configFiles) close() {
	if f.lock >= 0 {
		_ = unix.Flock(f.lock, unix.LOCK_UN)
		_ = unix.Close(f.lock)
	}
	_ = unix.Close(f.dir)
}

func (f *configFiles) checkFile(fd int, private bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: expected a user-owned regular file", ErrConfigConflict)
	}
	if private && st.Mode&0o077 != 0 {
		return fmt.Errorf("%w: Switcher metadata is not private", ErrConfigConflict)
	}
	return nil
}

func (f *configFiles) read(name string, private bool) ([]byte, bool, error) {
	fd, err := unix.Openat(f.dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: read %s: %v", ErrConfigConflict, name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := f.checkFile(fd, private); err != nil {
		return nil, false, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4<<20+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, false, fmt.Errorf("%w: config file too large or unreadable", ErrConfigConflict)
	}
	return raw, true, nil
}

func (f *configFiles) temp(data []byte) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := ".switcher-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(f.dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(data); err != nil {
		file.Close()
		_ = unix.Unlinkat(f.dir, name, 0)
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = unix.Unlinkat(f.dir, name, 0)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = unix.Unlinkat(f.dir, name, 0)
		return "", err
	}
	return name, nil
}

func (f *configFiles) replace(name string, data []byte, expected ...expectedFile) error {
	if _, exists, err := f.read(name, name != f.base); err != nil {
		return err
	} else if exists && name == ".switcher.lock" {
		return fmt.Errorf("%w: lock cannot be replaced", ErrConfigConflict)
	}
	tmp, err := f.temp(data)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(f.dir, tmp, 0)
	if name == f.base {
		if err := codexFault("before_rename"); err != nil {
			return err
		}
	}
	if len(expected) > 0 {
		current, present, err := f.read(name, name != f.base)
		if err != nil || present != expected[0].exists || digest(current) != expected[0].digest {
			return fmt.Errorf("%w: %s changed before rename", ErrConfigConflict, name)
		}
	}
	if err := unix.Renameat(f.dir, tmp, f.dir, name); err != nil {
		return err
	}
	return unix.Fsync(f.dir)
}

func (f *configFiles) remove(name string, expected ...expectedFile) error {
	if current, exists, err := f.read(name, name != f.base); err != nil {
		return err
	} else if !exists {
		if len(expected) > 0 && expected[0].exists {
			return fmt.Errorf("%w: %s disappeared", ErrConfigConflict, name)
		}
		return nil
	} else if len(expected) > 0 && (!expected[0].exists || digest(current) != expected[0].digest) {
		return fmt.Errorf("%w: %s changed before removal", ErrConfigConflict, name)
	}
	if len(expected) > 0 {
		if err := codexFault("before_unlink"); err != nil {
			return err
		}
		current, present, err := f.read(name, name != f.base)
		if err != nil || !present || digest(current) != expected[0].digest {
			return fmt.Errorf("%w: %s changed before unlink", ErrConfigConflict, name)
		}
	}
	if err := unix.Unlinkat(f.dir, name, 0); err != nil {
		return err
	}
	return unix.Fsync(f.dir)
}

func (f *configFiles) backupOnce(data []byte) error {
	name := f.base + ".switcher-backup"
	if _, exists, err := f.read(name, true); err != nil || exists {
		return err
	}
	tmp, err := f.temp(data)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(f.dir, tmp, 0)
	if err := unix.Linkat(f.dir, tmp, f.dir, name, 0); err != nil {
		return fmt.Errorf("%w: backup: %v", ErrConfigConflict, err)
	}
	return unix.Fsync(f.dir)
}
