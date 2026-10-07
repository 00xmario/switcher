package remote

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// hostMux stands in for the host's server: it echoes what reached it.
func hostMux(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(DeviceHeader) != "" || r.Header.Get("Cookie") != "" {
			t.Error("device token or cookie reached the host's routes")
		}
		switch {
		case r.URL.Path == "/api/state":
			json.NewEncoder(w).Encode(map[string]any{"accounts": []any{map[string]any{"id": "host-account"}}, "version": "host-version", "hub_management_key": "host-key"})
		default:
			b, _ := io.ReadAll(r.Body)
			device, _ := PairedDevice(r)
			io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(b)+" auth="+r.Header.Get("Authorization")+" account="+r.Header.Get(AccountHeader)+" device="+device)
		}
	})
}

func pairedPair(t *testing.T) (*Host, *Client) {
	t.Helper()
	host, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	if err := host.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	code, err := host.NewPairingCode()
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(t.TempDir(), "client-key")
	if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, strings.ToLower(code.Code), "Laptop"); err != nil {
		t.Fatal(err)
	}
	return host, client
}

func TestPairedClientUsesTheHostForAccountsAndProviders(t *testing.T) {
	host, client := pairedPair(t)
	if s := host.Status(); len(s.Devices) != 1 || s.Devices[0].Name != "Laptop" || s.Pairing != nil {
		t.Fatalf("host status after pairing: %+v", s)
	}
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local "+r.URL.Path) })
	handler := client.Middleware(func(*http.Request) map[string]any {
		return map[string]any{"version": "client-version", "hub_management_key": "client-key"}
	}, local)
	get := func(method, path, body string, header http.Header) string {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range header {
			r.Header[k] = v
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Body.String()
	}
	var state map[string]any
	json.Unmarshal([]byte(get("GET", "/api/state", "", nil)), &state)
	if state["version"] != "client-version" || state["hub_management_key"] != "client-key" || len(state["accounts"].([]any)) != 1 || state["remote"].(map[string]any)["connected"] != true {
		t.Fatalf("merged state: %v", state)
	}
	if got := get("POST", "/codex/v1/responses?x=1", `{"model":"m"}`, http.Header{"Authorization": {"Bearer cli"}, "Cookie": {"s=1"}}); got != `POST /codex/v1/responses?x=1 {"model":"m"} auth=Bearer cli account= device=Laptop` {
		t.Fatalf("provider request: %q", got)
	}
	if got := get("GET", "/v0/management/auth-files", "", http.Header{"Authorization": {"Bearer client-key"}}); !strings.HasPrefix(got, "GET /v0/management/auth-files  auth= ") {
		t.Fatalf("management request: %q", got)
	}
	if got := get("GET", "/v0/management/auth-files", "", http.Header{"Authorization": {"Bearer wrong"}}); !strings.Contains(got, "management key required") {
		t.Fatalf("unauthenticated management request: %q", got)
	}
	if got := get("GET", "/api/settings", "", nil); got != "local /api/settings" {
		t.Fatalf("local route: %q", got)
	}
	r := httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(`{}`))
	resp, ok, err := client.Inference(context.Background(), "host-account", r, []byte(`{}`))
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "POST /remote/anthropic/v1/messages?beta=true {} auth= account=host-account device=Laptop" {
		t.Fatalf("inference: %q", b)
	}
}

func TestPairingRejectsWrongCodesAndRevokedDevices(t *testing.T) {
	host, err := NewHost(t.TempDir(), 0, hostMux(t))
	if err != nil {
		t.Fatal(err)
	}
	host.Quiet = true
	t.Cleanup(host.Close)
	host.SetEnabled(true)
	host.NewPairingCode()
	client := NewClient(t.TempDir(), "")
	for i := 0; i < codeAttempts; i++ {
		if _, err := client.Connect(context.Background(), "127.0.0.1", host.Status().Port, "AAAA-AAAA", "x"); err == nil {
			t.Fatal("wrong code paired")
		}
	}
	if host.Status().Pairing != nil {
		t.Fatal("code survived too many attempts")
	}
	_, client = pairedPair(t)
	_ = client
	host2, client2 := pairedPair(t)
	if err := host2.Revoke(host2.Status().Devices[0].ID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
	if _, _, err := client2.Inference(context.Background(), "", r, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "pair again") {
		t.Fatalf("revoked device: %v", err)
	}
	if s := client2.Status(); s.Reachable || !strings.Contains(s.Error, "pair again") {
		t.Fatalf("client status after revocation: %+v", s)
	}
}

// A device in the middle with its own certificate cannot use a code it relays.
func TestPairingProofIsBoundToTheHostCertificateAndRole(t *testing.T) {
	a, _ := pairingKey("ABCD-EFGH", "aa")
	b, _ := pairingKey("ABCD-EFGH", "bb")
	c, _ := pairingKey("abcd efgh", "aa")
	if pairingProof(a, "client", "n") == pairingProof(b, "client", "n") {
		t.Fatal("proof ignores the certificate")
	}
	if pairingProof(a, "client", "n") != pairingProof(c, "client", "n") {
		t.Fatal("code formatting changes the proof")
	}
	if pairingProof(a, "client", "n") == pairingProof(a, "host", "n") {
		t.Fatal("the host could replay the client's proof")
	}
}

// A device pretending to be the host, without the code, cannot complete a
// pairing: the client checks the host's proof before saving anything.
func TestClientRefusesAHostThatDoesNotKnowTheCode(t *testing.T) {
	var asked bool
	rogue := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
		json.NewEncoder(w).Encode(pairResponse{DeviceID: "d", Token: "stolen", HostName: "Studio Mac", HostProof: "made-up"})
	}))
	defer rogue.Close()
	host, port, _ := net.SplitHostPort(rogue.Listener.Addr().String())
	n, _ := strconv.Atoi(port)
	client := NewClient(t.TempDir(), "")
	_, err := client.Connect(context.Background(), host, n, "ABCD-EFGH", "Laptop")
	if !asked || err == nil || client.Connected() {
		t.Fatalf("paired with a host that does not know the code: asked=%v err=%v", asked, err)
	}
	if _, statErr := os.Stat(client.path()); !os.IsNotExist(statErr) {
		t.Fatal("pairing was saved")
	}
}

func TestDisconnectRestoresLocalRoutes(t *testing.T) {
	_, client := pairedPair(t)
	var changes []bool
	client.OnChange(func(connected bool) { changes = append(changes, connected) })
	if err := client.Disconnect(); err != nil {
		t.Fatal(err)
	}
	handler := client.Middleware(func(*http.Request) map[string]any { return nil }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "local") }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/codex/v1/responses", nil))
	if w.Body.String() != "local" || len(changes) != 2 || changes[0] != true || changes[1] != false {
		t.Fatalf("after disconnect: %q %v", w.Body.String(), changes)
	}
}

func TestHostServesOnlyThisMacTheLocalNetworkAndTailscale(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "192.168.178.20": true, "10.0.0.4": true, "172.16.5.1": true,
		"169.254.1.2": true, "fe80::1": true, "fd12:3456::1": true, "100.101.2.3": true, "fd7a:115c:a1e0::1": true,
		"::ffff:192.168.1.5": true,
		"8.8.8.8":            false, "84.105.1.2": false, "100.128.0.1": false, "2a02:a210::1": false, "::ffff:84.105.1.2": false,
	} {
		if got := privateSource(net.ParseIP(ip)); got != want {
			t.Errorf("%s: accepted=%v, want %v", ip, got, want)
		}
	}
}

func TestClientLearnsTheHostsNewAddresses(t *testing.T) {
	host, client := pairedPair(t)
	host.addresses = func() ([]string, []string) { return []string{"studio.local"}, []string{"100.101.2.3"} }
	client.refreshAddresses()
	saved := NewClient(client.dir, "")
	want := []string{"studio.local", "100.101.2.3", "127.0.0.1"}
	if got := saved.cfg.Addresses; strings.Join(got[:3], ",") != strings.Join(want, ",") {
		t.Fatalf("saved addresses %v, want first %v", got, want)
	}
	client.refreshAddresses() // rate limited: no second request within two minutes
}
