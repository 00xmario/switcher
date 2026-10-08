package main

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// The phone dashboard. While Switcher's phone access is on, the add-on serves
// HTTPS on the tailnet with Tailscale's certificate for this node and passes
// requests from the tailnet owner's own devices to Switcher's phone listener,
// together with the device's identity. Everyone else is refused here, before
// Switcher sees the request. Switcher then admits only devices approved on
// this Mac. These header names match internal/phone.
const (
	phoneKeyHeader    = "X-Switcher-Phone-Key"
	phoneNodeHeader   = "X-Switcher-Phone-Node"
	phoneDeviceHeader = "X-Switcher-Phone-Device"
	phoneOSHeader     = "X-Switcher-Phone-Os"
	phoneUserHeader   = "X-Switcher-Phone-User"
	phoneOriginHeader = "X-Switcher-Phone-Origin"
)

// selfInfo is what the gate needs to know about this node.
type selfInfo struct {
	user    tailcfg.UserID
	tagged  bool
	dnsName string // MagicDNS name without the trailing dot
}

// owner reports whether a peer belongs to the Tailscale user this node was
// signed in with. Tagged devices, devices shared in from another tailnet and
// other users of the same tailnet are refused, and so is everyone when this
// node itself is tagged and has no owner.
func owner(self selfInfo, n *tailcfg.Node) bool {
	return n != nil && !self.tagged && !self.user.IsZero() &&
		!n.IsTagged() && n.Sharer.IsZero() && n.User == self.user && !n.StableID.IsZero()
}

type identityKey struct{}

type identity struct {
	node, device, os, user, origin string
}

type phoneGate struct {
	on      atomic.Bool
	forward string // Switcher's phone listener
	token   string
	whois   func(ctx context.Context, addr string) (*apitype.WhoIsResponse, error)
	self    func(ctx context.Context) (selfInfo, error)
	listen  func() (net.Listener, error) // the tailnet's HTTPS port
	proxy   *httputil.ReverseProxy

	mu        sync.Mutex
	running   bool // the listening loop is alive
	listening bool
	lastErr   string
}

func newPhoneGate(forward, token string, whois func(context.Context, string) (*apitype.WhoIsResponse, error), self func(context.Context) (selfInfo, error), listen func() (net.Listener, error)) *phoneGate {
	p := &phoneGate{forward: forward, token: token, whois: whois, self: self, listen: listen}
	p.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			id, _ := pr.In.Context().Value(identityKey{}).(identity)
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", p.forward
			pr.Out.Host = pr.In.Host
			// Only the add-on speaks for the device: drop anything the
			// browser sent under these names.
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-switcher-phone-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Set(phoneKeyHeader, p.token)
			pr.Out.Header.Set(phoneNodeHeader, id.node)
			pr.Out.Header.Set(phoneDeviceHeader, url.QueryEscape(id.device))
			pr.Out.Header.Set(phoneOSHeader, url.QueryEscape(id.os))
			pr.Out.Header.Set(phoneUserHeader, url.QueryEscape(id.user))
			pr.Out.Header.Set(phoneOriginHeader, id.origin)
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				http.Error(w, "Switcher is not answering on this Mac.", http.StatusBadGateway)
			}
		},
	}
	return p
}

func (p *phoneGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	who, err := p.whois(ctx, r.RemoteAddr)
	self, selfErr := p.self(ctx)
	cancel()
	if err != nil || selfErr != nil || who == nil || !owner(self, who.Node) {
		http.Error(w, "This Switcher only answers its owner's devices.", http.StatusForbidden)
		return
	}
	// The certificate covers this node's name only, but check the name the
	// browser asked for too, so no other name can lead here.
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if self.dnsName == "" || host != strings.ToLower(self.dnsName) {
		http.Error(w, "Unknown address.", http.StatusMisdirectedRequest)
		return
	}
	if !p.on.Load() {
		http.Error(w, "Phone access is off on this Mac. Turn it on in Switcher's Settings.", http.StatusServiceUnavailable)
		return
	}
	device := who.Node.ComputedName
	if device == "" && who.Node.Hostinfo.Valid() {
		device = who.Node.Hostinfo.Hostname()
	}
	id := identity{node: string(who.Node.StableID), device: device, origin: "https://" + host}
	if who.Node.Hostinfo.Valid() {
		id.os = who.Node.Hostinfo.OS()
	}
	if who.UserProfile != nil {
		id.user = who.UserProfile.LoginName
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	p.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
}

// set turns phone access on or off. The first time it is on, the add-on
// starts listening on the tailnet's port 443 and keeps doing so: off only
// refuses requests.
func (p *phoneGate) set(on bool) {
	p.on.Store(on)
	if !on {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.running = true
	go p.run()
}

// run listens until it succeeds and listens again if serving stops. It ends
// only while access is off, deciding that under the lock set takes, so a
// quick off and on never leaves nothing listening.
func (p *phoneGate) run() {
	for {
		p.mu.Lock()
		if !p.on.Load() {
			p.running = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		// Listening waits until this node is signed in.
		ln, err := p.listen()
		if err != nil {
			p.mu.Lock()
			p.lastErr = phoneProblem(err)
			p.mu.Unlock()
			time.Sleep(30 * time.Second)
			continue
		}
		p.mu.Lock()
		p.listening, p.lastErr = true, ""
		p.mu.Unlock()
		(&http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}).Serve(ln)
		p.mu.Lock()
		p.listening = false
		p.mu.Unlock()
		time.Sleep(time.Second)
	}
}

// state reports whether the dashboard is reachable, or why not.
func (p *phoneGate) state() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.on.Load() && p.listening, p.lastErr
}

// phoneProblem turns a listen error into what the user can do about it.
func phoneProblem(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "MagicDNS"):
		return "Turn on MagicDNS in your tailnet's DNS settings"
	case strings.Contains(msg, "HTTPS"):
		return "Turn on HTTPS certificates in your tailnet's DNS settings"
	}
	return "The phone dashboard could not start"
}
