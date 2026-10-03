package settings

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// Pin every directory component without following user-controlled symlinks.
// Child operations remain relative to the descriptor even if its path moves.
func openPrivateDirectory(path string, create bool) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, errors.New("settings directory must be absolute")
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp", "/etc"} {
			if path == alias || strings.HasPrefix(path, alias+"/") {
				path = "/private" + path
				break
			}
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, part, 0o700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || int(info.Uid) != os.Getuid() {
		unix.Close(fd)
		return -1, errors.New("settings directory must be owned by this user")
	}
	return fd, nil
}

func ensurePrivateDirectory(path string) error {
	fd, err := openPrivateDirectory(path, true)
	if err == nil {
		unix.Close(fd)
	}
	return err
}

func readPrivateFile(path string) ([]byte, error) {
	dir, err := openPrivateDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || unix.Fstat(fd, &stat) != nil || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("settings credential must be a private user-owned regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if len(raw) > 4<<20 {
		return nil, errors.New("settings credential file is too large")
	}
	return raw, err
}

func checkTarget(dir int, name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing unsafe settings target %s", name)
	}
	return nil
}

func writeAtomic(path string, raw []byte) error {
	dir, err := openPrivateDirectory(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	name := filepath.Base(path)
	if err := checkTarget(dir, name); err != nil {
		return err
	}
	tmp := ".switcher-settings-" + rand.Text()
	fd, err := unix.Openat(dir, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dir, tmp, 0)
	f := os.NewFile(uintptr(fd), tmp)
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := checkTarget(dir, name); err != nil {
		return err
	}
	if err := unix.Renameat(dir, tmp, dir, name); err != nil {
		return err
	}
	return unix.Fsync(dir)
}

func removePrivateFile(path string) error {
	dir, err := openPrivateDirectory(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	err = unix.Unlinkat(dir, filepath.Base(path), 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}
