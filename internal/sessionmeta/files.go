package sessionmeta

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	maxFile    = 1 << 20
	maxBytes   = 32 << 20
	maxEntries = 4096
)

type budget struct {
	ctx     context.Context
	entries int
	bytes   int64
}

func newBudget(ctx context.Context) *budget {
	return &budget{ctx: ctx, entries: maxEntries, bytes: maxBytes}
}

func (b *budget) active() bool {
	return b.ctx.Err() == nil && b.entries > 0 && b.bytes > 0
}

func safeDirectory(st *unix.Stat_t, own bool) bool {
	uid := uint32(os.Getuid())
	return st.Mode&unix.S_IFMT == unix.S_IFDIR &&
		(st.Uid == uid || !own && st.Uid == 0) &&
		(st.Mode&0022 == 0 || !own && st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)
}

// Pin every path component without following symlinks. Root-owned sticky temp
// ancestors are permitted; the metadata root and descendants must be user owned.
func openRoot(path string, b *budget) (*os.File, bool) {
	if path == "" {
		return nil, true
	}
	if !b.active() || len(path) > 4096 || !filepath.IsAbs(path) || path == "/" || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return nil, false
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for j, part := range parts {
		if !b.active() {
			unix.Close(fd)
			return nil, false
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			// A configured root that does not exist is optional absence.
			return nil, err == unix.ENOENT
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || !safeDirectory(&st, j == len(parts)-1) {
			unix.Close(fd)
			return nil, false
		}
	}
	return os.NewFile(uintptr(fd), path), true
}

func openChild(dir *os.File, name string) (*os.File, bool) {
	var named unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return nil, false
	}
	if named.Mode&unix.S_IFMT == unix.S_IFLNK {
		// A symlink could conceal an account/workspace containing conflicts.
		return nil, false
	}
	if named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, true // Ordinary files are not account/workspace directories.
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || !safeDirectory(&st, true) || st.Dev != named.Dev || st.Ino != named.Ino {
		unix.Close(fd)
		return nil, false
	}
	return os.NewFile(uintptr(fd), name), true
}

// Fixed-depth traversal visits only the documented account/workspace or project
// directories. Chunked enumeration counts all entries, including rejected ones.
// Every accepted unreadable/unsafe file reaches reject. A false result means
// some directory could not be fully inspected, so it cannot establish authority.
// Safe display metadata can still be visited during an incomplete scan.
func walkMetadata(path string, depth int, b *budget, accept func(string) bool, visit func(string, []byte), reject func(string)) bool {
	root, complete := openRoot(path, b)
	if root == nil {
		return complete
	}
	defer root.Close()
	var walk func(*os.File, int) bool
	walk = func(dir *os.File, remaining int) bool {
		complete := true
		for b.active() {
			names, err := dir.Readdirnames(1)
			if len(names) == 0 {
				return complete && err == io.EOF
			}
			b.entries--
			name := names[0]
			if remaining > 0 {
				child, inspected := openChild(dir, name)
				if !inspected {
					complete = false
				}
				if child != nil {
					if !walk(child, remaining-1) {
						complete = false
					}
					child.Close()
				}
			} else if accept(name) {
				if data := readMetadata(dir, name, b); data != nil {
					visit(name, data)
				} else if reject != nil {
					reject(name)
				}
			}
			if err != nil {
				return complete && err == io.EOF
			}
		}
		return false // Budget exhaustion or cancellation before confirmed EOF.
	}
	return walk(root, depth)
}

func safeFile(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Uid == uint32(os.Getuid()) && st.Nlink == 1 && st.Mode&0022 == 0 && st.Size >= 0 && st.Size <= maxFile
}

func readMetadata(dir *os.File, name string, b *budget) []byte {
	if b.ctx.Err() != nil || b.bytes <= 0 {
		return nil
	}
	// Check type before opening, then verify the opened descriptor. Nonblocking
	// opens also protect against a file being replaced by a FIFO between checks.
	var named unix.Stat_t
	if unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeFile(&named) {
		return nil
	}
	if named.Size > b.bytes {
		b.bytes = 0
		return nil
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || !safeFile(&st) || st.Dev != named.Dev || st.Ino != named.Ino || st.Size > b.bytes {
		return nil
	}
	before, err := f.Stat()
	if err != nil {
		return nil
	}
	limit := int64(maxFile + 1)
	if b.bytes < limit {
		limit = b.bytes
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	b.bytes -= int64(len(data))
	if err != nil || b.ctx.Err() != nil || len(data) > maxFile || int64(len(data)) != st.Size {
		return nil
	}
	after, err := f.Stat()
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW) != nil || !safeFile(&named) || named.Dev != st.Dev || named.Ino != st.Ino {
		return nil
	}
	return data
}
