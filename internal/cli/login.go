package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const loginHelp = `A browser login works from anywhere, also over SSH:

  1. switcher login claude prints a sign-in link. Open it in a browser on any
     device and sign in.
  2. On the Mac that runs Switcher, the browser returns to Switcher and the
     account is added. On another device, the browser ends on a page that
     cannot load (its address starts with http://localhost). Copy that whole
     address and paste it into the waiting command, or run:
       switcher login finish '<address>'

Grok and Copilot show a code to enter on their website instead; OpenCode uses
an API key (switcher add-key opencode). If a provider's own CLI is already
signed in on this Mac, 'switcher login import <provider>' adopts that login.

Agents: 'switcher login claude --no-wait --json' prints the link and returns.
After the user pastes the address, 'switcher login finish <address> --json'
adds the account and prints it. --relogin <account> signs an existing account
in again. Without --no-wait and with --json, login prints one JSON line per
event: started, then done or failed.
`

type loginHandle struct {
	State           string `json:"state"`
	URL             string `json:"url,omitempty"`
	Kind            string `json:"kind"`
	VerificationURL string `json:"verification_url,omitempty"`
	UserCode        string `json:"user_code,omitempty"`
}

func runLogin(c *ctx, args []string) error {
	if len(args) > 0 && (args[0] == "finish" || args[0] == "import") {
		if args[0] == "finish" {
			return runLoginFinish(c, args[1:])
		}
		return runLoginImport(c, args[1:])
	}
	fs := c.flags("login")
	relogin := fs.String("relogin", "", "")
	noWait := fs.Bool("no-wait", false, "")
	open := fs.Bool("open", false, "")
	// Switcher forgets a pending sign-in after ten minutes.
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	provider := strings.ToLower(pos[0])
	if err := knownProvider(provider); err != nil {
		return err
	}
	if addMethod[provider] == "key" {
		return usagef("%s uses an API key; run: switcher add-key %s", providerName(provider), provider)
	}
	body := map[string]string{"provider": provider}
	if *relogin != "" {
		_, accounts, err := c.state()
		if err != nil {
			return err
		}
		account, err := resolve(*relogin, accounts)
		if err != nil {
			return err
		}
		body["relogin_of"] = account.ID
	}
	var handle loginHandle
	if err := c.cl.call(http.MethodPost, "/api/login", body, &handle); err != nil {
		return err
	}
	started := map[string]any{"event": "started", "provider": provider, "state": handle.State, "kind": handle.Kind}
	if handle.Kind == "device" || handle.UserCode != "" {
		started["verification_url"], started["user_code"] = handle.VerificationURL, handle.UserCode
		c.printf("Sign in to %s: open %s and enter the code %s\n", providerName(provider), handle.VerificationURL, handle.UserCode)
	} else {
		started["url"] = handle.URL
		started["finish"] = "switcher login finish '<address the browser ends on>'"
		c.printf("Sign in to %s by opening this link in a browser on any device:\n\n  %s\n\n", providerName(provider), handle.URL)
		c.printf("Signing in on another device? The browser ends on a page that cannot load.\nCopy its address (http://localhost…) and paste it here.\n\n")
		if *open || (c.opt.Interactive && c.opt.Getenv("SSH_CONNECTION") == "") {
			openURL(handle.URL)
		}
	}
	if c.json {
		if *noWait {
			delete(started, "event")
		}
		c.emit(started)
	}
	if *noWait {
		return nil
	}
	if c.opt.Interactive && handle.Kind != "device" {
		go c.readPastes(handle.State)
	}
	c.printf("Waiting for the sign-in…\n")
	var account *Account
	err = wait(*timeout, 300*time.Millisecond, func() (bool, error) {
		done, a, err := c.loginOutcome(handle.State)
		account = a
		return done, err
	})
	if err != nil {
		return err
	}
	if account == nil {
		return errors.New("the sign-in is no longer pending: it expired or finished elsewhere; check with: switcher accounts")
	}
	c.loginDone(account)
	return nil
}

// readPastes finishes the login with an address pasted into the terminal.
func (c *ctx) readPastes(state string) {
	lines := bufio.NewScanner(c.opt.Stdin)
	for lines.Scan() {
		text := strings.TrimSpace(lines.Text())
		if text == "" {
			continue
		}
		if _, err := c.finish(text, state); err != nil {
			fmt.Fprintf(c.opt.Stderr, "%v\nPaste the full address the browser ended on.\n", err)
			continue
		}
		return
	}
}

// loginOutcome reports whether the login finished, with its account.
func (c *ctx) loginOutcome(state string) (bool, *Account, error) {
	var answer struct {
		Status  string `json:"status"`
		Error   string `json:"error"`
		Account *struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Email    string `json:"email"`
			Plan     string `json:"plan"`
		} `json:"account"`
	}
	if err := c.cl.call(http.MethodGet, "/api/login/"+url.PathEscape(state), nil, &answer); err != nil {
		return false, nil, err
	}
	switch answer.Status {
	case "pending":
		return false, nil, nil
	case "failed":
		if answer.Error == "" {
			answer.Error = "the sign-in did not complete"
		}
		return false, nil, errors.New(answer.Error)
	case "done":
		if a := answer.Account; a != nil {
			return true, &Account{ID: a.ID, Provider: a.Provider, Email: a.Email, Plan: a.Plan, Usage: []Window{}}, nil
		}
	}
	// "finished": Switcher no longer knows this sign-in; another waiter
	// received it, or it expired.
	return true, nil, nil
}

func (c *ctx) loginDone(account *Account) {
	if account == nil {
		c.result(map[string]any{"event": "done"}, "Signed in.")
		return
	}
	c.result(map[string]any{"event": "done", "account": account},
		fmt.Sprintf("Added %s account %s (%s).", providerName(account.Provider), account.Email, account.ID))
}

// finish delivers a pasted sign-in address to Switcher's local callback.
func (c *ctx) finish(address, state string) (string, error) {
	u, err := url.Parse(strings.Trim(strings.TrimSpace(address), `'"<>`))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", usagef("that is not the address the browser ended on")
	}
	host, port := u.Hostname(), u.Port()
	n, _ := strconv.Atoi(port)
	if (host != "localhost" && host != "127.0.0.1" && host != "::1") || !slices.Contains(c.opt.CallbackPorts, n) {
		return "", usagef("expected an address starting with http://localhost and a sign-in port, such as http://localhost:54545/callback?code=…")
	}
	query := u.Query()
	if query.Get("code") == "" {
		if errText := query.Get("error_description") + " " + query.Get("error"); strings.TrimSpace(errText) != "" {
			return "", fmt.Errorf("the provider refused the sign-in: %s", strings.TrimSpace(errText))
		}
		return "", usagef("the address has no sign-in code; copy the complete address")
	}
	// Some providers put the state in the fragment, which browsers keep.
	if query.Get("state") == "" {
		if fragment, err := url.ParseQuery(u.Fragment); err == nil && fragment.Get("state") != "" {
			query.Set("state", fragment.Get("state"))
		} else if state != "" {
			query.Set("state", state)
		}
	}
	target := fmt.Sprintf("http://%s%s?%s", net.JoinHostPort("127.0.0.1", port), u.EscapedPath(), query.Encode())
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.cl.http.Do(req)
	if err != nil {
		if refused(err) {
			return "", errNotRunning
		}
		return "", err
	}
	defer resp.Body.Close()
	var answer struct {
		OK    bool   `json:"ok"`
		State string `json:"state"`
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if json.Unmarshal(raw, &answer) != nil {
		return "", errors.New("unexpected answer from Switcher's sign-in callback")
	}
	if !answer.OK {
		return "", fmt.Errorf("sign-in failed: %s", answer.Error)
	}
	return answer.State, nil
}

func runLoginFinish(c *ctx, args []string) error {
	fs := c.flags("login finish")
	state := fs.String("state", "", "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	resolved, err := c.finish(pos[0], *state)
	if err != nil {
		return err
	}
	var account *Account
	if resolved != "" {
		_, account, _ = c.loginOutcome(resolved)
	}
	c.loginDone(account)
	return nil
}

func runLoginImport(c *ctx, args []string) error {
	fs := c.flags("login import")
	relogin := fs.String("relogin", "", "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	provider := strings.ToLower(pos[0])
	if err := knownProvider(provider); err != nil {
		return err
	}
	body := map[string]string{"provider": provider}
	if *relogin != "" {
		_, accounts, err := c.state()
		if err != nil {
			return err
		}
		account, err := resolve(*relogin, accounts)
		if err != nil {
			return err
		}
		body["relogin_of"] = account.ID
	}
	var answer struct {
		Account *Account `json:"account"`
	}
	if err := c.cl.call(http.MethodPost, "/api/login/import", body, &answer); err != nil {
		return err
	}
	if answer.Account != nil && answer.Account.Usage == nil {
		answer.Account.Usage = []Window{}
	}
	c.loginDone(answer.Account)
	return nil
}

func runAddKey(c *ctx, args []string) error {
	fs := c.flags("add-key")
	key := fs.String("key", "", "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	provider := strings.ToLower(pos[0])
	if err := knownProvider(provider); err != nil {
		return err
	}
	if *key == "" && c.opt.StdinTerminal {
		return usagef("pass the key with --key, or pipe it in: pbpaste | switcher add-key %s", provider)
	}
	if *key == "" {
		// Read from stdin so the key stays out of shell history.
		b, err := io.ReadAll(io.LimitReader(c.opt.Stdin, 64<<10))
		if err != nil {
			return err
		}
		*key = strings.TrimSpace(string(b))
	}
	if *key == "" {
		return usagef("no key: pass --key or pipe it on stdin")
	}
	var answer struct {
		Account *Account `json:"account"`
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts", map[string]string{"provider": provider, "key": *key}, &answer); err != nil {
		return err
	}
	if answer.Account != nil && answer.Account.Usage == nil {
		answer.Account.Usage = []Window{}
	}
	c.loginDone(answer.Account)
	return nil
}

func openURL(target string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, target).Start()
}
