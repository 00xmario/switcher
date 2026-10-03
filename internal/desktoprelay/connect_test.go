package desktoprelay_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func startFixture(t *testing.T, cfg desktoprelay.Config) (*desktoprelay.Manager, desktoprelay.ScopeSetup) {
	t.Helper()
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	s, err := m.CreateScope("fixture")
	if err != nil {
		t.Fatal(err)
	}
	return m, s
}

func TestPipelinedClientHelloSurvivesConnectHijackBuffer(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("pipeline preserved"))}, nil
	})
	_, s := startFixture(t, cfg)
	u, _ := url.Parse(s.ProxyURL)
	password, _ := u.User.Password()
	auth := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	header := "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: Basic " + auth + "\r\n\r\n"
	wrapped := &pipelineConn{Conn: c, head: header, reader: bufio.NewReader(c)}
	pem, err := os.ReadFile(s.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	tc := tls.Client(wrapped, &tls.Config{RootCAs: pool, ServerName: "api.anthropic.com"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if b := drain(t, send(t, tc, bufio.NewReader(tc), "/v1/messages", `{}`, "", nil)); b != "pipeline preserved" {
		t.Fatal(b)
	}
}

type pipelineConn struct {
	net.Conn
	head         string
	reader       *bufio.Reader
	wrote, ready bool
}

func (c *pipelineConn) Write(p []byte) (int, error) {
	if !c.wrote {
		c.wrote = true
		data := append([]byte(c.head), p...)
		n, err := c.Conn.Write(data)
		if err != nil {
			return 0, err
		}
		if n != len(data) {
			return 0, io.ErrShortWrite
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}
func (c *pipelineConn) Read(p []byte) (int, error) {
	if !c.ready {
		r, err := http.ReadResponse(c.reader, &http.Request{Method: "CONNECT"})
		if err != nil {
			return 0, err
		}
		if r.StatusCode != 200 {
			return 0, errors.New("fixture CONNECT failed")
		}
		c.ready = true
	}
	return c.reader.Read(p)
}

func connectRaw(t *testing.T, setup desktoprelay.ScopeSetup, authority, auth string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	u, err := url.Parse(setup.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if auth == "setup" {
		password, _ := u.User.Password()
		auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+password))
	}
	_, err = io.WriteString(c, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+"\r\nProxy-Authorization: "+auth+"\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	r, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	return c, br, r
}

func tunnel(t *testing.T, setup desktoprelay.ScopeSetup) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	c, _, resp := connectRaw(t, setup, "api.anthropic.com:443", "setup")
	if resp.StatusCode != 200 {
		t.Fatalf("CONNECT status %d", resp.StatusCode)
	}
	pem, err := os.ReadFile(setup.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("bad CA")
	}
	tc := tls.Client(c, &tls.Config{RootCAs: pool, ServerName: "api.anthropic.com", MinVersion: tls.VersionTLS12})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return tc, bufio.NewReader(tc)
}

func send(t *testing.T, c net.Conn, br *bufio.Reader, path, body, session string, extra http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequest("POST", "https://api.anthropic.com"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer caller-token")
	r.Header.Set("X-Api-Key", "caller-key")
	r.Header.Set("Anthropic-Beta", "oauth-2025-04-20,caller-feature")
	r.Header.Set("Content-Type", "application/json")
	if session != "" {
		r.Header.Set("X-Claude-Code-Session-Id", session)
	}
	for k, v := range extra {
		r.Header[k] = v
	}
	if err := r.Write(c); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthenticatedConnectTLSKeepsMultipleRequestsAndCallerBytes(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.String() != "https://api.anthropic.com/v1/messages?beta=true" || r.Host != "api.anthropic.com" || r.Header.Get("Authorization") != "Bearer caller-token" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("unsafe forwarding: %s, %v", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 201, Header: http.Header{"X-Fixture": []string{"kept"}}, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})
	_, s := startFixture(t, cfg)
	_, _, rejected := connectRaw(t, s, "api.anthropic.com:443", "Basic bad")
	if rejected.StatusCode != 407 {
		t.Fatalf("unauthenticated CONNECT = %d", rejected.StatusCode)
	}
	c, br := tunnel(t, s)
	body := `{"model":"native-model","messages":[{"content":"exact bytes"}],"unknown":{"signed":"keep"}}`
	for i := 0; i < 2; i++ {
		resp := send(t, c, br, "/v1/messages?beta=true", body, "", nil)
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 201 || string(b) != body || resp.Header.Get("X-Fixture") != "kept" {
			t.Fatalf("response %d: %s, %v", resp.StatusCode, b, err)
		}
	}
}
