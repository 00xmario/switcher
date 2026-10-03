package desktoprelay_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlindTunnelPinsPublicDNSAnswerAndPreservesOpaqueBytes(t *testing.T) {
	cfg := fixtureConfig(t)
	var lookups, dials atomic.Int32
	cfg.LookupIP = func(context.Context, string) ([]net.IP, error) {
		if lookups.Add(1) > 1 {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	cfg.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		if network != "tcp" || addr != "93.184.216.34:443" {
			t.Errorf("unpinned dial %s %s", network, addr)
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			p := make([]byte, 5)
			if _, err := io.ReadFull(b, p); err != nil {
				return
			}
			b.Write(p)
		}()
		return a, nil
	}
	_, s := startFixture(t, cfg)
	c, br, r := connectRaw(t, s, "public.example.com:443", "setup")
	if r.StatusCode != 200 {
		t.Fatalf("public CONNECT = %d %s", r.StatusCode, drain(t, r))
	}
	c.Write([]byte{0x16, 0x03, 0x03, 0x00, 0x00})
	p := make([]byte, 5)
	if _, err := io.ReadFull(br, p); err != nil {
		t.Fatal(err)
	}
	if string(p) != string([]byte{0x16, 0x03, 0x03, 0x00, 0x00}) {
		t.Fatal("blind bytes intercepted")
	}
	_, _, r = connectRaw(t, s, "public.example.com:443", "setup")
	if r.StatusCode != 403 || dials.Load() != 1 || lookups.Load() != 2 {
		t.Fatalf("rebinding accepted: %d dials %d", r.StatusCode, dials.Load())
	}
}

func TestConnectHostHeaderMustMatchAuthority(t *testing.T) {
	_, s := startFixture(t, fixtureConfig(t))
	u, _ := url.Parse(s.ProxyURL)
	password, _ := u.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\nProxy-Authorization: Basic "+auth+"\r\n\r\n")
	r, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 400 {
		t.Fatalf("conflicting CONNECT Host = %d", r.StatusCode)
	}
}

func TestRejectedOuterRequestsCannotReuseStaleConnectHostCapture(t *testing.T) {
	for _, first := range []string{"GET / HTTP/1.1", "CONNECT api.anthropic.com:443 HTTP/1.1"} {
		t.Run(first, func(t *testing.T) {
			_, s := startFixture(t, fixtureConfig(t))
			u, _ := url.Parse(s.ProxyURL)
			password, _ := u.User.Password()
			auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
			c, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			br := bufio.NewReader(c)
			io.WriteString(c, first+"\r\nHost: api.anthropic.com:443\r\n\r\n")
			r, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, r)
			if !r.Close {
				t.Fatal("rejected outer connection remained reusable")
			}
			io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: evil.example.com:443\r\nProxy-Authorization: Basic "+auth+"\r\n\r\n")
			if r, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"}); err == nil && r.StatusCode == 200 {
				t.Fatal("stale Host admitted CONNECT")
			}
		})
	}
}

func TestBlindTunnelPreservesResponseAfterClientHalfClose(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, err := io.ReadAll(c)
		if err == nil && string(b) == "opaque request" {
			io.WriteString(c, "complete opaque response")
		}
	}()
	cfg := fixtureConfig(t)
	cfg.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	cfg.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, l.Addr().String())
	}
	_, s := startFixture(t, cfg)
	c, br, r := connectRaw(t, s, "public.example.com:443", "setup")
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	if _, err = io.WriteString(c, "opaque request"); err != nil {
		t.Fatal(err)
	}
	if err = c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(br)
	if err != nil || string(b) != "complete opaque response" {
		t.Fatalf("half-close lost response %q %v", b, err)
	}
	await(t, done)
}

func TestScopeDeletionClosesItsOpaqueTunnel(t *testing.T) {
	cfg := fixtureConfig(t)
	ended := make(chan struct{})
	cfg.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer close(ended)
			defer b.Close()
			p := make([]byte, 5)
			if _, err := io.ReadFull(b, p); err != nil {
				return
			}
			b.Write(p)
			io.Copy(io.Discard, b)
		}()
		return a, nil
	}
	m, s := startFixture(t, cfg)
	c, br, r := connectRaw(t, s, "public.example.com:443", "setup")
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	c.Write([]byte("hello"))
	p := make([]byte, 5)
	if _, err := io.ReadFull(br, p); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteScope(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("revoked opaque tunnel still open")
	}
	await(t, ended)
}

func TestPrivateReservedAmbiguousAndNonHTTPSAuthoritiesCannotDial(t *testing.T) {
	for _, target := range []string{"[api.anthropic.com]:443", "127.0.0.1:443", "10.0.0.1:443", "169.254.169.254:443", "192.0.2.1:443", "198.18.0.1:443", "[::1]:443", "[fc00::1]:443", "[2001:db8::1]:443", "api.anthropic.com:80", "api.anthropic.com:0443", "api.anthropic.com.:443", "API.ANTHROPIC.COM:443", "127.1:443", "2130706433:443", "api.anthropic.com@evil.com:443", "api.anthropic.com:443/evil"} {
		t.Run(target, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var dialled atomic.Bool
			cfg.DialContext = func(context.Context, string, string) (net.Conn, error) { dialled.Store(true); return nil, io.EOF }
			cfg.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil }
			_, s := startFixture(t, cfg)
			_, _, r := connectRaw(t, s, target, "setup")
			if r.StatusCode == 200 || dialled.Load() {
				t.Fatalf("unsafe authority admitted %d", r.StatusCode)
			}
		})
	}
	t.Run("mixed DNS", func(t *testing.T) {
		cfg := fixtureConfig(t)
		cfg.LookupIP = func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.1.1.1")}, nil
		}
		cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
			t.Error("mixed answer dialled")
			return nil, io.EOF
		}
		_, s := startFixture(t, cfg)
		_, _, r := connectRaw(t, s, "public.example.com:443", "setup")
		if r.StatusCode != 403 {
			t.Fatal(r.StatusCode)
		}
	})
}

func TestTLSRejectsWrongOrAbsentSNI(t *testing.T) {
	for _, sni := range []string{"evil.example.com", "sub.api.anthropic.com", ""} {
		t.Run(sni, func(t *testing.T) {
			_, s := startFixture(t, fixtureConfig(t))
			c, _, r := connectRaw(t, s, "api.anthropic.com:443", "setup")
			if r.StatusCode != 200 {
				t.Fatal(r.StatusCode)
			}
			tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: sni})
			if err := tc.Handshake(); err == nil {
				t.Fatal("wrong SNI received a certificate")
			}
		})
	}
}

func TestDecryptedHostAbsoluteURLAndAmbiguousPathCannotExfiltrate(t *testing.T) {
	requests := []string{
		"POST /v1/messages HTTP/1.1\r\nHost: evil.example.com\r\n",
		"POST https://evil.example.com/v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\n",
		"POST //evil.example.com/v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\n",
		"POST /v1/%6dessages HTTP/1.1\r\nHost: api.anthropic.com\r\n",
		"POST /v1/../v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\n",
	}
	for _, head := range requests {
		t.Run(strings.Split(head, "\r\n")[0], func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				t.Error("unsafe decrypted request reached egress")
				return nil, io.EOF
			})
			_, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			io.WriteString(c, head+"Content-Length: 2\r\n\r\n{}")
			r, err := http.ReadResponse(br, &http.Request{Method: "POST"})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, r)
			if r.StatusCode != 400 {
				t.Fatalf("unsafe request = %d", r.StatusCode)
			}
		})
	}
}

func TestRedirectAndEndToEndHeadersAreReturnedWithoutFollow(t *testing.T) {
	cfg := fixtureConfig(t)
	var calls atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "api.anthropic.com" || r.URL.Scheme != "https" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Nominated") != "" || r.Header.Get("X-End-To-End") != "kept" {
			t.Errorf("request fence/hop headers %s %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://evil.example.com/collect"}, "Connection": []string{"X-Private-Hop"}, "X-Private-Hop": []string{"strip"}, "X-Upstream": []string{"one", "two"}}, Body: io.NopCloser(strings.NewReader("exact redirect body"))}, nil
	})
	_, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	r := send(t, c, br, "/v1/messages?x=%2F&x=2", `{}`, "", http.Header{"Connection": []string{"X-Nominated"}, "X-Nominated": []string{"secret-hop"}, "Proxy-Authorization": []string{"should-strip"}, "X-End-To-End": []string{"kept"}})
	if b := drain(t, r); r.StatusCode != 307 || b != "exact redirect body" || r.Header.Get("Location") != "https://evil.example.com/collect" || r.Header.Get("X-Private-Hop") != "" || len(r.Header.Values("X-Upstream")) != 2 || calls.Load() != 1 {
		t.Fatalf("redirect changed %d %q %v", r.StatusCode, b, r.Header)
	}
}

func TestProductionTransportVerifiesUpstreamTLSBeforeSendingCredentials(t *testing.T) {
	var reached atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Store(true); w.WriteHeader(200) }))
	server.Config.ErrorLog = nil
	server.StartTLS()
	defer server.Close()
	cfg := fixtureConfig(t)
	cfg.Transport = nil
	cfg.FixtureTLSRoots = x509.NewCertPool()
	cfg.Source = fixtureSource()
	cfg.LookupIP = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	cfg.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	drain(t, r)
	if r.StatusCode != 502 || reached.Load() {
		t.Fatal("untrusted upstream TLS accepted")
	}
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	r = send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	drain(t, r)
	if r.StatusCode != 502 || reached.Load() {
		t.Fatal("selected OAuth reached an unverified TLS upstream")
	}
}

func TestHeadersAndAdvertisedBodiesAreBoundedBeforeUpstream(t *testing.T) {
	for _, head := range []string{"Content-Length: 16777217\r\n", "X-Large: " + strings.Repeat("a", 48<<10) + "\r\nContent-Length: 0\r\n"} {
		t.Run(head[:12], func(t *testing.T) {
			_, s := startFixture(t, fixtureConfig(t))
			c, br := tunnel(t, s)
			io.WriteString(c, "POST /v1/messages HTTP/1.1\r\nHost: api.anthropic.com\r\n"+head+"\r\n")
			r, err := http.ReadResponse(br, &http.Request{Method: "POST"})
			if err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != 413 && r.StatusCode != 431 {
				t.Fatalf("oversized input status %d", r.StatusCode)
			}
		})
	}
}
