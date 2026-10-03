package desktoprelay

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxHeaders = 32 << 10

func quietServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: maxHeaders, ErrorLog: log.New(io.Discard, "", 0)}
}

func ingressServer(m *Manager, r *runtime) *http.Server {
	s := quietServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { m.connect(r, w, req) }))
	s.BaseContext = func(net.Listener) context.Context { return r.ctx }
	s.ConnContext = ingressContext
	return s
}

func (m *Manager) authenticate(req *http.Request) (string, bool) {
	v := req.Header.Values("Proxy-Authorization")
	if len(v) != 1 || !strings.HasPrefix(v[0], "Basic ") || len(v[0]) > 512 {
		return "", false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(v[0], "Basic "))
	if err != nil {
		return "", false
	}
	id, secret, ok := strings.Cut(string(b), ":")
	if !ok {
		return "", false
	}
	m.mu.Lock()
	s, exists := m.state.Scopes[id]
	if m.failed {
		exists = false
	}
	m.mu.Unlock()
	expected := sha256.Sum256([]byte(s.Secret))
	actual := sha256.Sum256([]byte(secret))
	valid := subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
	return id, exists && valid
}

func (m *Manager) scopeExists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.state.Scopes[id]
	return ok
}

func (m *Manager) connect(r *runtime, w http.ResponseWriter, req *http.Request) {
	closeBody := m.guardBody(w, req)
	defer closeBody()
	// One outer admission attempt per socket. Inner TLS HTTP keepalive is
	// independent; rejected CONNECT attempts must never reuse captured headers.
	w.Header().Set("Connection", "close")
	peer, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil || !net.ParseIP(peer).IsLoopback() {
		http.Error(w, "loopback required", 403)
		return
	}
	if req.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", 405)
		return
	}
	id, ok := m.authenticate(req)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="Switcher Desktop relay"`)
		http.Error(w, "scope admission required", 407)
		return
	}
	host, err := authority(req.RequestURI)
	wire, _ := req.Context().Value(ingressContextKey{}).(*ingressConn)
	if err != nil || req.Host != req.RequestURI || wire == nil || wire.hostCount != 1 || wire.host != req.RequestURI || req.ContentLength > 0 || len(req.TransferEncoding) != 0 {
		http.Error(w, "invalid CONNECT authority", 400)
		return
	}
	closeBody()
	var upstream net.Conn
	if host != originHost {
		upstream, err = m.dialPublic(req.Context(), "tcp", req.RequestURI)
		if err != nil {
			http.Error(w, "destination unavailable or forbidden", 403)
			return
		}
		defer upstream.Close()
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", 503)
		return
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer c.Close()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.conns[c] = tunnelRecord{scope: id, blind: upstream != nil}
	r.wg.Add(1)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.conns, c); r.mu.Unlock(); r.wg.Done() }()
	if !m.scopeExists(id) {
		return
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = rw.Flush(); err != nil {
		return
	}
	// Hijack may have buffered a pipelined ClientHello or blind tunnel bytes.
	buffered := &bufferConn{Conn: c, reader: rw.Reader}
	if upstream != nil {
		c.SetDeadline(time.Time{})
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
	tc := tls.Server(buffered, &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName != originHost {
			return nil, errors.New("SNI outside relay origin")
		}
		return &leaf, nil
	}})
	if err := tc.HandshakeContext(r.ctx); err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	one := newOneListener(tc)
	inner := quietServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { m.forward(r, id, w, req) }))
	inner.BaseContext = func(net.Listener) context.Context { return r.ctx }
	inner.Serve(one)
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
