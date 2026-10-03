package desktoprelay_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRealTransportDoesNotRetryEmptyRequestOnFailingReusedConnection(t *testing.T) {
	var reached, dials atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	roots := x509.NewCertPool()
	roots.AddCert(upstream.Certificate())
	transport := &http.Transport{Proxy: nil, DisableCompression: true, DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api.anthropic.com:443" {
			t.Errorf("origin changed %q", addr)
		}
		c, err := (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		tc := tls.Client(c, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"})
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		if dials.Add(1) == 1 {
			return &failSecondWriteConn{Conn: tc}, nil
		}
		return tc, nil
	}}
	defer transport.CloseIdleConnections()
	cfg := fixtureConfig(t)
	cfg.Transport = transport
	_, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	request := func() *http.Response {
		r, _ := http.NewRequest("GET", "https://api.anthropic.com/v1/models", strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer caller-token")
		r.Header.Set("Idempotency-Key", "fixture-idempotent")
		if err := r.Write(c); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(br, r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r := request()
	drain(t, r)
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	r = request()
	drain(t, r)
	if r.StatusCode != 502 || dials.Load() != 1 || reached.Load() != 1 {
		t.Fatalf("implicit empty-body network retry: status %d dials %d reached %d", r.StatusCode, dials.Load(), reached.Load())
	}
}

type failSecondWriteConn struct {
	net.Conn
	writes atomic.Int32
}

func (c *failSecondWriteConn) Write(p []byte) (int, error) {
	if c.writes.Add(1) == 2 {
		c.Conn.Close()
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(p)
}
