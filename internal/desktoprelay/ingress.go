package desktoprelay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func quietServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
}

func ingressServer(m *Manager, r *runtime) *http.Server {
	s := quietServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { m.connect(r, w, req) }))
	s.BaseContext = func(net.Listener) context.Context { return r.ctx }
	return s
}

func (m *Manager) authenticate(req *http.Request) (string, bool) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "Basic "))
	if err != nil {
		return "", false
	}
	id, secret, ok := strings.Cut(string(b), ":")
	if !ok {
		return "", false
	}
	m.mu.Lock()
	s, exists := m.state.Scopes[id]
	m.mu.Unlock()
	expected := sha256.Sum256([]byte(s.Secret))
	actual := sha256.Sum256([]byte(secret))
	return id, exists && subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}

// connect intercepts api.anthropic.com so the credential can be swapped, and
// tunnels every other destination unchanged.
func (m *Manager) connect(r *runtime, w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	scope, ok := m.authenticate(req)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="Switcher Desktop relay"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	var upstream net.Conn
	if req.Host != originHost+":443" {
		var err error
		if upstream, err = m.dial(req.Context(), "tcp", req.Host); err != nil {
			http.Error(w, "destination unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	if !r.track(c) {
		return
	}
	defer r.untrack(c)
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = rw.Flush(); err != nil {
		return
	}
	// Hijack may have buffered a pipelined ClientHello or tunnel bytes.
	buffered := &bufferConn{Conn: c, reader: rw.Reader}
	if upstream != nil {
		splice(r.ctx, buffered, upstream)
		return
	}
	m.mu.Lock()
	cert, key := m.state.Certificate, m.state.PrivateKey
	m.mu.Unlock()
	leaf, err := leafCertificate(cert, key)
	if err != nil {
		return
	}
	tc := tls.Server(buffered, &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}, Certificates: []tls.Certificate{leaf}})
	if err := tc.HandshakeContext(r.ctx); err != nil {
		return
	}
	inner := quietServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { m.forward(r, scope, w, req) }))
	inner.BaseContext = func(net.Listener) context.Context { return r.ctx }
	inner.Serve(newOneListener(tc))
	inner.Close()
}

type bufferConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

type closeConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *closeConn) Close() error { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }

// oneListener serves exactly one already-accepted connection.
type oneListener struct {
	conn *closeConn
	once bool
}

func newOneListener(c net.Conn) *oneListener {
	return &oneListener{conn: &closeConn{Conn: c, closed: make(chan struct{})}}
}
func (l *oneListener) Accept() (net.Conn, error) {
	if !l.once {
		l.once = true
		return l.conn, nil
	}
	<-l.conn.closed
	return nil, net.ErrClosed
}
func (l *oneListener) Close() error   { return l.conn.Close() }
func (l *oneListener) Addr() net.Addr { return l.conn.LocalAddr() }

func splice(ctx context.Context, a, b net.Conn) {
	stop := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	copyTo := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
		} else if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
		done <- struct{}{}
	}
	go copyTo(a, b)
	go copyTo(b, a)
	<-done
	<-done
}
