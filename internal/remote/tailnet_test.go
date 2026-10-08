package remote

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain doubles as a fake switcher-tailnet add-on: it reports a running
// tailnet node and connects every CONNECT target to 127.0.0.1 on the same port.
func TestMain(m *testing.M) {
	if os.Getenv("SWITCHER_TAILNET_FAKE") == "1" {
		fakeAddon()
		return
	}
	os.Exit(m.Run())
}

func fakeAddon() {
	token := os.Getenv("SWITCHER_TAILNET_TOKEN")
	var phoneOn atomic.Bool
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Switcher-Tailnet") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/phone":
			phoneOn.Store(r.URL.Query().Get("on") == "1")
			w.WriteHeader(http.StatusNoContent)
			return
		case "/phone-env":
			// What the add-on was started with, for the phone tests.
			json.NewEncoder(w).Encode(map[string]any{"forward": os.Getenv("SWITCHER_TAILNET_PHONE"),
				"key": os.Getenv("SWITCHER_TAILNET_PHONE_KEY"), "args": os.Args[1:], "on": phoneOn.Load()})
			return
		}
		if r.Method == http.MethodConnect {
			_, port, _ := net.SplitHostPort(r.Host)
			remote, err := net.Dial("tcp", "127.0.0.1:"+port)
			if err != nil {
				http.Error(w, "unreachable", http.StatusBadGateway)
				return
			}
			conn, rw, _ := w.(http.Hijacker).Hijack()
			rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			rw.Flush()
			go func() { io.Copy(remote, rw.Reader); remote.Close() }()
			go func() { io.Copy(conn, remote); conn.Close() }()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": "Running", "name": "switcher-studio", "dns_name": "switcher-studio.tail1234.ts.net", "ip": "100.101.2.3",
			"peers": []map[string]any{{"name": "switcher-air", "dns_name": "switcher-air.tail1234.ts.net", "ip": "100.101.2.4", "online": true}}})
	}))
	json.NewEncoder(os.Stdout).Encode(map[string]string{"control": ln.Addr().String()})
	io.Copy(io.Discard, bufio.NewReader(os.Stdin))
}

func fakeTailnet(t *testing.T, dir string, hostPort int) *Tailnet {
	t.Helper()
	t.Setenv("SWITCHER_TAILNET_FAKE", "1")
	tn := NewTailnet(dir, "1.2.3", hostPort)
	downloads := 0
	tn.download = func(ctx context.Context, dest string) error {
		downloads++
		self, err := os.Executable()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(self)
		if err != nil {
			return err
		}
		os.MkdirAll(dest[:strings.LastIndex(dest, "/")], 0o700)
		sum := sha256.Sum256(b)
		os.WriteFile(dest+".sha256", []byte(hex.EncodeToString(sum[:])), 0o600)
		return os.WriteFile(dest, b, 0o700)
	}
	t.Cleanup(tn.Close)
	return tn
}

func TestTailnetAddonIsDownloadedOnlyWhenTurnedOn(t *testing.T) {
	dir := t.TempDir()
	tn := fakeTailnet(t, dir, 1)
	if s := tn.Status(); s.Installed || s.Enabled || s.Running {
		t.Fatalf("fresh status: %+v", s)
	}
	if err := tn.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := tn.Status()
	if !s.Installed || !s.Running || s.State != "Running" || s.DNSName != "switcher-studio.tail1234.ts.net" || len(s.Peers) != 1 {
		t.Fatalf("running status: %+v", s)
	}
	if got := tn.Addresses(); strings.Join(got, ",") != "switcher-studio.tail1234.ts.net,100.101.2.3" {
		t.Fatalf("addresses: %v", got)
	}
	// The choice is remembered; Remove deletes the add-on again.
	again := NewTailnet(dir, "1.2.3", 1)
	if !again.Status().Enabled {
		t.Fatal("enabled choice was not saved")
	}
	if err := tn.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := tn.Status(); s.Installed || s.Enabled || s.Running {
		t.Fatalf("after remove: %+v", s)
	}
}

// An add-on that stops while turned on is started again.
func TestTailnetAddonRestartsAfterItStops(t *testing.T) {
	tn := fakeTailnet(t, t.TempDir(), 1)
	tn.retryDelay = 10 * time.Millisecond
	if err := tn.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	tn.mu.Lock()
	first := tn.cmd
	tn.mu.Unlock()
	first.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tn.mu.Lock()
		restarted := tn.cmd != nil && tn.cmd != first
		tn.mu.Unlock()
		if restarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the add-on was not restarted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Turned off, it stays off.
	tn.Disable()
	time.Sleep(100 * time.Millisecond)
	if s := tn.Status(); s.Running {
		t.Fatalf("after disable: %+v", s)
	}
}

// A first start that fails, here an offline download, is tried again.
func TestTailnetAddonRetriesAFailedStart(t *testing.T) {
	tn := fakeTailnet(t, t.TempDir(), 1)
	tn.retryDelay = 10 * time.Millisecond
	download := tn.download
	failures := 1
	tn.download = func(ctx context.Context, dest string) error {
		if failures > 0 {
			failures--
			return errors.New("offline")
		}
		return download(ctx, dest)
	}
	if err := tn.Enable(context.Background()); err == nil {
		t.Fatal("the first start should fail")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !tn.Status().Running {
		if time.Now().After(deadline) {
			t.Fatalf("the add-on was not started again: %+v", tn.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client pairs and works over a Tailscale name through the add-on.
func TestClientPairsAndForwardsOverTheTailnet(t *testing.T) {
	host, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	host.SetEnabled(true)
	code, _ := host.NewPairingCode()
	port := host.Status().Port
	tn := fakeTailnet(t, t.TempDir(), port)
	if err := tn.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	client := NewClient(t.TempDir(), "")
	client.UseTailnet(tn.Dial)
	if _, err := client.Connect(context.Background(), "switcher-studio.tail1234.ts.net", port, code.Code, "Air"); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, ok, err := client.Inference(ctx, "a", r, []byte(`{}`))
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "/remote/anthropic/v1/messages") || !strings.Contains(string(b), "device=Air") {
		t.Fatalf("over the tailnet: %q", b)
	}
	if s := client.Status(); s.Address != "switcher-studio.tail1234.ts.net" {
		t.Fatalf("client address: %+v", s)
	}
}

func TestTailnetNamesAndAddresses(t *testing.T) {
	if got := TailnetHostname("Mario's MacBook Pro"); got != "switcher-marios-macbook-pro" {
		t.Fatal(got)
	}
	for host, want := range map[string]bool{"100.101.2.3": true, "studio.tail1234.ts.net": true, "studio.local": false, "192.168.1.2": false} {
		if onTailnet(host) != want {
			t.Errorf("%s: onTailnet=%v", host, !want)
		}
	}
}

// phoneEnv asks the fake add-on what it was started with.
func phoneEnv(t *testing.T, tn *Tailnet) (forward, key string, args []string, on bool) {
	t.Helper()
	tn.mu.Lock()
	control, token := tn.control, tn.token
	tn.mu.Unlock()
	req, _ := http.NewRequest(http.MethodGet, "http://"+control+"/phone-env", nil)
	req.Header.Set("X-Switcher-Tailnet", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Forward, Key string
		Args         []string
		On           bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got.Forward, got.Key, got.Args, got.On
}

func TestPhoneKeyFollowsTheRunningAddon(t *testing.T) {
	// Phone settings in Switcher's own environment never reach the add-on.
	t.Setenv("SWITCHER_TAILNET_PHONE", "127.0.0.1:9")
	t.Setenv("SWITCHER_TAILNET_PHONE_KEY", strings.Repeat("e", 48))
	tn := fakeTailnet(t, t.TempDir(), 1)
	tn.retryDelay = 10 * time.Millisecond
	on := atomic.Bool{}
	tn.ServePhone("127.0.0.1:4321", on.Load)
	if tn.PhoneKey() != "" {
		t.Fatal("a phone key before the add-on runs")
	}
	if err := tn.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	forward, key, args, _ := phoneEnv(t, tn)
	if forward != "127.0.0.1:4321" || len(key) != 48 || key != tn.PhoneKey() || key == tn.token {
		t.Fatalf("add-on started with forward %q key %q; Switcher expects %q", forward, key, tn.PhoneKey())
	}
	for _, arg := range args {
		if strings.Contains(arg, "phone") {
			t.Fatalf("phone settings passed as a flag, which older add-ons reject: %v", args)
		}
	}
	// A sync already under way takes the change along, so it arrives soon
	// rather than before SyncPhone returns.
	on.Store(true)
	tn.SyncPhone()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, _, _, got := phoneEnv(t, tn); got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("phone access was not passed on")
		}
	}

	// A restarted add-on gets a new key, and Switcher follows it.
	tn.mu.Lock()
	first := tn.cmd
	tn.mu.Unlock()
	first.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tn.mu.Lock()
		restarted := tn.cmd != nil && tn.cmd != first
		tn.mu.Unlock()
		if restarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the add-on was not restarted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, again, _, _ := phoneEnv(t, tn)
	if again == key || again != tn.PhoneKey() {
		t.Fatalf("after a restart the add-on has %q and Switcher expects %q", again, tn.PhoneKey())
	}
	tn.Disable()
	if tn.PhoneKey() != "" {
		t.Fatal("a phone key after the add-on stopped")
	}
}

func TestNoPhoneKeyWithoutThePhoneListener(t *testing.T) {
	t.Setenv("SWITCHER_TAILNET_PHONE", "127.0.0.1:9")
	t.Setenv("SWITCHER_TAILNET_PHONE_KEY", strings.Repeat("e", 48))
	tn := fakeTailnet(t, t.TempDir(), 1)
	if err := tn.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if forward, key, _, _ := phoneEnv(t, tn); forward != "" || key != "" || tn.PhoneKey() != "" {
		t.Fatalf("phone settings without a phone listener: %q %q %q", forward, key, tn.PhoneKey())
	}
}
