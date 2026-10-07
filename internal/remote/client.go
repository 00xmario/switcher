package remote

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type clientFile struct {
	HostName    string    `json:"host_name"`
	Addresses   []string  `json:"addresses"`
	Port        int       `json:"port"`
	Fingerprint string    `json:"fingerprint"`
	DeviceID    string    `json:"device_id"`
	Token       string    `json:"token"`
	PairedAt    time.Time `json:"paired_at"`
}

// ClientStatus describes the host this Switcher uses, if any.
type ClientStatus struct {
	Connected bool      `json:"connected"`
	HostName  string    `json:"host_name,omitempty"`
	Address   string    `json:"address,omitempty"`
	Reachable bool      `json:"reachable"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
}

// Client forwards this Switcher's accounts and provider traffic to a host.
type Client struct {
	dir           string
	managementKey string
	mu            sync.Mutex
	cfg           *clientFile
	transport     *http.Transport
	lastGood      string
	reachable     bool
	lastErr       string
	checkedAt     time.Time
	hostState     map[string]any
	onChange      func(bool)
}

// NewClient loads a saved pairing from dir. managementKey authenticates this
// Switcher's own T3 Code hub requests before they are forwarded.
func NewClient(dir, managementKey string) *Client {
	c := &Client{dir: dir, managementKey: managementKey}
	var cfg clientFile
	if err := readJSON(c.path(), &cfg); err == nil && cfg.Token != "" && cfg.Fingerprint != "" && len(cfg.Addresses) > 0 {
		c.setLocked(&cfg)
	}
	return c
}

func (c *Client) path() string { return filepath.Join(c.dir, "client.json") }

// OnChange is called with true when a host is connected and false when it is
// disconnected, including once now for the saved state.
func (c *Client) OnChange(fn func(connected bool)) {
	c.mu.Lock()
	c.onChange = fn
	connected := c.cfg != nil
	c.mu.Unlock()
	fn(connected)
}

func (c *Client) setLocked(cfg *clientFile) {
	c.cfg = cfg
	c.transport = nil
	c.hostState = nil
	c.lastGood = ""
	if cfg != nil {
		c.transport = c.newTransport(cfg)
	}
}

func (c *Client) newTransport(cfg *clientFile) *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DialContext:         c.dial,
		TLSClientConfig:     pinned(cfg.Fingerprint),
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
}

// pinned accepts exactly the host certificate that was paired.
func pinned(fp string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || fingerprint(cs.PeerCertificates[0].Raw) != fp {
				return errors.New("this is not the paired Switcher host")
			}
			return nil
		}}
}

// dial tries the address that worked last, then the host's other addresses,
// so moving between home Wi-Fi and Tailscale needs no reconfiguration.
func (c *Client) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	c.mu.Lock()
	if c.cfg == nil {
		c.mu.Unlock()
		return nil, errors.New("no Switcher host connected")
	}
	port := c.cfg.Port
	addresses := append([]string(nil), c.cfg.Addresses...)
	if c.lastGood != "" {
		addresses = append([]string{c.lastGood}, addresses...)
	}
	c.mu.Unlock()
	var lastErr error
	seen := map[string]bool{}
	for _, address := range addresses {
		if seen[address] {
			continue
		}
		seen[address] = true
		d := net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(address, strconv.Itoa(port)))
		if err == nil {
			c.mu.Lock()
			c.lastGood = address
			c.mu.Unlock()
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("the Switcher host is unreachable: %w", lastErr)
}

// Connected reports whether this Switcher uses a host.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg != nil
}

// Status reports the connected host and whether it answered recently.
func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil {
		return ClientStatus{}
	}
	address := c.lastGood
	if address == "" {
		address = c.cfg.Addresses[0]
	}
	return ClientStatus{Connected: true, HostName: c.cfg.HostName, Address: address, Reachable: c.reachable, Error: c.lastErr, CheckedAt: c.checkedAt}
}

// Connect pairs with the host at address using the code it shows.
func (c *Client) Connect(ctx context.Context, address string, port int, code, deviceName string) (ClientStatus, error) {
	address = strings.TrimSpace(address)
	if host, p, err := net.SplitHostPort(address); err == nil {
		address = host
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	if address == "" {
		return ClientStatus{}, errors.New("enter the host's address")
	}
	if port == 0 {
		port = DefaultPort
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Read the host's certificate, then prove the code against exactly it.
	var fp string
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return ClientStatus{}, fmt.Errorf("could not reach a Switcher at %s: is sharing turned on there?", address)
	}
	if certs := conn.(*tls.Conn).ConnectionState().PeerCertificates; len(certs) > 0 {
		fp = fingerprint(certs[0].Raw)
	}
	conn.Close()
	if fp == "" {
		return ClientStatus{}, errors.New("the host did not present a certificate")
	}
	cfg := &clientFile{Addresses: []string{address}, Port: port, Fingerprint: fp}
	body, _ := json.Marshal(pairRequest{Name: deviceName, Proof: pairingProof(code, fp)})
	transport := &http.Transport{Proxy: nil, TLSClientConfig: pinned(fp)}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+net.JoinHostPort(address, strconv.Itoa(port))+"/remote/pair", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return ClientStatus{}, fmt.Errorf("pairing failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = "the host refused the pairing"
		}
		return ClientStatus{}, errors.New(e.Error)
	}
	var paired pairResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&paired); err != nil || paired.Token == "" {
		return ClientStatus{}, errors.New("the host sent an invalid pairing reply")
	}
	cfg.HostName, cfg.DeviceID, cfg.Token, cfg.PairedAt = paired.HostName, paired.DeviceID, paired.Token, time.Now().UTC()
	for _, a := range paired.Addresses {
		if a != address {
			cfg.Addresses = append(cfg.Addresses, a)
		}
	}
	if err := writeJSON(c.path(), cfg); err != nil {
		return ClientStatus{}, err
	}
	c.mu.Lock()
	c.setLocked(cfg)
	c.lastGood, c.reachable, c.lastErr, c.checkedAt = address, true, "", time.Now()
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(true)
	}
	return c.Status(), nil
}

// Disconnect forgets the host; this Switcher uses its own accounts again.
func (c *Client) Disconnect() error {
	if err := os.Remove(c.path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	c.mu.Lock()
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	c.setLocked(nil)
	c.reachable, c.lastErr = false, ""
	onChange := c.onChange
	c.mu.Unlock()
	if onChange != nil {
		onChange(false)
	}
	return nil
}

func (c *Client) note(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = time.Now()
	c.reachable = err == nil
	c.lastErr = ""
	if err != nil {
		c.lastErr = err.Error()
	}
}

// do sends one request to the host as this paired device.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	cfg, transport := c.cfg, c.transport
	c.mu.Unlock()
	if cfg == nil {
		return nil, errors.New("no Switcher host connected")
	}
	req.URL.Scheme, req.URL.Host, req.Host = "https", "switcher-host", "switcher-host"
	req.Header.Set(DeviceHeader, cfg.Token)
	resp, err := transport.RoundTrip(req)
	c.note(err)
	if err == nil && resp.Header.Get(rejectedHeader) != "" {
		c.note(errors.New("the host no longer accepts this Mac; pair again"))
	}
	return resp, err
}

// Inference sends a Claude request to the host. It implements the Desktop
// relay's remote hook.
func (c *Client) Inference(ctx context.Context, account string, r *http.Request, body []byte) (*http.Response, bool, error) {
	if !c.Connected() {
		return nil, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, "https://switcher-host/remote/anthropic"+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		return nil, true, err
	}
	req.URL.RawQuery = r.URL.RawQuery
	req.Header = r.Header.Clone()
	for _, key := range []string{"Proxy-Authorization", "Proxy-Connection", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		req.Header.Del(key)
	}
	req.Header.Set(AccountHeader, account)
	resp, err := c.do(req)
	return resp, true, err
}

// forwardedPaths go to the host while connected; everything else, including
// this Mac's settings, CLI setup and usage logs, stays local.
var forwardedPaths = []string{"/api/accounts/", "/api/usage/refresh", "/api/providers/", "/v0/management/",
	"/codex/", "/claude/", "/grok/", "/opencode/", "/antigravity/", "/gemini/", "/copilot/"}

func forwarded(path string) bool {
	for _, prefix := range forwardedPaths {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// LocalState supplies this Mac's own fields of /api/state: version, update,
// Desktop relay, hub address and display preferences.
type LocalState func(r *http.Request) map[string]any

// localStateKeys stay this Mac's own in the merged /api/state.
var localStateKeys = []string{"version", "update", "desktop_relay", "hub_url", "hub_management_key",
	"menu_usage_bars", "reset_notifications", "compact_accounts", "merge_accounts"}

// Middleware sends account and provider traffic to the host while connected.
func (c *Client) Middleware(local LocalState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Connected() {
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
			c.serveState(w, r, local)
		case forwarded(r.URL.Path):
			if strings.HasPrefix(r.URL.Path, "/v0/management/") && !c.localManagementKey(r) {
				writeError(w, http.StatusUnauthorized, "management key required")
				return
			}
			c.proxy(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (c *Client) localManagementKey(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Management-Key")
	}
	return c.managementKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(c.managementKey)) == 1
}

func (c *Client) proxy(w http.ResponseWriter, r *http.Request) {
	management := strings.HasPrefix(r.URL.Path, "/v0/management/")
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "https", "switcher-host"
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("X-Switcher-CSRF")
			if management {
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("X-Management-Key")
			}
		},
		Transport:     roundTripFunc(c.do),
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			if req.Context().Err() == nil {
				writeError(w, http.StatusBadGateway, "the Switcher host "+c.Status().HostName+" is unreachable")
			}
		},
	}
	rp.ServeHTTP(w, r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serveState shows the host's accounts with this Mac's own settings. While the
// host is unreachable the last host state is shown with a warning.
func (c *Client) serveState(w http.ResponseWriter, r *http.Request, local LocalState) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://switcher-host/api/state", nil)
	var state map[string]any
	resp, err := c.do(req)
	if err == nil {
		switch {
		case resp.StatusCode == http.StatusOK:
			err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&state)
		case resp.Header.Get(rejectedHeader) != "":
			err = errors.New("the host no longer accepts this Mac; pair again")
		default:
			err = fmt.Errorf("the host answered %d", resp.StatusCode)
		}
		resp.Body.Close()
		if err != nil {
			c.note(err)
		}
	}
	c.mu.Lock()
	if err == nil {
		c.hostState = state
	} else {
		state = c.hostState
	}
	c.mu.Unlock()
	if state == nil {
		state = map[string]any{"accounts": []any{}, "active": map[string]any{}}
	}
	merged := make(map[string]any, len(state)+4)
	for k, v := range state {
		merged[k] = v
	}
	mine := local(r)
	for _, k := range localStateKeys {
		if v, ok := mine[k]; ok {
			merged[k] = v
		} else {
			delete(merged, k)
		}
	}
	status := c.Status()
	merged["remote"] = map[string]any{"role": "client", "host": status.HostName, "address": status.Address, "connected": err == nil, "error": status.Error}
	writeJSONResponse(w, http.StatusOK, merged)
}

func isNotExist(err error) bool { return os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) }
