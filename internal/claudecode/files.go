package claudecode

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type fileValue struct {
	Data   []byte `json:"data"`
	Exists bool   `json:"exists"`
}

// Tests inject failures at the directory durability boundary, without touching
// live stores or simulating a crash of the user's machine.
var syncDirectoryFD = unix.Fsync

const nativeFileLimit = 2 << 20

// A journal contains two snapshots of up to four native stores, base64-encoded.
// Keep aggregate recovery bounded without applying the single-store limit.
const recoveryFileLimit = 32 << 20

func openDirectory(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("native Claude paths must be absolute")
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
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		created := false
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, component, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, err
			}
			created = true
			next, err = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, errors.New("native Claude directory is missing, inaccessible, or redirected")
		}
		if create {
			// Sync existing user-owned ancestry too: a previous failed mkdir
			// sync, or a directory created by the account store, may be visible
			// without its name yet being durable.
			var child, parent unix.Stat_t
			if err := unix.Fstat(next, &child); err != nil {
				_ = unix.Close(next)
				return nil, err
			}
			if err := unix.Fstat(fd, &parent); err != nil {
				_ = unix.Close(next)
				return nil, err
			}
			if created || int(child.Uid) == os.Getuid() {
				if err := syncDirectoryFD(next); err != nil {
					_ = unix.Close(next)
					return nil, err
				}
			}
			if created || int(parent.Uid) == os.Getuid() {
				if err := syncDirectoryFD(fd); err != nil {
					_ = unix.Close(next)
					return nil, err
				}
			}
		}
		_ = unix.Close(fd)
		fd = next
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || int(info.Uid) != os.Getuid() {
		return nil, errors.New("native Claude directory is not user-owned")
	}
	return os.OpenRoot("/dev/fd/" + intText(fd))
}

func safeDirectory(path string, create bool) error {
	root, err := openDirectory(path, create)
	if err != nil {
		return err
	}
	return root.Close()
}

func syncPrivateDirectory(path string) error {
	root, err := openDirectory(path, true)
	if err != nil {
		return err
	}
	defer root.Close()
	return syncRoot(root)
}

func syncRoot(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return syncDirectoryFD(int(dir.Fd()))
}

func readPrivate(path string) (fileValue, error) {
	return readPrivateLimit(path, nativeFileLimit)
}

func readRecovery(path string) (fileValue, error) {
	return readPrivateLimit(path, recoveryFileLimit)
}

func readPrivateLimit(path string, limit int64) (fileValue, error) {
	root, err := openDirectory(filepath.Dir(path), false)
	if err != nil {
		if _, e := os.Lstat(filepath.Dir(path)); os.IsNotExist(e) {
			return fileValue{}, nil
		}
		return fileValue{}, err
	}
	defer root.Close()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return fileValue{}, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return fileValue{}, errors.New("native Claude file is not a readable regular file")
	}
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fileValue{}, errors.New("cannot read native Claude file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return fileValue{}, errors.New("native Claude file is too large or unreadable")
	}
	return fileValue{Data: data, Exists: true}, nil
}

func writePrivate(path string, value fileValue) error {
	return writePrivateLimit(path, value, nativeFileLimit)
}

func writeRecovery(path string, value fileValue) error {
	return writePrivateLimit(path, value, recoveryFileLimit)
}

func writePrivateLimit(path string, value fileValue, limit int) error {
	if value.Exists && len(value.Data) > limit {
		return errors.New("native Claude file exceeds its bounded storage limit")
	}
	root, err := openDirectory(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	if info, err := root.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return ErrConflict
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !value.Exists {
		if err := root.Remove(name); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
		}
		return syncRoot(root)
	}
	temp := ".switcher-" + rand.Text() + ".tmp"
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	if _, err = file.Write(value.Data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	return syncRoot(root)
}
