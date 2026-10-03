package desktoprelay_test

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func fixtureAdmission(t *testing.T, s desktoprelay.ScopeSetup) (string, string) {
	t.Helper()
	u, err := url.Parse(s.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	return u.Host, "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
}

func expectBoundedEOF(t *testing.T, c net.Conn, within time.Duration) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(within))
	_, err := io.Copy(io.Discard, c)
	if err != nil {
		t.Fatalf("rejected incomplete body did not close within %s: %v", within, err)
	}
}

func TestIncompleteRejectedConnectBodiesReleaseAllAdmissionSlots(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprint("authenticated=", authenticated), func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.FixtureReadTimeout = 150 * time.Millisecond
			_, s := startFixture(t, cfg)
			address, auth := fixtureAdmission(t, s)
			if !authenticated {
				auth = "Basic invalid"
			}
			var clients []net.Conn
			defer func() {
				for _, c := range clients {
					c.Close()
				}
			}()
			for i := 0; i < 128; i++ {
				c, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, c)
				c.SetWriteDeadline(time.Now().Add(time.Second))
				_, err = io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: "+auth+"\r\nContent-Length: 64\r\n\r\nx")
				if err != nil {
					t.Fatal(err)
				}
			}
			var wg sync.WaitGroup
			var stuck atomic.Int32
			for _, c := range clients {
				wg.Add(1)
				go func(c net.Conn) {
					defer wg.Done()
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					if _, err := io.Copy(io.Discard, c); err != nil {
						stuck.Add(1)
					}
				}(c)
			}
			wg.Wait()
			if n := stuck.Load(); n != 0 {
				t.Fatalf("%d incomplete rejected CONNECTs retained admissions", n)
			}
			// Exercise every slot again without closing the first batch from the client.
			_, auth = fixtureAdmission(t, s)
			// net/http may send FIN before its bounded TCP RST-avoidance wait
			// finishes. Admission recovery must complete without client cleanup.
			recoveryDeadline := time.Now().Add(2 * time.Second)
			for i := 0; i < 128; i++ {
				for {
					c, err := net.DialTimeout("tcp", address, time.Second)
					if err != nil {
						t.Fatal(err)
					}
					c.SetDeadline(recoveryDeadline)
					_, err = io.WriteString(c, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\nProxy-Authorization: "+auth+"\r\n\r\n")
					if err == nil {
						var r *http.Response
						r, err = http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
						if err == nil && r.StatusCode == 200 {
							clients = append(clients, c)
							break
						}
					}
					c.Close()
					if time.Now().After(recoveryDeadline) {
						t.Fatalf("admission slot %d not reusable within cleanup bound: %v", i, err)
					}
					<-time.After(10 * time.Millisecond)
				}
			}
		})
	}
}

func TestIncompleteRejectedTLSBodiesCloseAndDoNotReachUpstream(t *testing.T) {
	for _, scenario := range []string{"wrong_host", "revoked_scope", "body_read_timeout", "oversized_chunk", "body_parse_error"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.FixtureReadTimeout = 150 * time.Millisecond
			if scenario == "oversized_chunk" {
				cfg.FixtureReadTimeout = 2 * time.Second
			}
			var sent atomic.Int32
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				sent.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			m, s := startFixture(t, cfg)
			c, _ := tunnel(t, s)
			host := "api.anthropic.com"
			if scenario == "wrong_host" {
				host = "evil.example.com"
			}
			if scenario == "revoked_scope" {
				if err := m.DeleteScope(s.ID); err != nil {
					t.Fatal(err)
				}
			}
			c.SetWriteDeadline(time.Now().Add(3 * time.Second))
			head := "POST /v1/messages HTTP/1.1\r\nHost: " + host + "\r\n"
			switch scenario {
			case "oversized_chunk":
				_, err := io.WriteString(c, head+"Transfer-Encoding: chunked\r\n\r\n"+fmt.Sprintf("%x\r\n", desktoprelay.MaxBodyBytes+2))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = io.CopyN(c, zeroReader{}, desktoprelay.MaxBodyBytes+1); err != nil {
					t.Fatal(err)
				}
			case "body_parse_error":
				if _, err := io.WriteString(c, head+"Transfer-Encoding: chunked\r\n\r\ninvalid-chunk\r\nx"); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := io.WriteString(c, head+"Content-Length: 64\r\n\r\nx"); err != nil {
					t.Fatal(err)
				}
			}
			expectBoundedEOF(t, c, 3*time.Second)
			if sent.Load() != 0 {
				t.Fatal("rejected body reached upstream")
			}
			// A clean request proves the listener and request slots remain usable.
			if scenario == "revoked_scope" {
				var err error
				s, err = m.CreateScope("replacement")
				if err != nil {
					t.Fatal(err)
				}
			}
			c2, br := tunnel(t, s)
			r := send(t, c2, br, "/v1/messages", `{}`, "", nil)
			drain(t, r)
			if r.StatusCode != 200 || sent.Load() != 1 {
				t.Fatal("request slot not reusable")
			}
		})
	}
}
