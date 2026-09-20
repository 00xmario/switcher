// Package update implements Switcher's self-update flow: check the newest
// published version, and on demand download the release binary, swap it in
// place, and exec back into the new build.
package update

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

	mu        sync.Mutex
	latest    string
	lastCheck time.Time
}

// New creates a checker for the running version.
func New(current string) *Checker { return &Checker{Current: current} }

// State reports the cached check, refreshing it if the cache is stale.
func (c *Checker) State() State {
	c.mu.Lock()
	stale := time.Since(c.lastCheck) > 6*time.Hour
	c.mu.Unlock()
	if stale || c.latest == "" {
		c.Refresh()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	latest := c.latest
	return State{Latest: latest, Available: latest != "" && compare(latest, c.Current) > 0}
}

// Refresh re-reads the latest version: the gh CLI first (it already holds
// GitHub credentials on this machine), then the raw VERSION file, which
// works once the repository is public.
func (c *Checker) Refresh() {
	tag := ""
	if out, err := exec.Command("gh", "api", "repos/"+Repo+"/releases/latest", "--jq", ".tag_name").Output(); err == nil {
		tag = strings.TrimSpace(string(out))
	}
	if tag == "" {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(
			"https://raw.githubusercontent.com/" + Repo + "/main/VERSION")
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				buf := make([]byte, 64)
				if n, _ := resp.Body.Read(buf); n > 0 {
					tag = strings.TrimSpace(string(buf[:n]))
					if tag != "" && !strings.HasPrefix(tag, "v") {
						tag = "v" + tag
					}
				}
			}
		}
	}
	c.mu.Lock()
	if tag != "" {
		c.latest = tag
	}
	c.lastCheck = time.Now()
	c.mu.Unlock()
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
	tag := c.Latest()
	if tag == "" {
		return errors.New("no update known; check for updates first")
	}
	latest := strings.TrimPrefix(tag, "v")
	asset := fmt.Sprintf("switcher-server_%s_%s_%s", latest, runtime.GOOS, runtime.GOARCH)
	dir, err := os.MkdirTemp("", "switcher-update-")
	if err != nil {
		return fmt.Errorf("prepare download: %w", err)
	}
	if out, err := exec.Command("gh", "release", "download", tag,
		"--repo", Repo, "--pattern", asset, "--dir", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("download release: %s (%v)", strings.TrimSpace(string(out)), err)
	}
	binaryPath := filepath.Join(dir, asset)

	// The exec bit: downloaded files are not executable.
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		return fmt.Errorf("prepare binary: %w", err)
	}

	// Smoke test before replacing a working install.
	if out, err := exec.Command(binaryPath, "version").CombinedOutput(); err != nil {
		return fmt.Errorf("downloaded binary failed to start: %s (%v)", strings.TrimSpace(string(out)), err)
	}

	current, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}
	if current, err = filepath.EvalSymlinks(current); err != nil {
		return fmt.Errorf("resolve running binary: %w", err)
	}

	// Swap: move the old file aside, move the new one into place; restore
	// on failure so a botched update cannot take the tool down.
	backup := current + ".previous"
	_ = os.Remove(backup)
	if err := os.Rename(current, backup); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	if err := os.Rename(binaryPath, current); err != nil {
		_ = os.Rename(backup, current)
		return fmt.Errorf("swap binary: %w", err)
	}
	if err := os.Chmod(current, 0o755); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}

	log.Printf("update: installed %s, restarting", latest)
	time.Sleep(300 * time.Millisecond) // let the HTTP response flush
	return syscall.Exec(current, os.Args, os.Environ())
}
