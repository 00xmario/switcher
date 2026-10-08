// Package phone lets the owner use Switcher from their phone over Tailscale.
//
// The Tailscale add-on serves the phone dashboard with this node's HTTPS
// certificate and passes on only requests from devices of the tailnet user
// who signed it in, adding the device's identity. Switcher then admits only
// phones approved on this Mac: a phone shows a code, and the owner types that
// code into Switcher on the Mac. The phone receives a session cookie that
// works only from that same Tailscale device. The phone listener serves a
// small page and a fixed list of actions; settings, logins and keys stay on
// the Mac.
package phone

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	codeTTL     = 5 * time.Minute
	maxPending  = 3
	idleExpiry  = 30 * 24 * time.Hour
	maxSession  = 90 * 24 * time.Hour
	maxFailures = 10
	// requestGap is how often one device may ask for a new code.
	requestGap = 5 * time.Second
)

var (
	ErrNoCode      = errors.New("no phone is waiting with that code")
	ErrTooMany     = errors.New("too many phones are waiting; approve or deny one first")
	ErrNotFound    = errors.New("no such phone")
	ErrOff         = errors.New("phone access is off on this Mac")
	ErrTooManyMiss = errors.New("too many wrong codes; ask the phone for a new one")
	ErrTooSoon     = errors.New("wait a few seconds before asking for another code")
)

// Identity is the Tailscale device a request came from, as the add-on
// reported it.
type Identity struct {
	Node   string // stable node id
	Device string
	OS     string
	User   string // Tailscale login, for display only
}

// Device is an approved phone.
type Device struct {
	ID          string    `json:"id"`
	Node        string    `json:"node"`
	Name        string    `json:"name"`
	OS          string    `json:"os,omitempty"`
	User        string    `json:"user,omitempty"`
	PairedAt    time.Time `json:"paired_at"`
	LastSeen    time.Time `json:"last_seen"`
	SessionHash string    `json:"session_hash"`
	CSRF        string    `json:"csrf"`
}

// DeviceView is a device without its secrets.
type DeviceView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	OS       string    `json:"os,omitempty"`
	User     string    `json:"user,omitempty"`
	PairedAt time.Time `json:"paired_at"`
	LastSeen time.Time `json:"last_seen"`
}

// Pending is a phone waiting for approval.
type Pending struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	OS       string    `json:"os,omitempty"`
	User     string    `json:"user,omitempty"`
	Expires  time.Time `json:"expires_at"`
	Approved bool      `json:"approved"`
	node     string
	code     string
	pollHash string
}

type file struct {
	Enabled bool     `json:"enabled"`
	Devices []Device `json:"devices"`
}

// Access holds the phone access choice, approved phones and waiting phones.
type Access struct {
	path     string
	mu       sync.Mutex
	state    file
	pending  []*Pending
	misses   int
	missFrom time.Time
	asked    map[string]time.Time // last code request per device
	now      func() time.Time
}

// New loads the saved state. An unreadable file leaves phone access off.
func New(path string) *Access {
	a := &Access{path: path, now: time.Now}
	if body, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(body, &a.state) != nil {
			a.state = file{}
		}
	}
	return a
}

// Enabled reports whether phone access is on.
func (a *Access) Enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.Enabled
}

// SetEnabled turns phone access on or off. Turning it off also drops phones
// waiting for approval; approved phones stay until revoked, and get back in
// when access is turned on again.
func (a *Access) SetEnabled(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := a.state.Enabled
	a.state.Enabled = on
	if err := a.saveLocked(); err != nil {
		a.state.Enabled = before
		return err
	}
	if !on {
		a.pending = nil
	}
	return nil
}

// Devices lists approved phones.
func (a *Access) Devices() []DeviceView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]DeviceView, 0, len(a.state.Devices))
	for _, d := range a.state.Devices {
		out = append(out, DeviceView{ID: d.ID, Name: d.Name, OS: d.OS, User: d.User, PairedAt: d.PairedAt, LastSeen: d.LastSeen})
	}
	return out
}

// Waiting lists phones waiting for approval.
func (a *Access) Waiting() []Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropExpiredLocked()
	out := make([]Pending, 0, len(a.pending))
	for _, p := range a.pending {
		out = append(out, *p)
	}
	return out
}

// Request starts approval for a phone. It returns the code to show on the
// phone and a secret the phone presents when it asks whether it was approved.
// A phone that asks again replaces its earlier request.
func (a *Access) Request(id Identity) (code, poll string, expires time.Time, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.state.Enabled {
		return "", "", time.Time{}, ErrOff
	}
	a.dropExpiredLocked()
	if a.asked == nil {
		a.asked = map[string]time.Time{}
	}
	if last, ok := a.asked[id.Node]; ok && a.now().Sub(last) < requestGap {
		return "", "", time.Time{}, ErrTooSoon
	}
	for node, at := range a.asked {
		if a.now().Sub(at) > codeTTL {
			delete(a.asked, node)
		}
	}
	a.asked[id.Node] = a.now()
	kept := a.pending[:0]
	for _, p := range a.pending {
		if p.node != id.Node {
			kept = append(kept, p)
		}
	}
	a.pending = kept
	if len(a.pending) >= maxPending {
		return "", "", time.Time{}, ErrTooMany
	}
	code = a.newCodeLocked()
	poll = randomHex(32)
	p := &Pending{ID: randomHex(8), Name: id.Device, OS: id.OS, User: id.User, Expires: a.now().Add(codeTTL),
		node: id.Node, code: code, pollHash: hash(poll)}
	a.pending = append(a.pending, p)
	return code, poll, p.Expires, nil
}

// Approve lets the phone showing code in. Only Switcher on this Mac calls it.
func (a *Access) Approve(code string) (Pending, error) {
	code = normalizeCode(code)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.state.Enabled {
		return Pending{}, ErrOff
	}
	a.dropExpiredLocked()
	if a.now().Sub(a.missFrom) > codeTTL {
		a.misses, a.missFrom = 0, a.now()
	}
	if a.misses >= maxFailures {
		return Pending{}, ErrTooManyMiss
	}
	for _, p := range a.pending {
		if len(code) == len(p.code) && subtle.ConstantTimeCompare([]byte(code), []byte(p.code)) == 1 && !p.Approved {
			p.Approved = true
			return *p, nil
		}
	}
	a.misses++
	return Pending{}, ErrNoCode
}

// Deny drops a waiting phone.
func (a *Access) Deny(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.pending {
		if p.ID == id {
			a.pending = append(a.pending[:i], a.pending[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// Revoke signs an approved phone out for good.
func (a *Access) Revoke(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, d := range a.state.Devices {
		if d.ID == id {
			before := append([]Device(nil), a.state.Devices...)
			a.state.Devices = append(a.state.Devices[:i], a.state.Devices[i+1:]...)
			if err := a.saveLocked(); err != nil {
				a.state.Devices = before
				return err
			}
			return nil
		}
	}
	return ErrNotFound
}

// PollResult is what a waiting phone learns.
type PollResult struct {
	Status  string // "waiting", "approved" or "expired"
	Code    string
	Expires time.Time
	Session string // set once, when approved
	CSRF    string
}

// Poll tells a waiting phone whether it was approved. On approval the phone
// becomes a device bound to its Tailscale node and receives its session.
func (a *Access) Poll(id Identity, poll string) (PollResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.state.Enabled {
		return PollResult{}, ErrOff
	}
	want := hash(poll)
	for i, p := range a.pending {
		if p.node != id.Node || subtle.ConstantTimeCompare([]byte(p.pollHash), []byte(want)) != 1 {
			continue
		}
		if a.now().After(p.Expires) {
			a.pending = append(a.pending[:i], a.pending[i+1:]...)
			return PollResult{Status: "expired"}, nil
		}
		if !p.Approved {
			return PollResult{Status: "waiting", Code: p.code, Expires: p.Expires}, nil
		}
		session, csrf := randomHex(32), randomHex(32)
		device := Device{ID: randomHex(8), Node: p.node, Name: p.Name, OS: p.OS, User: p.User,
			PairedAt: a.now().UTC(), LastSeen: a.now().UTC(), SessionHash: hash(session), CSRF: csrf}
		before := append([]Device(nil), a.state.Devices...)
		// One device per Tailscale node: pairing again replaces the old one.
		kept := a.state.Devices[:0:0]
		for _, d := range a.state.Devices {
			if d.Node != p.node {
				kept = append(kept, d)
			}
		}
		a.state.Devices = append(kept, device)
		if err := a.saveLocked(); err != nil {
			a.state.Devices = before
			return PollResult{}, err
		}
		a.pending = append(a.pending[:i], a.pending[i+1:]...)
		return PollResult{Status: "approved", Session: session, CSRF: csrf}, nil
	}
	return PollResult{Status: "expired"}, nil
}

// Session returns the approved phone a session cookie belongs to. The cookie
// works only from the Tailscale device it was issued to, while phone access
// is on, within 30 days of the last use and 90 days of pairing.
func (a *Access) Session(id Identity, session string) (DeviceView, string, bool) {
	if session == "" || id.Node == "" {
		return DeviceView{}, "", false
	}
	want := hash(session)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.state.Enabled {
		return DeviceView{}, "", false
	}
	now := a.now()
	for i, d := range a.state.Devices {
		if subtle.ConstantTimeCompare([]byte(d.SessionHash), []byte(want)) != 1 {
			continue
		}
		if d.Node != id.Node || now.Sub(d.LastSeen) > idleExpiry || now.Sub(d.PairedAt) > maxSession {
			return DeviceView{}, "", false
		}
		if now.Sub(d.LastSeen) > time.Minute {
			a.state.Devices[i].LastSeen = now.UTC()
			_ = a.saveLocked()
		}
		return DeviceView{ID: d.ID, Name: d.Name, OS: d.OS, User: d.User, PairedAt: d.PairedAt, LastSeen: d.LastSeen}, d.CSRF, true
	}
	return DeviceView{}, "", false
}

func (a *Access) dropExpiredLocked() {
	now := a.now()
	kept := a.pending[:0]
	for _, p := range a.pending {
		if now.Before(p.Expires) {
			kept = append(kept, p)
		}
	}
	a.pending = kept
}

func (a *Access) newCodeLocked() string {
	for {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			panic(err)
		}
		code := fmt.Sprintf("%06d", n.Int64())
		unique := true
		for _, p := range a.pending {
			if p.code == code {
				unique = false
			}
		}
		if unique {
			return code
		}
	}
}

func (a *Access) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// normalizeCode keeps the digits of what the user typed, so "482 913" and
// "482-913" both work.
func normalizeCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if unicode.IsDigit(r) && r < 128 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
