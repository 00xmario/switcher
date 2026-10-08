package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Settings the command line may change. Network access and the dashboard
// password stay in the app, where their prompts and warnings are.
var settingKeys = []string{"merge_accounts", "compact_accounts", "menu_usage_bars", "reset_notifications", "auto_use_reset", "auto_switch_claude"}

func runSettings(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("settings"), args, 0, -1)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		if pos[0] != "set" || len(pos) < 2 {
			return usagef("use: switcher settings set key=value ...")
		}
		patch := map[string]bool{}
		for _, pair := range pos[1:] {
			key, value, ok := strings.Cut(pair, "=")
			if !ok || !slices.Contains(settingKeys, key) {
				return usagef("%q is not a setting; use one of %s", pair, strings.Join(settingKeys, ", "))
			}
			on, err := strconv.ParseBool(value)
			if err != nil {
				return usagef("%s takes true or false", key)
			}
			patch[key] = on
		}
		if err := c.cl.call(http.MethodPatch, "/api/settings", patch, nil); err != nil {
			return err
		}
	}
	var all map[string]any
	if err := c.cl.call(http.MethodGet, "/api/settings", nil, &all); err != nil {
		return err
	}
	shown := map[string]any{}
	for _, key := range settingKeys {
		if v, ok := all[key]; ok {
			shown[key] = v
		}
	}
	if c.json {
		c.emit(shown)
		return nil
	}
	var rows [][]string
	for _, key := range settingKeys {
		if v, ok := shown[key]; ok {
			rows = append(rows, []string{key, fmt.Sprint(v)})
		}
	}
	fmt.Fprintln(c.out, table(rows))
	return nil
}

func runSetup(c *ctx, args []string) error {
	fs := c.flags("setup")
	undo := fs.Bool("undo", false, "")
	test := fs.Bool("test", false, "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		if pos[0] != "codex" {
			return usagef("only codex is configured by Switcher; Claude Code switches its own login with: switcher use <account>")
		}
		path := "/api/cli-setup/codex/install"
		switch {
		case *undo:
			path = "/api/cli-setup/codex/restore"
		case *test:
			path = "/api/cli-setup/codex/test"
		}
		var answer map[string]any
		if err := c.cl.call(http.MethodPost, path, nil, &answer); err != nil {
			return err
		}
		human := "Codex now uses Switcher."
		if *undo {
			human = "Codex uses its previous provider again."
		} else if *test {
			// One real request through Switcher's Codex route; the Codex CLI
			// itself is not run.
			outcome, _ := answer["outcome"].(string)
			if outcome != "success" {
				return fmt.Errorf("the Switcher route test did not succeed (%s)", strings.ReplaceAll(orDefault(outcome, "unknown"), "_", " "))
			}
			human = "A test request through Switcher's Codex route succeeded."
		}
		c.result(answer, human)
		return nil
	}
	var answer struct {
		Clients []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Stage         string `json:"stage"`
			NextStep      string `json:"next_step,omitempty"`
			Configuration struct {
				Condition string `json:"condition"`
			} `json:"configuration"`
		} `json:"clients"`
	}
	if err := c.cl.call(http.MethodGet, "/api/cli-setup", nil, &answer); err != nil {
		return err
	}
	if c.json {
		c.emit(answer)
		return nil
	}
	var rows [][]string
	for _, client := range answer.Clients {
		state := client.Configuration.Condition
		if state == "" || state == "not_checked" {
			state = client.Stage
		}
		rows = append(rows, []string{client.Name, strings.ReplaceAll(state, "_", " "), client.NextStep})
	}
	fmt.Fprintln(c.out, table(rows))
	return nil
}

func runStart(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("start"), args, 0, 0); err != nil {
		return err
	}
	st, _, err := c.state()
	if err == nil {
		c.result(map[string]any{"running": true, "version": st.Version, "url": c.url}, "Switcher is already running.")
		return nil
	}
	if !errors.Is(err, errNotRunning) {
		return err
	}
	if c.opt.Start == nil {
		return errors.New("this build cannot start Switcher; open the app")
	}
	if err := c.opt.Start(); err != nil {
		return err
	}
	err = wait(20*time.Second, 250*time.Millisecond, func() (bool, error) {
		var e error
		st, _, e = c.state()
		if errors.Is(e, errNotRunning) {
			return false, nil
		}
		return e == nil, e
	})
	if err != nil {
		return fmt.Errorf("Switcher did not start: %w (see ~/.switcher/server.log)", err)
	}
	c.result(map[string]any{"running": true, "version": st.Version, "url": c.url}, fmt.Sprintf("Switcher %s is running at %s.", st.Version, c.url))
	return nil
}

func runOpen(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("open"), args, 0, 0); err != nil {
		return err
	}
	if _, _, err := c.state(); err != nil {
		return err
	}
	if c.opt.Getenv("SSH_CONNECTION") != "" {
		c.result(map[string]string{"url": c.url}, "The dashboard only opens on this Mac; from here, forward it with ssh -L 8787:127.0.0.1:8787 and open "+c.url)
		return nil
	}
	openURL(c.url)
	c.result(map[string]string{"url": c.url}, "Opened "+c.url)
	return nil
}

func runInstallCLI(c *ctx, args []string) error {
	fs := c.flags("install-cli")
	dir := fs.String("dir", "", "")
	force := fs.Bool("force", false, "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	target, err := filepath.EvalSymlinks(c.opt.Executable)
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	candidates := []string{*dir}
	if *dir == "" {
		candidates = []string{"/usr/local/bin", "/opt/homebrew/bin", filepath.Join(home, ".local", "bin")}
	}
	for i, d := range candidates {
		last := i == len(candidates)-1
		if last {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		link := filepath.Join(d, "switcher")
		if info, err := os.Lstat(link); err == nil {
			// Only a link to a Switcher binary is ours to replace; another
			// tool called switcher stays.
			current, _ := os.Readlink(link)
			ours := info.Mode()&os.ModeSymlink != 0 && (current == target || filepath.Base(current) == "SwitcherServer" || *force)
			if !ours {
				if last {
					return fmt.Errorf("%s already exists and is not Switcher; remove it, pass --force, or choose --dir", link)
				}
				continue
			}
			if os.Remove(link) != nil {
				continue
			}
		}
		if err := os.Symlink(target, link); err != nil {
			if last {
				return err
			}
			continue
		}
		onPath := slices.Contains(filepath.SplitList(c.opt.Getenv("PATH")), d)
		human := "Installed " + link
		if !onPath {
			human += fmt.Sprintf("\nAdd it to your PATH: echo 'export PATH=\"%s:$PATH\"' >> ~/.zshrc", d)
		}
		c.result(map[string]any{"path": link, "target": target, "on_path": onPath}, human)
		return nil
	}
	return errors.New("no writable directory for the switcher command")
}
