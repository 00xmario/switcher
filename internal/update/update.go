// Package update implements Switcher's self-update flow: check the newest
// published version, and on demand download the release binary, swap it in
// place, and exec back into the new build.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Repo is the GitHub slug the releases live in.
const Repo = "00xmario/switcher"

// State carries the cached update check so the UI can render it.
type State struct {
	Latest    string `json:"latest,omitempty"`
	Available bool   `json:"update_available"`
}

// Checker caches the latest known release and refreshes it on demand.
type Checker struct {
	Current string

	mu              sync.Mutex
	latest          string
	lastCheck       time.Time // last successful metadata check
	inFlight        *metadataCall
	metadataFetch   func(context.Context) (string, error) // fixture hook, set before use
	metadataTimeout time.Duration                         // fixture timeout, capped at 20s
	installMu       sync.Mutex
}

const metadataCheckTimeout = 20 * time.Second

type metadataCall struct {
	done  chan struct{}
	state State
	err   error
}

// validTag accepts only release tags of the form vX.Y.Z so a hostile
// release tag can never be interpreted by gh as a flag.
var validTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// New creates a checker for the running version.
func New(current string) *Checker { return &Checker{Current: current} }

// State reports the cached check. A stale cache never blocks the caller:
// a background refresh is triggered instead, so the request path stays
// free of network and subprocess work.
func (c *Checker) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.stateLocked()
	if time.Since(c.lastCheck) > 6*time.Hour {
		c.startCheckLocked()
	}
	return state
}

// Check performs a fresh metadata check or waits for one already in progress.
// A shared check is bounded to 20 seconds. Canceling a caller stops that caller's
// wait without canceling other waiters. On failure, the returned State is the
// last-good cache and err is non-nil; freshness advances only after success.
// Check never downloads a release asset, installs a binary, or restarts.
func (c *Checker) Check(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return c.checkCanceled(err)
	}
	c.mu.Lock()
	call := c.startCheckLocked()
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return c.checkCanceled(ctx.Err())
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return c.checkCanceled(err)
		}
		return call.state, call.err
	}
}

func (c *Checker) stateLocked() State {
	return State{Latest: c.latest, Available: c.latest != "" && compare(c.latest, c.Current) > 0}
}

func (c *Checker) checkCanceled(err error) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateLocked(), fmt.Errorf("check for updates: %w", err)
}

// startCheckLocked only allocates the shared call under mu. The worker performs
// all subprocess and HTTP work without holding the cache mutex.
func (c *Checker) startCheckLocked() *metadataCall {
	if c.inFlight != nil {
		return c.inFlight
	}
	call := &metadataCall{done: make(chan struct{})}
	c.inFlight = call
	fetch := c.metadataFetch
	if fetch == nil {
		fetch = fetchMetadata
	}
	timeout := c.metadataTimeout
	if timeout <= 0 || timeout > metadataCheckTimeout {
		timeout = metadataCheckTimeout
	}
	go c.runCheck(call, fetch, timeout)
	return call
}

func (c *Checker) runCheck(call *metadataCall, fetch func(context.Context) (string, error), timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	type result struct {
		tag string
		err error
	}
	results := make(chan result, 1)
	go func() {
		tag, err := fetch(ctx)
		results <- result{tag, err}
	}()
	var tag string
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case fetched := <-results:
		tag, err = fetched.tag, fetched.err
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if err == nil {
			tag, err = normalizeReleaseTag(tag)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.latest = tag
		c.lastCheck = time.Now()
	} else {
		err = fmt.Errorf("check for updates: %w", err)
	}
	call.state, call.err = c.stateLocked(), err
	c.inFlight = nil
	close(call.done)
}

// RefreshOnce preserves the legacy best-effort API: an already-running check
// is skipped and errors are ignored. Explicit callers should use Check.
func (c *Checker) RefreshOnce() {
	c.mu.Lock()
	if c.inFlight != nil {
		c.mu.Unlock()
		return
	}
	call := c.startCheckLocked()
	c.mu.Unlock()
	<-call.done
}

// Refresh preserves the legacy synchronous, best-effort API.
func (c *Checker) Refresh() {
	_, _ = c.Check(context.Background())
}

func normalizeReleaseTag(raw string) (string, error) {
	tag := strings.TrimSpace(raw)
	if !validTag.MatchString(tag) {
		return "", errors.New("invalid release version tag; expected vX.Y.Z")
	}
	return "v" + strings.TrimPrefix(tag, "v"), nil
}

func fetchMetadata(ctx context.Context) (string, error) {
	readTag := func(ctx context.Context) ([]byte, error) {
		ghCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ghCtx, "gh", "api", "repos/"+Repo+"/releases/latest", "--jq", ".tag_name")
		cmd.WaitDelay = time.Second
		return cmd.Output()
	}
	return fetchReleaseMetadata(ctx, readTag, &http.Client{Timeout: 10 * time.Second})
}

// fetchReleaseMetadata exposes only the metadata I/O boundary to fixtures.
// It tries the release API first, then reads and validates the whole VERSION
// response, rejecting oversized content instead of accepting a prefix.
func fetchReleaseMetadata(ctx context.Context, readTag func(context.Context) ([]byte, error), client *http.Client) (string, error) {
	out, ghErr := readTag(ctx)
	if ghErr == nil {
		var tag string
		tag, ghErr = normalizeReleaseTag(string(out))
		if ghErr == nil {
			return tag, nil
		}
	}
	ghErr = fmt.Errorf("GitHub release lookup: %w", ghErr)
	if err := ctx.Err(); err != nil {
		return "", errors.Join(ghErr, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://raw.githubusercontent.com/"+Repo+"/main/VERSION", nil)
	if err != nil {
		return "", errors.Join(ghErr, fmt.Errorf("VERSION fallback: %w", err))
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Join(ghErr, fmt.Errorf("VERSION fallback: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.Join(ghErr, fmt.Errorf("VERSION fallback: HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65))
	if err != nil {
		return "", errors.Join(ghErr, fmt.Errorf("VERSION fallback: %w", err))
	}
	if len(body) > 64 {
		return "", errors.Join(ghErr, errors.New("VERSION fallback: response exceeds 64 bytes"))
	}
	tag, err := normalizeReleaseTag(string(body))
	if err != nil {
		return "", errors.Join(ghErr, fmt.Errorf("VERSION fallback: %w", err))
	}
	return tag, nil
}

// Latest returns the cached latest tag.
func (c *Checker) Latest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// compare returns >0 when a is newer than b (semver-ish, v tolerated).
func compare(a, b string) int {
	a, b = strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")
	if a == b {
		return 0
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		ai, bi := part(as, i), part(bs, i)
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

func part(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
	if err != nil {
		return 0
	}
	return n
}

// InstallAndRestart downloads the latest release for this platform,
// smoke-tests it, swaps the running binary, and exec-restarts the server.
// The process image is replaced, so this call never returns.
func (c *Checker) InstallAndRestart() error {
	return c.installAndRestart(installOps{
		tempDir: os.MkdirTemp,
		download: func(ctx context.Context, tag, asset, dir string) error {
			if out, err := exec.CommandContext(ctx, "gh", "release", "download", tag,
				"--repo", Repo, "--pattern", asset, "--pattern", asset+".sha256",
				"--dir", dir).CombinedOutput(); err != nil {
				return fmt.Errorf("download release: %s (%w)", strings.TrimSpace(string(out)), err)
			}
			return nil
		},
		smoke: func(ctx context.Context, path string) ([]byte, error) {
			return exec.CommandContext(ctx, path, "version").CombinedOutput()
		},
		executable: os.Executable,
		rename:     os.Rename,
		chmod:      os.Chmod,
		syncDir:    syncDirectory,
		exec: func(path string) error {
			time.Sleep(300 * time.Millisecond) // let the HTTP response flush
			return syscall.Exec(path, os.Args, os.Environ())
		},
	})
}

// installOps keeps download, process execution, and filesystem fault injection
// at the updater boundary. Tests supply every hook and only fixture paths.
type installOps struct {
	tempDir    func(string, string) (string, error)
	download   func(context.Context, string, string, string) error
	smoke      func(context.Context, string) ([]byte, error)
	executable func() (string, error)
	rename     func(string, string) error
	chmod      func(string, os.FileMode) error
	syncDir    func(string) error
	exec       func(string) error
}

func (c *Checker) installAndRestart(ops installOps) error {
	c.installMu.Lock()
	defer c.installMu.Unlock()
	tag := c.Latest()
	if tag == "" {
		return errors.New("no update known; check for updates first")
	}
	if !validTag.MatchString(tag) {
		return fmt.Errorf("refusing to install malformed release tag %q", tag)
	}
	if compare(tag, c.Current) <= 0 {
		return fmt.Errorf("release %s is not newer than current version %s", tag, c.Current)
	}
	latest := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("switcher-server_%s_%s_%s", latest, runtime.GOOS, runtime.GOARCH)
	current, err := ops.executable()
	if err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}
	if current, err = filepath.EvalSymlinks(current); err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}
	parent := filepath.Dir(current)
	// Downloads and backups live on the destination filesystem. The private
	// staging directory also prevents a partially downloaded file being used.
	dir, err := ops.tempDir(parent, ".switcher-update-")
	if err != nil {
		return fmt.Errorf("prepare download: %w", err)
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := ops.download(ctx, tag, asset, dir); err != nil {
		return err
	}
	binaryPath := filepath.Join(dir, asset)

	if err := verifyChecksum(binaryPath, asset); err != nil {
		return err
	}

	// The exec bit: downloaded files are not executable.
	if err := ops.chmod(binaryPath, 0o755); err != nil {
		return fmt.Errorf("prepare binary: %w", err)
	}

	// Smoke test before replacing a working install.
	smokeCtx, smokeCancel := context.WithTimeout(ctx, 15*time.Second)
	defer smokeCancel()
	out, err := ops.smoke(smokeCtx, binaryPath)
	if err != nil {
		return fmt.Errorf("downloaded binary failed to start: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	if got := strings.TrimSpace(string(out)); got != "switcher "+latest {
		return fmt.Errorf("downloaded binary version %q does not match release %s", got, latest)
	}
	// A smoke-tested binary must still have the verified release bytes.
	if err := verifyChecksum(binaryPath, asset); err != nil {
		return err
	}
	if err := syncFile(binaryPath); err != nil {
		return fmt.Errorf("prepare binary: %w", err)
	}

	// Copy the original without removing its live path. Publish the durable
	// backup first, then atomically rename the staged binary over the original.
	backup := current + ".previous"
	backupStage := filepath.Join(dir, "previous")
	if err := copyBinary(current, backupStage); err != nil {
		return fmt.Errorf("back up binary: %w", err)
	}
	if err := ops.rename(backupStage, backup); err != nil {
		return fmt.Errorf("back up binary: %w", err)
	}
	if err := ops.syncDir(parent); err != nil {
		return fmt.Errorf("persist binary backup: %w", err)
	}
	if err := ops.rename(binaryPath, current); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	rollback := func(cause error) error {
		if err := ops.rename(backup, current); err != nil {
			return errors.Join(cause, fmt.Errorf("rollback failed; original binary retained at %s: %w", backup, err))
		}
		if err := ops.syncDir(parent); err != nil {
			return errors.Join(cause, fmt.Errorf("original binary restored at %s but rollback sync failed: %w", current, err))
		}
		return cause
	}
	if err := ops.syncDir(parent); err != nil {
		return rollback(fmt.Errorf("persist binary replacement: %w", err))
	}
	// Successful exec does not run defers. Remove the now-unused staging
	// directory while rollback can still use the published backup.
	if err := os.RemoveAll(dir); err != nil {
		return rollback(fmt.Errorf("clean update staging directory: %w", err))
	}

	log.Printf("update: installed %s, restarting", latest)
	if err := ops.exec(current); err != nil {
		return rollback(fmt.Errorf("restart updated binary: %w", err))
	}
	return nil
}

func verifyChecksum(binaryPath, asset string) error {
	expected, err := os.ReadFile(binaryPath + ".sha256")
	if err != nil {
		return fmt.Errorf("download incomplete: read %s.sha256: %w", asset, err)
	}
	entry := strings.TrimSpace(string(expected))
	fields := strings.Fields(entry)
	if strings.ContainsAny(entry, "\r\n") || len(fields) < 1 || len(fields) > 2 {
		return fmt.Errorf("invalid checksum for %s: expected exactly one SHA-256 entry", asset)
	}
	want, err := hex.DecodeString(fields[0])
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("invalid SHA-256 digest for %s", asset)
	}
	if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") != asset {
		return fmt.Errorf("checksum names a different asset than %s", asset)
	}
	sum, err := sha256sum(binaryPath)
	if err != nil {
		return fmt.Errorf("read downloaded binary: %w", err)
	}
	if sum != hex.EncodeToString(want) {
		return fmt.Errorf("checksum mismatch for %s", asset)
	}
	return nil
}

func copyBinary(source, destination string) error {
	src, err := os.Open(source)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return dst.Close()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// sha256sum hex-digests a file.
func sha256sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
