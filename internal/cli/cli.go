// Package cli is Switcher's command line: a thin client for the running
// Switcher server, so everything the app does can be done from a terminal,
// over SSH or by an agent. Every command takes --json for machine-readable
// output and exits 0 on success, 1 on failure, 2 on a usage error and 3 when
// Switcher is not running.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Options connect the command line to its process.
type Options struct {
	Port          int
	Version       string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	Getenv        func(string) string
	Interactive   bool // stdin and stdout are a terminal
	StdinTerminal bool
	// DeviceToken returns the dashboard device token, needed only while a
	// dashboard password is set.
	DeviceToken func() string
	// Start launches the Switcher server in the background.
	Start func() error
	// CallbackPorts are the local OAuth redirect ports a pasted sign-in
	// address may point at.
	CallbackPorts []int
	// Executable is this binary, for install-cli.
	Executable string
}

const (
	exitOK         = 0
	exitFail       = 1
	exitUsage      = 2
	exitNotRunning = 3
)

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return usageError{fmt.Sprintf(format, args...)} }

var errNotRunning = errors.New("Switcher is not running; start it with: switcher start")

type command struct {
	name, args, summary, help string
	run                       func(c *ctx, args []string) error
}

// The order here is the order of `switcher help`.
var commands []*command

func init() {
	commands = []*command{
		{name: "status", summary: "Show accounts, usage, sharing and Claude Desktop", run: runStatus},
		{name: "accounts", args: "[--provider P]", summary: "List accounts with their usage", run: runAccounts},
		{name: "use", args: "<account>", summary: "Use an account for its provider (Claude also switches Claude Code)", run: runUse},
		{name: "login", args: "<provider>", summary: "Sign in to a provider and add the account", help: loginHelp, run: runLogin},
		{name: "add-key", args: "<provider> [--key K]", summary: "Add an API-key account (key from --key or stdin)", run: runAddKey},
		{name: "remove", args: "<account> --yes", summary: "Remove an account from Switcher", run: runRemove},
		{name: "refresh", args: "[account]", summary: "Refresh usage for one or all accounts", run: runRefresh},
		{name: "share", args: "[on|off|code|devices|revoke <id> --yes]", summary: "Share this Mac's accounts with your other Macs", help: shareHelp, run: runShare},
		{name: "connect", args: "<address> --code CODE", summary: "Use the accounts of a Switcher that shares them", run: runConnect},
		{name: "disconnect", args: "--yes", summary: "Stop using another Switcher", run: runDisconnect},
		{name: "discover", summary: "Find Switchers that share on your network or tailnet", run: runDiscover},
		{name: "away", args: "[on|off|remove --yes]", summary: "Reach your Macs from anywhere with the Tailscale add-on", run: runAway},
		{name: "desktop", args: "[connect|disconnect|restart --yes]", summary: "Route Claude Desktop through Switcher", run: runDesktop},
		{name: "setup", args: "[codex [--undo|--test]]", summary: "Point CLI tools at Switcher", run: runSetup},
		{name: "settings", args: "[set key=value ...]", summary: "Show or change settings", run: runSettings},
		{name: "start", summary: "Start Switcher in the background if it is not running", run: runStart},
		{name: "open", summary: "Open the dashboard in a browser", run: runOpen},
		{name: "install-cli", args: "[--dir DIR]", summary: "Put the switcher command on your PATH", run: runInstallCLI},
	}
}

// Run executes one command line and returns the exit code.
func Run(args []string, opt Options) int {
	if opt.Stdout == nil {
		opt.Stdout = os.Stdout
	}
	if opt.Stderr == nil {
		opt.Stderr = os.Stderr
	}
	if opt.Stdin == nil {
		opt.Stdin = os.Stdin
	}
	if opt.Getenv == nil {
		opt.Getenv = os.Getenv
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			if cmd := find(args[1]); cmd != nil {
				printCommandHelp(opt.Stdout, cmd)
				return exitOK
			}
		}
		printHelp(opt.Stdout)
		return exitOK
	}
	cmd := find(args[0])
	if cmd == nil {
		fmt.Fprintf(opt.Stderr, "switcher: unknown command %q\nRun 'switcher help' for the list of commands.\n", args[0])
		return exitUsage
	}
	c := newCtx(opt)
	err := cmd.run(c, args[1:])
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		printCommandHelp(opt.Stdout, cmd)
		return exitOK
	}
	code := exitFail
	var usage usageError
	switch {
	case errors.As(err, &usage):
		code = exitUsage
	case errors.Is(err, errNotRunning):
		code = exitNotRunning
	}
	if c.json {
		kind := map[int]string{exitFail: "failed", exitUsage: "usage", exitNotRunning: "not_running"}[code]
		c.emit(map[string]any{"error": err.Error(), "code": kind})
	}
	fmt.Fprintf(opt.Stderr, "switcher %s: %v\n", cmd.name, err)
	if code == exitUsage {
		fmt.Fprintf(opt.Stderr, "Usage: switcher %s %s\n", cmd.name, cmd.args)
	}
	return code
}

func find(name string) *command {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd
		}
	}
	return nil
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Switcher: one harness, all your AI accounts.

Usage: switcher <command> [arguments] [--json]

Commands:
`)
	width := 0
	for _, cmd := range commands {
		width = max(width, len(cmd.name+" "+cmd.args))
	}
	for _, cmd := range commands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, strings.TrimSpace(cmd.name+" "+cmd.args), cmd.summary)
	}
	fmt.Fprint(w, `
Run without a command to start the server itself (the app does this for you);
'switcher version' prints the version.

Every command takes --json for machine-readable output and --url to reach a
Switcher on another port. Exit codes: 0 ok, 1 failed, 2 usage, 3 not running.
Accounts can be named by id, email, or provider:email.

Examples:
  switcher status
  switcher login claude                  # works over SSH: see 'switcher help login'
  switcher use work@example.com
  switcher share on && switcher share code
  switcher connect studio.local --code K7QF-M2XP
`)
}

func printCommandHelp(w io.Writer, cmd *command) {
	fmt.Fprintf(w, "Usage: switcher %s\n\n%s.\n", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.summary)
	if cmd.help != "" {
		fmt.Fprintf(w, "\n%s", cmd.help)
	}
	fmt.Fprint(w, "\nFlags: --json for machine-readable output, --url to reach another Switcher port.\n")
}

// ctx is one command's run: flags, output and the server client.
type ctx struct {
	opt  Options
	json bool
	url  string
	out  io.Writer
	cl   *client
}

func newCtx(opt Options) *ctx {
	url := strings.TrimRight(opt.Getenv("SWITCHER_URL"), "/")
	if url == "" {
		url = fmt.Sprintf("http://127.0.0.1:%d", opt.Port)
	}
	return &ctx{opt: opt, url: url, out: opt.Stdout}
}

// flags returns a flag set with the shared --json and --url flags.
func (c *ctx) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&c.json, "json", false, "")
	fs.StringVar(&c.url, "url", c.url, "")
	return fs
}

// parse reads flags anywhere among the arguments and checks the number of
// positional arguments.
func (c *ctx) parse(fs *flag.FlagSet, args []string, minArgs, maxArgs int) ([]string, error) {
	var pos, tail []string
	for i, a := range args {
		if a == "--" {
			tail, args = args[i+1:], args[:i]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	pos = append(pos, tail...)
	if len(pos) < minArgs {
		return nil, usagef("missing argument")
	}
	if maxArgs >= 0 && len(pos) > maxArgs {
		return nil, usagef("unexpected argument %q", pos[maxArgs])
	}
	c.cl = newClient(c.url, c.opt.DeviceToken)
	return pos, nil
}

// emit writes one JSON document (one line in streams).
func (c *ctx) emit(v any) {
	enc := json.NewEncoder(c.out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(c.opt.Stderr, "switcher: %v\n", err)
	}
}

func (c *ctx) printf(format string, args ...any) {
	if !c.json {
		fmt.Fprintf(c.out, format, args...)
	}
}

// result prints v as JSON, or the human text.
func (c *ctx) result(v any, human string) {
	if c.json {
		c.emit(v)
		return
	}
	if human != "" {
		fmt.Fprintln(c.out, strings.TrimRight(human, "\n"))
	}
}

var providerNames = map[string]string{"claude": "Claude", "codex": "Codex", "grok": "Grok", "opencode": "OpenCode",
	"antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"}

// How each provider adds an account.
var addMethod = map[string]string{"claude": "browser", "codex": "browser", "antigravity": "browser", "gemini": "browser",
	"grok": "device", "copilot": "device", "opencode": "key"}

var planNames = map[string]string{"claude_max_5x": "Max 5x", "claude_max_20x": "Max 20x", "claude_max": "Max", "claude_pro": "Pro",
	"max": "Max", "pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free", "copilot_pro": "Pro",
	"copilot_pro_plus": "Pro+", "copilot_business": "Business", "copilot_enterprise": "Enterprise", "copilot_free": "Free"}

func providerName(id string) string {
	if name := providerNames[id]; name != "" {
		return name
	}
	return id
}

func knownProvider(id string) error {
	if _, ok := providerNames[id]; ok {
		return nil
	}
	ids := make([]string, 0, len(providerNames))
	for id := range providerNames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return usagef("unknown provider %q; use one of %s", id, strings.Join(ids, ", "))
}

// table aligns rows into columns without trailing spaces.
func table(rows [][]string) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// wait polls until done reports true, the timeout passes or done fails.
func wait(timeout, every time.Duration, done func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("timed out")
		}
		time.Sleep(every)
	}
}
