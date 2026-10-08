package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const me = tailcfg.UserID(7)

func node(user, sharer tailcfg.UserID, tags ...string) *tailcfg.Node {
	return &tailcfg.Node{StableID: "nPhone", User: user, Sharer: sharer, Tags: tags, ComputedName: "marios-iphone",
		Hostinfo: (&tailcfg.Hostinfo{OS: "iOS"}).View()}
}

func TestOnlyTheOwnersOwnDevicesAreAdmitted(t *testing.T) {
	self := selfInfo{user: me, dnsName: "switcher-mac.tail1.ts.net"}
	for _, tc := range []struct {
		name string
		self selfInfo
		node *tailcfg.Node
		want bool
	}{
		{"the owner's phone", self, node(me, 0), true},
		{"another user in the tailnet", self, node(8, 0), false},
		{"a device shared in from another tailnet", self, node(me, 8), false},
		{"a tagged device", self, node(me, 0, "tag:server"), false},
		{"no device", self, nil, false},
		{"a device without a stable id", self, &tailcfg.Node{User: me}, false},
		{"this node is tagged and has no owner", selfInfo{user: me, tagged: true}, node(me, 0), false},
		{"this node is not signed in", selfInfo{}, node(0, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := owner(tc.self, tc.node); got != tc.want {
				t.Fatalf("owner = %v, want %v", got, tc.want)
			}
		})
	}
}

// gateFor returns a gate in front of a backend that records what reached it.
func gateFor(t *testing.T, who *tailcfg.Node, on bool) (*phoneGate, *http.Request) {
	t.Helper()
	got := new(http.Request)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		_, _ = w.Write([]byte("switcher"))
	}))
	t.Cleanup(backend.Close)
	whois := func(context.Context, string) (*apitype.WhoIsResponse, error) {
		if who == nil {
			return nil, errors.New("unknown peer")
		}
		return &apitype.WhoIsResponse{Node: who, UserProfile: &tailcfg.UserProfile{LoginName: "me@example.com"}}, nil
	}
	self := func(context.Context) (selfInfo, error) {
		return selfInfo{user: me, dnsName: "switcher-mac.tail1.ts.net"}, nil
	}
	p := newPhoneGate(strings.TrimPrefix(backend.URL, "http://"), "secret-token", whois, self)
	p.on.Store(on)
	return p, got
}

func serve(p *phoneGate, host string, header http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/api/session", nil)
	r.RemoteAddr = "100.100.1.2:51234"
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestGateForwardsTheOwnersPhoneWithItsIdentity(t *testing.T) {
	p, got := gateFor(t, node(me, 0), true)
	spoof := http.Header{"X-Switcher-Phone-Node": {"nAttacker"}, "X-Switcher-Phone-Key": {"guess"}, "X-Switcher-Phone-Extra": {"1"}}
	w := serve(p, "switcher-mac.tail1.ts.net", spoof)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got.Header.Get(phoneKeyHeader) != "secret-token" || got.Header.Get(phoneNodeHeader) != "nPhone" {
		t.Fatalf("identity not set by the add-on: key=%q node=%q", got.Header.Get(phoneKeyHeader), got.Header.Get(phoneNodeHeader))
	}
	if got.Header.Get("X-Switcher-Phone-Extra") != "" {
		t.Fatal("a browser-sent phone header reached Switcher")
	}
	if device, _ := url.QueryUnescape(got.Header.Get(phoneDeviceHeader)); device != "marios-iphone" {
		t.Fatalf("device %q", device)
	}
	if got.Header.Get(phoneOriginHeader) != "https://switcher-mac.tail1.ts.net" || got.Host != "switcher-mac.tail1.ts.net" {
		t.Fatalf("origin %q host %q", got.Header.Get(phoneOriginHeader), got.Host)
	}
}

func TestGateRefusesEveryoneElse(t *testing.T) {
	for _, tc := range []struct {
		name string
		who  *tailcfg.Node
		host string
		on   bool
		want int
	}{
		{"another user", node(8, 0), "switcher-mac.tail1.ts.net", true, http.StatusForbidden},
		{"an unknown peer", nil, "switcher-mac.tail1.ts.net", true, http.StatusForbidden},
		{"another name", node(me, 0), "evil.example.com", true, http.StatusMisdirectedRequest},
		{"phone access off", node(me, 0), "switcher-mac.tail1.ts.net", false, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, got := gateFor(t, tc.who, tc.on)
			if w := serve(p, tc.host, nil); w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
			if got.Header.Get(phoneKeyHeader) != "" {
				t.Fatal("a refused request reached Switcher")
			}
		})
	}
}
