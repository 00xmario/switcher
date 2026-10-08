// switcher-tailnet is Switcher's optional Tailscale add-on. Switcher downloads
// it only when "Away from home" is turned on, so the app itself carries no
// Tailscale code. It joins the user's tailnet as its own node (no system VPN),
// forwards the tailnet's port 8788 to this Mac's Switcher host listener, and
// gives Switcher a loopback CONNECT proxy to reach other Switchers. With phone
// access on, it also serves the phone dashboard over HTTPS (see phone.go).
//
// Switcher starts it with SWITCHER_TAILNET_TOKEN set (and, for the phone
// dashboard, SWITCHER_TAILNET_PHONE and SWITCHER_TAILNET_PHONE_KEY) and reads one JSON line
// with the control address from stdout. It exits when stdin closes, so it
// never outlives Switcher.
package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tailscale.com/tsnet"
)

// hostPrefix marks tailnet nodes that run Switcher, for discovery.
const hostPrefix = "switcher-"

func main() {
	dir := flag.String("dir", "", "state directory")
	hostname := flag.String("hostname", "", "tailnet host name, starting with switcher-")
	forward := flag.String("forward", "127.0.0.1:8788", "local Switcher host listener")
	port := flag.Int("port", 8788, "tailnet port to serve")
	forwardOn := flag.Bool("forwarding", false, "forward tailnet connections from the start")
	flag.Parse()
	// Forward only while this Mac's Switcher is a host, so nothing else that
	// may listen on the local port is exposed to the tailnet.
	var forwarding atomic.Bool
	forwarding.Store(*forwardOn)
	token := os.Getenv("SWITCHER_TAILNET_TOKEN")
	if *dir == "" || *hostname == "" || token == "" {
		log.Fatal("switcher-tailnet is started by Switcher")
	}
	srv := &tsnet.Server{Dir: *dir, Hostname: *hostname, Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	defer srv.Close()
	lc, err := srv.LocalClient()
	if err != nil {
		log.Fatalf("local client: %v", err)
	}

	var selfMu sync.Mutex
	var selfCached selfInfo
	var selfAt time.Time
	self := func(ctx context.Context) (selfInfo, error) {
		selfMu.Lock()
		defer selfMu.Unlock()
		if time.Since(selfAt) < 10*time.Second {
			return selfCached, nil
		}
		st, err := lc.StatusWithoutPeers(ctx)
		if err != nil || st.Self == nil || st.BackendState != "Running" {
			return selfInfo{}, fmt.Errorf("not signed in")
		}
		selfCached = selfInfo{user: st.Self.UserID, tagged: st.Self.Tags != nil && st.Self.Tags.Len() > 0,
			dnsName: strings.TrimSuffix(st.Self.DNSName, ".")}
		selfAt = time.Now()
		return selfCached, nil
	}
	// The phone dashboard is configured through the environment so that a
	// Switcher never passes a flag an older add-on would reject. Its key is
	// separate from the control token: it travels with every phone request.
	var phone *phoneGate
	if forward, key := os.Getenv("SWITCHER_TAILNET_PHONE"), os.Getenv("SWITCHER_TAILNET_PHONE_KEY"); forward != "" && len(key) >= 32 {
		phone = newPhoneGate(forward, key, lc.WhoIs, self, func() (net.Listener, error) { return srv.ListenTLS("tcp", ":443") })
	}

	// Tailnet peers reach this Mac's Switcher host through the node.
	go func() {
		ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Printf("listen on tailnet: %v", err)
			return
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if !forwarding.Load() {
				conn.Close()
				continue
			}
			go func() {
				defer conn.Close()
				local, err := net.DialTimeout("tcp", *forward, 5*time.Second)
				if err != nil {
					return
				}
				defer local.Close()
				splice(conn, local)
			}()
		}
	}()

	authorized := func(r *http.Request) bool {
		got := r.Header.Get("X-Switcher-Tailnet")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Bearer ")
		}
		return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
	}
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodConnect:
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			remote, err := srv.Dial(ctx, "tcp", r.Host)
			cancel()
			if err != nil {
				http.Error(w, "unreachable on the tailnet", http.StatusBadGateway)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				remote.Close()
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				remote.Close()
				return
			}
			rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			rw.Flush()
			go func() {
				defer conn.Close()
				defer remote.Close()
				splice(&bufferedConn{Conn: conn, r: rw.Reader}, remote)
			}()
		case r.URL.Path == "/status":
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			st, err := lc.Status(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			out := status{State: st.BackendState, AuthURL: st.AuthURL, Peers: []peer{}}
			if st.CurrentTailnet != nil {
				out.Tailnet = st.CurrentTailnet.Name
				out.HTTPS = st.CurrentTailnet.MagicDNSEnabled && len(st.CertDomains) > 0
			}
			if phone != nil {
				out.Phone, out.PhoneError = phone.state()
			}
			if st.Self != nil {
				out.Name = st.Self.HostName
				out.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
				for _, ip := range st.Self.TailscaleIPs {
					if ip.Is4() {
						out.IP = ip.String()
					}
				}
			}
			for _, p := range st.Peer {
				if !strings.HasPrefix(strings.ToLower(p.HostName), hostPrefix) {
					continue
				}
				entry := peer{Name: p.HostName, DNSName: strings.TrimSuffix(p.DNSName, "."), Online: p.Online}
				for _, ip := range p.TailscaleIPs {
					if ip.Is4() {
						entry.IP = ip.String()
					}
				}
				out.Peers = append(out.Peers, entry)
			}
			sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].Name < out.Peers[j].Name })
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/phone" && r.Method == http.MethodPost:
			if phone == nil {
				http.NotFound(w, r)
				return
			}
			phone.set(r.URL.Query().Get("on") == "1")
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/forwarding" && r.Method == http.MethodPost:
			forwarding.Store(r.URL.Query().Get("on") == "1")
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/logout" && r.Method == http.MethodPost:
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if err := lc.Logout(ctx); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("control listener: %v", err)
	}
	go (&http.Server{Handler: control, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	json.NewEncoder(os.Stdout).Encode(map[string]string{"control": ln.Addr().String()})

	// Exit with Switcher: it holds our stdin open for as long as it runs.
	io.Copy(io.Discard, bufio.NewReader(os.Stdin))
}

type status struct {
	State   string `json:"state"`
	AuthURL string `json:"auth_url,omitempty"`
	Tailnet string `json:"tailnet,omitempty"`
	Name    string `json:"name,omitempty"`
	DNSName string `json:"dns_name,omitempty"`
	IP      string `json:"ip,omitempty"`
	Peers   []peer `json:"peers"`
	// HTTPS reports that the tailnet issues certificates, which the phone
	// dashboard needs; Phone that the dashboard is being served.
	HTTPS      bool   `json:"https"`
	Phone      bool   `json:"phone"`
	PhoneError string `json:"phone_error,omitempty"`
}

type peer struct {
	Name    string `json:"name"`
	DNSName string `json:"dns_name"`
	IP      string `json:"ip"`
	Online  bool   `json:"online"`
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}
