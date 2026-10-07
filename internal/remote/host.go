package remote

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DeviceHeader carries a paired device's token. Provider traffic keeps its own
// Authorization header, so the device proves itself separately.
const DeviceHeader = "X-Switcher-Device"

// AccountHeader names the host account for Claude inference; empty means the
// host's own Claude selection.
const AccountHeader = "X-Switcher-Account"

// rejectedHeader marks the host's own refusal of a device, as opposed to a
// provider's 401 passed through.
const rejectedHeader = "X-Switcher-Device-Rejected"

const (
	codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	codeLength   = 8
	codeTTL      = 10 * time.Minute
	codeAttempts = 5
)

type device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_sha256"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type hostFile struct {
	Enabled bool     `json:"enabled"`
	Devices []device `json:"devices"`
}

// DeviceView is a paired device as the UI shows it.
type DeviceView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
}

// PairingView is the code another device enters to pair.
type PairingView struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HostStatus describes this Switcher as a host.
type HostStatus struct {
	Enabled   bool         `json:"enabled"`
	Listening bool         `json:"listening"`
	Name      string       `json:"name"`
	Port      int          `json:"port"`
	LAN       []string     `json:"lan"`
	Tailscale []string     `json:"tailscale"`
	Devices   []DeviceView `json:"devices"`
	Pairing   *PairingView `json:"pairing,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// Host serves this Switcher's accounts to paired devices.
type Host struct {
	dir, name, fp string
	port          int
	cert          tls.Certificate
	next          http.Handler
	mu            sync.Mutex
	state         hostFile
	code          string
	codeExpires   time.Time
	attempts      int
	srv           *http.Server
	listening     bool
	listenErr     string
	advertising   func()
	// Quiet skips the Bonjour advertisement (tests).
	Quiet bool
}

// NewHost loads the host state from dir. next serves the routes paired devices
// may use; it must apply no further authentication.
func NewHost(dir string, port int, next http.Handler) (*Host, error) {
	cert, fp, err := hostCertificate(dir)
	if err != nil {
		return nil, err
	}
	h := &Host{dir: dir, port: port, cert: cert, fp: fp, next: next, name: MachineName()}
	if err := readJSON(h.path(), &h.state); err != nil && !errors.Is(err, io.EOF) && !isNotExist(err) {
		return nil, err
	}
	return h, nil
}

func (h *Host) path() string { return filepath.Join(h.dir, "host.json") }

// Fingerprint identifies the host certificate.
func (h *Host) Fingerprint() string { return h.fp }

// Start opens the listener when hosting is enabled.
func (h *Host) Start() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state.Enabled {
		h.listenLocked()
	}
}

func (h *Host) listenLocked() {
	if h.srv != nil {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", h.port))
	if err != nil {
		h.listenErr = fmt.Sprintf("port %d is in use", h.port)
		return
	}
	h.listenErr = ""
	h.port = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second, ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{h.cert}, MinVersion: tls.VersionTLS12}}
	h.srv, h.listening = srv, true
	go func() {
		srv.ServeTLS(ln, "", "")
		h.mu.Lock()
		if h.srv == srv {
			h.srv, h.listening = nil, false
		}
		h.mu.Unlock()
	}()
	if !h.Quiet {
		h.advertising = advertise(h.name, h.port, h.fp)
	}
}

func (h *Host) stopLocked() {
	if h.advertising != nil {
		h.advertising()
		h.advertising = nil
	}
	if h.srv != nil {
		h.srv.Close()
		h.srv, h.listening = nil, false
	}
}

// Close stops serving without changing the saved preference.
func (h *Host) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopLocked()
}

// SetEnabled turns hosting on or off and remembers the choice.
func (h *Host) SetEnabled(enabled bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.state.Enabled
	h.state.Enabled = enabled
	if err := writeJSON(h.path(), h.state); err != nil {
		h.state.Enabled = previous
		return err
	}
	if enabled {
		h.listenLocked()
	} else {
		h.stopLocked()
		h.code = ""
	}
	return nil
}

// NewPairingCode shows a fresh code for one device to pair with.
func (h *Host) NewPairingCode() (PairingView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.Enabled {
		return PairingView{}, errors.New("turn on sharing first")
	}
	b := make([]byte, codeLength)
	if _, err := rand.Read(b); err != nil {
		return PairingView{}, err
	}
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	h.code, h.codeExpires, h.attempts = string(b), time.Now().Add(codeTTL), 0
	return PairingView{Code: formatCode(h.code), ExpiresAt: h.codeExpires}, nil
}

// Revoke removes a paired device; its token stops working immediately.
func (h *Host) Revoke(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, d := range h.state.Devices {
		if d.ID == id {
			previous := h.state.Devices
			h.state.Devices = append(append([]device(nil), h.state.Devices[:i]...), h.state.Devices[i+1:]...)
			if err := writeJSON(h.path(), h.state); err != nil {
				h.state.Devices = previous
				return err
			}
			return nil
		}
	}
	return errors.New("device not found")
}

// Status reports hosting, addresses, paired devices and an active code.
func (h *Host) Status() HostStatus {
	lan, tailscale := Addresses()
	h.mu.Lock()
	defer h.mu.Unlock()
	s := HostStatus{Enabled: h.state.Enabled, Listening: h.listening, Name: h.name, Port: h.port, LAN: lan, Tailscale: tailscale,
		Devices: []DeviceView{}, Error: h.listenErr}
	for _, d := range h.state.Devices {
		s.Devices = append(s.Devices, DeviceView{ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt, LastSeen: d.LastSeen})
	}
	if h.code != "" && time.Now().Before(h.codeExpires) {
		s.Pairing = &PairingView{Code: formatCode(h.code), ExpiresAt: h.codeExpires}
	}
	return s
}

// remotePaths are the routes a paired device may use: account views and
// actions, provider proxies, the T3 Code hub and Claude inference.
var remotePaths = []string{"/remote/", "/api/state", "/api/accounts/", "/api/usage/refresh", "/api/providers/", "/v0/management/",
	"/codex/", "/claude/", "/grok/", "/opencode/", "/antigravity/", "/gemini/", "/copilot/"}

func allowedRemotePath(path string) bool {
	for _, prefix := range remotePaths {
		if path == prefix || strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

type deviceKey struct{}

// PairedDevice reports which paired device made a request, if any.
func PairedDevice(r *http.Request) (string, bool) {
	name, ok := r.Context().Value(deviceKey{}).(string)
	return name, ok
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/remote/pair" && r.Method == http.MethodPost {
		h.pair(w, r)
		return
	}
	d, ok := h.authenticate(r.Header.Get(DeviceHeader))
	if !ok {
		w.Header().Set(rejectedHeader, "1")
		writeError(w, http.StatusUnauthorized, "this device is not paired with this Switcher")
		return
	}
	if !allowedRemotePath(r.URL.Path) {
		writeError(w, http.StatusNotFound, "not available to paired devices")
		return
	}
	r.Header.Del(DeviceHeader)
	if r.URL.Path == "/remote/hello" {
		writeJSONResponse(w, http.StatusOK, map[string]string{"name": h.name})
		return
	}
	h.next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, d.Name)))
}

func (h *Host) authenticate(token string) (device, bool) {
	if token == "" {
		return device{}, false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.state.Enabled {
		return device{}, false
	}
	for i, d := range h.state.Devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(hash)) == 1 {
			if time.Since(d.LastSeen) > time.Minute {
				h.state.Devices[i].LastSeen = time.Now().UTC()
				_ = writeJSON(h.path(), h.state)
			}
			return d, true
		}
	}
	return device{}, false
}

type pairRequest struct {
	Name  string `json:"name"`
	Proof string `json:"proof"`
}

type pairResponse struct {
	DeviceID  string   `json:"device_id"`
	Token     string   `json:"token"`
	HostName  string   `json:"host_name"`
	Addresses []string `json:"addresses"`
}

func (h *Host) pair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req) != nil {
		writeError(w, http.StatusBadRequest, "invalid pairing request")
		return
	}
	h.mu.Lock()
	if !h.state.Enabled || h.code == "" || time.Now().After(h.codeExpires) {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "no pairing code is active on the host; show a new one in its Settings")
		return
	}
	h.attempts++
	if !hmac.Equal([]byte(req.Proof), []byte(pairingProof(h.code, h.fp))) {
		if h.attempts >= codeAttempts {
			h.code = ""
		}
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "wrong pairing code")
		return
	}
	token, id := randomHex(32), randomHex(8)
	sum := sha256.Sum256([]byte(token))
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 120 {
		name = "Paired device"
	}
	h.state.Devices = append(h.state.Devices, device{ID: id, Name: name, TokenHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UTC()})
	if err := writeJSON(h.path(), h.state); err != nil {
		h.state.Devices = h.state.Devices[:len(h.state.Devices)-1]
		h.mu.Unlock()
		writeError(w, http.StatusInternalServerError, "could not save the pairing")
		return
	}
	h.code = ""
	h.mu.Unlock()
	lan, tailscale := Addresses()
	writeJSONResponse(w, http.StatusOK, pairResponse{DeviceID: id, Token: token, HostName: h.name, Addresses: append(lan, tailscale...)})
}

// pairingProof binds the code to the host certificate the client saw, so a
// device in the middle cannot complete a pairing with its own certificate.
func pairingProof(code, fp string) string {
	mac := hmac.New(sha256.New, []byte(normalizeCode(code)))
	mac.Write([]byte("switcher-pair-v1|" + fp))
	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}

func formatCode(code string) string { return code[:4] + "-" + code[4:] }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSONResponse(w, status, map[string]string{"error": message})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
