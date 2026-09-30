// Package claudesync shares Claude Desktop discovery pointers, following the
// user's claude-sync script: quit, back up local_*.json, copy missing pointers,
// reopen. Actual transcripts in ~/.claude/projects are never accessed.
package claudesync

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Options struct {
	Root, BackupDir string
	Labels          map[string]string
	Quit, Reopen    func() error
	Wait            func(time.Duration)
	Now             func() time.Time
}

type AccountResult struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Dir   string `json:"dir"`
	Added int    `json:"added"`
	Total int    `json:"total"`
}

type Result struct {
	Added    int             `json:"added"`
	Backup   string          `json:"backup"`
	Accounts []AccountResult `json:"accounts"`
}

var ErrNoAccounts = errors.New("fewer than two Claude Desktop account indexes found")

func SessionIndexRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Claude", "claude-code-sessions"), nil
}

func DefaultBackupDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "desktop-session-sync-backups"), nil
}

type indexDir struct {
	account, relative string
	root              *os.Root
	names             []string
}

func closeIndexes(dirs []indexDir) {
	for _, dir := range dirs {
		dir.root.Close()
	}
}

// Directory handles constrain every later operation even if a namespace is
// replaced with a symlink while Claude exits. Empty workspaces participate.
func discover(root *os.Root) (dirs []indexDir, err error) {
	defer func() {
		if err != nil {
			closeIndexes(dirs)
		}
	}()
	accounts, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	for _, account := range accounts {
		if account.Type()&os.ModeSymlink != 0 {
			return dirs, errors.New("account index must not be a symlink")
		}
		if !account.IsDir() {
			continue
		}
		workspaces, err := fs.ReadDir(root.FS(), account.Name())
		if err != nil {
			return dirs, err
		}
		for _, workspace := range workspaces {
			if workspace.Type()&os.ModeSymlink != 0 {
				return dirs, errors.New("workspace index must not be a symlink")
			}
			if !workspace.IsDir() {
				continue
			}
			relative := filepath.Join(account.Name(), workspace.Name())
			handle, err := root.OpenRoot(relative)
			if err != nil {
				return dirs, err
			}
			dirs = append(dirs, indexDir{account: account.Name(), relative: relative, root: handle})
		}
	}
	identities := map[string]bool{}
	for _, dir := range dirs {
		identities[dir.account] = true
	}
	if len(identities) < 2 {
		return dirs, ErrNoAccounts
	}
	return dirs, nil
}

func sessionFiles(root *os.Root) ([]string, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		matched, _ := filepath.Match("local_*.json", entry.Name())
		if !matched {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("session pointer is not a regular file: %s", entry.Name())
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func Sync(ctx context.Context, opts Options) (result Result, runErr error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Root == "" || opts.BackupDir == "" {
		return result, errors.New("sync paths are required")
	}
	root, err := openDirectory(opts.Root, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return result, ErrNoAccounts
		}
		return result, err
	}
	defer root.Close()
	// Preflight avoids closing Desktop when there is no second account.
	preflight, err := discover(root)
	if err != nil {
		return result, err
	}
	closeIndexes(preflight)
	backups, err := openDirectory(opts.BackupDir, true)
	if err != nil {
		return result, err
	}
	defer backups.Close()
	// Install cleanup before asking Desktop to quit: cancellation may occur
	// after it receives the quit event but before it confirms exit.
	defer func() {
		if opts.Reopen != nil {
			if err := opts.Reopen(); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf("reopen Claude Desktop: %w", err))
			}
		}
	}()
	if opts.Quit != nil {
		if err := opts.Quit(); err != nil {
			return result, fmt.Errorf("close Claude Desktop: %w", err)
		}
	}
	if opts.Wait != nil {
		opts.Wait(2 * time.Second)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// The authoritative directory list includes indexes flushed during quit.
	dirs, err := discover(root)
	if err != nil {
		return result, err
	}
	defer closeIndexes(dirs)
	for i := range dirs {
		dirs[i].names, err = sessionFiles(dirs[i].root)
		if err != nil {
			return result, err
		}
	}
	now := time.Now()
	if opts.Now != nil {
		now = opts.Now()
	}
	backupName := now.Format("20060102-150405") + "-" + randomName()
	if err := backups.Mkdir(backupName, 0700); err != nil {
		return result, err
	}
	result.Backup = filepath.Join(opts.BackupDir, backupName)
	backup, err := backups.OpenRoot(backupName)
	if err != nil {
		return result, err
	}
	defer backup.Close()
	sources := map[string]string{}
	used := map[string]int{}
	// Complete every backup before any destination index is changed. Merge
	// reads those immutable snapshots, not files Desktop might later reopen.
	for _, dir := range dirs {
		label := dir.account
		used[label]++
		if used[label] > 1 {
			label = fmt.Sprintf("%s-%d", label, used[label])
		}
		if err := backup.Mkdir(label, 0700); err != nil {
			return result, err
		}
		for _, name := range dir.names {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			dest := filepath.Join(label, name)
			if _, err := copyPointer(dir.root, name, backup, dest); err != nil {
				return result, fmt.Errorf("back up %s: %w", name, err)
			}
			if _, exists := sources[name]; !exists {
				sources[name] = dest
			}
		}
	}
	order := make([]string, 0, len(sources))
	for name := range sources {
		order = append(order, name)
	}
	sort.Strings(order)
	for _, dir := range dirs {
		result.Accounts = append(result.Accounts, AccountResult{ID: dir.account, Label: labelFor(dir.account, opts.Labels), Dir: filepath.Join(opts.Root, dir.relative), Total: len(dir.names)})
		account := &result.Accounts[len(result.Accounts)-1]
		have := map[string]bool{}
		for _, name := range dir.names {
			have[name] = true
		}
		for _, name := range order {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if have[name] {
				continue
			}
			copied, err := copyPointer(backup, sources[name], dir.root, name)
			if err != nil {
				return result, fmt.Errorf("copy %s into %s: %w", name, dir.account, err)
			}
			if copied {
				account.Added++
				account.Total++
				result.Added++
			}
		}
	}
	sort.SliceStable(result.Accounts, func(i, j int) bool { return result.Accounts[i].Label < result.Accounts[j].Label })
	return result, nil
}

func randomName() string { return rand.Text() }

// copyPointer publishes a complete file with an atomic no-overwrite link.
// Root operations cannot follow a link out of the opened directory tree.
func copyPointer(src *os.Root, name string, dst *os.Root, target string) (bool, error) {
	info, err := src.Lstat(name)
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("session pointer must be a regular file")
	}
	in, err := src.Open(name)
	if err != nil {
		return false, err
	}
	defer in.Close()
	info, err = in.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("session pointer changed during copy")
	}
	temp := filepath.Join(filepath.Dir(target), ".switcher-sync-"+randomName())
	out, err := dst.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer dst.Remove(temp)
	defer out.Close()
	if _, err = io.Copy(out, in); err != nil {
		return false, err
	}
	if err = out.Sync(); err != nil {
		return false, err
	}
	if err = out.Close(); err != nil {
		return false, err
	}
	if err = dst.Chtimes(temp, info.ModTime(), info.ModTime()); err != nil {
		return false, err
	}
	if err = dst.Link(temp, target); os.IsExist(err) {
		return false, nil
	}
	return err == nil, err
}

func labelFor(id string, labels map[string]string) string {
	if name := labels[id]; name != "" {
		return name
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func Describe(r Result) string {
	if r.Added == 0 {
		return "No new Claude Code sessions to share"
	}
	parts := []string{}
	for _, account := range r.Accounts {
		if account.Added > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", account.Label, account.Added))
		}
	}
	word := "sessions"
	if r.Added == 1 {
		word = "session"
	}
	message := fmt.Sprintf("%d new %s added", r.Added, word)
	if len(parts) > 0 {
		message += " (" + strings.Join(parts, ", ") + ")"
	}
	return message
}
