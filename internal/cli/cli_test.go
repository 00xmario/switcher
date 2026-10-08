package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSwitcher answers the API calls the command line makes.
type fakeSwitcher struct {
	t     *testing.T
	mu    sync.Mutex
	calls []string
	login string // GET /api/login/{state} status
}

const fakeState = `{"version":"0.7.0","order":["claude","codex"],"hub_management_key":"mk",
 "accounts":[
  {"id":"c1","provider":"claude","email":"work@example.com","plan":"claude_max_20x","active":true,"native_active":true,
   "usage":{"windows":[{"label":"Session","used_percent":34},{"label":"Weekly","used_percent":61}]},"health":{"condition":"usage_current"}},
  {"id":"c2","provider":"claude","email":"home@example.com","plan":"claude_max_5x","active":false,
   "usage":{"windows":[{"label":"Session","used_percent":88}]},"health":{"condition":"needs_relogin"}},
  {"id":"x1","provider":"codex","email":"work@example.com","plan":"prolite","active":true,"usage":{"windows":[]},
   "reset_credits":{"count":2,"next_id":"credit_a"}}]}`

func (f *fakeSwitcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	login := f.login
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method + " " + r.URL.Path {
	case "GET /api/state":
		io.WriteString(w, fakeState)
	case "GET /api/remote":
		io.WriteString(w, `{"host":{"enabled":true,"devices":[{"id":"d1","name":"Air"}],"lan":["studio.local"]},"client":{"connected":false},"tailnet":{"enabled":false}}`)
	case "GET /api/desktop-relay":
		if r.Header.Get("X-Switcher-Desktop-Control") != "mk" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"desktop relay control authority required"}`)
			return
		}
		io.WriteString(w, `{"status":{"listening":true,"in_flight":1},"setup":{"configured":true,"condition":"configured"}}`)
	case "POST /api/accounts/c2/activate":
		io.WriteString(w, `{"status":"ok","active":"c2","native":{"changed":true}}`)
	case "POST /api/accounts/x1/use-reset":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["credit_id"] != "credit_a" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"wrong credit"}`)
			return
		}
		io.WriteString(w, `{"status":"ok","outcome":"reset","account":{"reset_credits":{"count":1}}}`)
	case "GET /api/phone":
		io.WriteString(w, `{"enabled":true,"ready":true,"url":"https://switcher-studio.tail1.ts.net",
			"waiting":[{"id":"w1","name":"marios-iphone"}],"devices":[{"id":"p1","name":"old-phone","last_seen":"2026-10-01T10:00:00Z"}]}`)
	case "POST /api/phone/approve":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "482913" {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":"no phone is waiting with that code"}`)
			return
		}
		io.WriteString(w, `{"status":"ok","approved":{"name":"marios-iphone"}}`)
	case "DELETE /api/phone/devices/p1":
		io.WriteString(w, `{"status":"ok"}`)
	case "POST /api/cli-setup/codex/test":
		io.WriteString(w, `{"outcome":"quota_or_rate_limit"}`)
	case "POST /api/login":
		io.WriteString(w, `{"state":"st1","kind":"browser","url":"https://claude.ai/oauth/authorize?x=1"}`)
	case "GET /api/login/st1":
		switch login {
		case "done":
			io.WriteString(w, `{"status":"done","account":{"id":"c3","provider":"claude","email":"new@example.com"}}`)
		case "failed":
			io.WriteString(w, `{"status":"failed","error":"login did not complete"}`)
		case "gone":
			io.WriteString(w, `{"status":"finished"}`)
		default:
			io.WriteString(w, `{"status":"pending"}`)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"no such route"}`)
	}
}

func (f *fakeSwitcher) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func run(t *testing.T, url string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, Options{Port: 1, Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errOut,
		Getenv: func(k string) string {
			if k == "SWITCHER_URL" {
				return url
			}
			return ""
		}})
	return code, out.String(), errOut.String()
}

func fake(t *testing.T) (*fakeSwitcher, string) {
	f := &fakeSwitcher{t: t}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func TestStatusShowsAccountsSharingAndDesktop(t *testing.T) {
	_, url := fake(t)
	code, out, errOut := run(t, url, "", "status")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"Switcher 0.7.0",
		"Claude\n  ● work@example.com  Max 20x  Session 34%  Weekly 61%  · Claude Code",
		"    home@example.com  Max 5x   Session 88%              · needs relogin",
		"Codex\n  ● work@example.com  Pro 5x",
		"Sharing         on · 1 paired Mac",
		"Claude Desktop  connected · 1 request in flight",
		"Away from home  off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("trailing space: %q", line)
		}
	}
}

func TestJSONOutputIsOneDocument(t *testing.T) {
	_, url := fake(t)
	code, out, _ := run(t, url, "", "accounts", "--provider", "claude", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	var got struct{ Accounts []Account }
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(got.Accounts) != 2 || got.Accounts[0].ID != "c1" || !got.Accounts[0].ClaudeCode || got.Accounts[1].Health != "needs_relogin" {
		t.Fatalf("accounts: %+v", got.Accounts)
	}
}

func TestAccountsAreFoundByIDEmailOrProvider(t *testing.T) {
	accounts := []Account{{ID: "c1", Provider: "claude", Email: "work@example.com"}, {ID: "x1", Provider: "codex", Email: "work@example.com"},
		{ID: "c2", Provider: "claude", Email: "home@example.com"}}
	for ref, want := range map[string]string{"c2": "c2", "codex:work@example.com": "x1", "HOME@example.com": "c2", "home": "c2", "x": "x1"} {
		got, err := resolve(ref, accounts)
		if err != nil || got.ID != want {
			t.Errorf("%s: got %q, %v", ref, got.ID, err)
		}
	}
	if _, err := resolve("work@example.com", accounts); err == nil || !strings.Contains(err.Error(), "matches 2 accounts") {
		t.Errorf("ambiguous email: %v", err)
	}
	if _, err := resolve("nobody", accounts); err == nil {
		t.Error("unknown account matched")
	}
}

func TestUseActivatesTheAccount(t *testing.T) {
	f, url := fake(t)
	code, out, errOut := run(t, url, "", "use", "home")
	if code != 0 || !f.called("POST /api/accounts/c2/activate") {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if !strings.Contains(out, "Claude now uses home@example.com. Claude Code switched too.") {
		t.Fatal(out)
	}
}

func TestExitCodes(t *testing.T) {
	_, url := fake(t)
	if code, _, _ := run(t, url, "", "frobnicate"); code != exitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code, _, errOut := run(t, url, "", "remove", "home"); code != exitUsage || !strings.Contains(errOut, "--yes") {
		t.Errorf("remove without --yes: %d %s", code, errOut)
	}
	// Nothing listens here.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := "http://" + ln.Addr().String()
	ln.Close()
	code, out, _ := run(t, closed, "", "status", "--json")
	if code != exitNotRunning || !strings.Contains(out, `"code":"not_running"`) {
		t.Errorf("not running: %d %s", code, out)
	}
	if code, out, _ := run(t, url, "", "help"); code != 0 || !strings.Contains(out, "switcher login claude") {
		t.Errorf("help: %d", code)
	}
	if code, out, _ := run(t, url, "", "login", "--help"); code != 0 || !strings.Contains(out, "login finish") {
		t.Errorf("login help: %d %s", code, out)
	}
}

func TestLoginWithoutWaitingPrintsTheLink(t *testing.T) {
	_, url := fake(t)
	code, out, _ := run(t, url, "", "login", "claude", "--no-wait", "--json")
	var got map[string]string
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("%d %s", code, out)
	}
	if got["state"] != "st1" || got["url"] != "https://claude.ai/oauth/authorize?x=1" || got["kind"] != "browser" {
		t.Fatalf("%v", got)
	}
	if code, _, errOut := run(t, url, "", "login", "opencode"); code != exitUsage || !strings.Contains(errOut, "add-key opencode") {
		t.Errorf("key provider: %d %s", code, errOut)
	}
}

func TestLoginWaitsForTheAccount(t *testing.T) {
	f, url := fake(t)
	f.login = "done"
	code, out, errOut := run(t, url, "", "login", "claude", "--json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"event":"started"`) || !strings.Contains(lines[1], `"email":"new@example.com"`) {
		t.Fatalf("events: %s", out)
	}
	f.login = "failed"
	if code, _, errOut := run(t, url, "", "login", "claude"); code != exitFail || !strings.Contains(errOut, "login did not complete") {
		t.Fatalf("failed login: %d %s", code, errOut)
	}
}

// A sign-in finished in a browser on another device is pasted back and
// delivered to the local callback, with the state from the fragment.
func TestLoginFinishDeliversThePastedAddress(t *testing.T) {
	var got string
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		if r.Header.Get("Accept") != "application/json" {
			t.Error("callback not asked for JSON")
		}
		io.WriteString(w, `{"ok":true,"state":"st1"}`)
	}))
	defer callback.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(callback.URL, "http://"))
	n, _ := strconv.Atoi(port)
	f, url := fake(t)
	f.login = "done"
	c := newCtx(Options{CallbackPorts: []int{n}, Getenv: func(string) string { return url }, Stdout: io.Discard, Stderr: io.Discard})
	c.cl = newClient(url, nil)
	state, err := c.finish("  'http://localhost:"+port+"/callback?code=abc#state=st1'  ", "")
	if err != nil || state != "st1" {
		t.Fatal(state, err)
	}
	if got != "/callback?code=abc&state=st1" {
		t.Fatalf("callback got %q", got)
	}
	for _, bad := range []string{"https://claude.ai/callback?code=abc", "http://localhost:9/callback?code=abc", "http://localhost:" + port + "/callback", "not a url"} {
		if _, err := c.finish(bad, ""); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := c.finish("http://localhost:"+port+"/callback?error=access_denied", ""); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("provider error: %v", err)
	}
}

func TestAddKeyReadsTheKeyFromStdin(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"status":"ok","account":{"id":"o1","provider":"opencode","email":"key"}}`)
	}))
	defer srv.Close()
	code, out, _ := run(t, srv.URL, "sk-secret\n", "add-key", "opencode")
	if code != 0 || body["key"] != "sk-secret" || body["provider"] != "opencode" || !strings.Contains(out, "Added OpenCode account") {
		t.Fatalf("%d %v %s", code, body, out)
	}
}

func TestFlagsMayFollowArguments(t *testing.T) {
	f, url := fake(t)
	code, out, _ := run(t, url, "", "use", "home", "--json")
	if code != 0 || !f.called("POST /api/accounts/c2/activate") || !strings.HasPrefix(out, "{") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestDisruptiveCommandsNeedYes(t *testing.T) {
	f, url := fake(t)
	for _, args := range [][]string{{"disconnect"}, {"share", "revoke", "Air"}, {"away", "remove"}} {
		code, _, errOut := run(t, url, "", args...)
		if code != exitUsage || !strings.Contains(errOut, "--yes") {
			t.Errorf("%v: %d %s", args, code, errOut)
		}
	}
	if f.called("POST /api/remote/disconnect") || f.called("DELETE /api/remote/host/devices/d1") || f.called("POST /api/remote/tailnet/remove") {
		t.Fatal("a disruptive command ran without --yes")
	}
}

func TestLoginThatIsNoLongerPendingFails(t *testing.T) {
	f, url := fake(t)
	f.login = "gone"
	if code, out, _ := run(t, url, "", "login", "claude"); code != exitFail || strings.Contains(out, "Signed in") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCodexRouteTestReportsItsOutcome(t *testing.T) {
	_, url := fake(t)
	if code, _, errOut := run(t, url, "", "setup", "codex", "--test"); code != exitFail || !strings.Contains(errOut, "quota or rate limit") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestDeviceTokenStaysOnThisMac(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	token := func() string { return "secret" }
	newClient(srv.URL, token).call(http.MethodGet, "/api/state", nil, nil)
	if got != "Bearer secret" {
		t.Fatalf("loopback: %q", got)
	}
	got = ""
	other := strings.Replace(srv.URL, "127.0.0.1", "localhost.example", 1)
	cl := newClient(other, token)
	cl.http.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(srv.URL, "http://"))
	}}
	cl.call(http.MethodGet, "/api/state", nil, nil)
	if got != "" {
		t.Fatalf("token sent to another host: %q", got)
	}
}

func TestInstallCLIKeepsAnotherSwitcherCommand(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "SwitcherServer")
	os.WriteFile(exe, []byte("x"), 0o755)
	other := filepath.Join(dir, "kubeswitch")
	os.WriteFile(other, []byte("x"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.Symlink(other, filepath.Join(bin, "switcher"))
	var out, errOut bytes.Buffer
	code := Run([]string{"install-cli", "--dir", bin}, Options{Executable: exe, Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }})
	if code != exitFail || !strings.Contains(errOut.String(), "is not Switcher") {
		t.Fatalf("%d %s", code, errOut.String())
	}
	if target, _ := os.Readlink(filepath.Join(bin, "switcher")); target != other {
		t.Fatalf("replaced another tool's link: %s", target)
	}
	// Its own link is updated.
	os.Remove(filepath.Join(bin, "switcher"))
	os.Symlink("/Applications/Old.app/Contents/MacOS/SwitcherServer", filepath.Join(bin, "switcher"))
	if code := Run([]string{"install-cli", "--dir", bin}, Options{Executable: exe, Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }}); code != 0 {
		t.Fatalf("own link: %d %s", code, errOut.String())
	}
}

func TestResetSpendsTheNextBankedResetOnlyWithYes(t *testing.T) {
	f, url := fake(t)
	code, _, errOut := run(t, url, "", "reset", "codex")
	if code != exitUsage || !strings.Contains(errOut, "2 banked resets of work@example.com; add --yes") {
		t.Fatalf("without --yes: exit %d %s", code, errOut)
	}
	if f.called("POST /api/accounts/x1/use-reset") {
		t.Fatal("spent a reset without --yes")
	}
	code, out, errOut := run(t, url, "", "reset", "codex", "--yes")
	if code != 0 || !strings.Contains(out, "Used a banked reset on work@example.com. 1 banked reset left.") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := run(t, url, "", "reset", "c2", "--yes"); code != exitFail || !strings.Contains(errOut, "has no banked reset") {
		t.Fatalf("account without resets: exit %d %s", code, errOut)
	}
}

func TestPhoneApprovesByCodeAndRevokesWithYes(t *testing.T) {
	f, url := fake(t)
	code, out, errOut := run(t, url, "", "phone")
	if code != 0 || !strings.Contains(out, "Phone access: on at https://switcher-studio.tail1.ts.net") || !strings.Contains(out, "marios-iphone (w1) waiting for its code") {
		t.Fatalf("status: exit %d out %q err %q", code, out, errOut)
	}
	if code, _, errOut := run(t, url, "", "phone", "approve", "000000"); code != exitFail || !strings.Contains(errOut, "no phone is waiting with that code") {
		t.Fatalf("wrong code: exit %d %s", code, errOut)
	}
	if code, out, _ := run(t, url, "", "phone", "approve", "482913"); code != 0 || !strings.Contains(out, "Approved marios-iphone.") {
		t.Fatalf("approve: exit %d %s", code, out)
	}
	if code, _, _ := run(t, url, "", "phone", "revoke", "old-phone"); code != exitUsage || f.called("DELETE /api/phone/devices/p1") {
		t.Fatalf("revoke without --yes: exit %d", code)
	}
	if code, _, errOut := run(t, url, "", "phone", "revoke", "old-phone", "--yes"); code != 0 || !f.called("DELETE /api/phone/devices/p1") {
		t.Fatalf("revoke: exit %d %s", code, errOut)
	}
}
