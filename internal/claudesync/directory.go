package claudesync

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// openDirectory acquires the initial directory handle without a check/open
// race. Every component is opened relative to its pinned parent and refuses
// symlinks. Creation is likewise relative to that parent, not a mutable path.
func openDirectory(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("sync directory must be absolute")
	}
	path = filepath.Clean(path)
	// macOS's system-owned aliases are fixed, not user-controlled links.
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp"} {
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
	defer func() { unix.Close(fd) }()
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" {
			continue
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && create {
			if err = unix.Mkdirat(fd, component, 0700); err != nil && err != unix.EEXIST {
				return nil, err
			}
			next, err = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("open sync directory component %s: %w", component, err)
		}
		unix.Close(fd)
		fd = next
	}
	// /dev/fd is the OS descriptor namespace. OpenRoot duplicates our pinned
	// directory, never re-resolving the caller's path. The fd stays open until
	// the independent Root handle has been acquired.
	return os.OpenRoot(fmt.Sprintf("/dev/fd/%d", fd))
}
