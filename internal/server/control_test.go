package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/settings"
)

func TestControlDeadlineBoundsPartialBodiesIncludingEarlyRejection(t *testing.T) {
	for _, tc := range []struct {
		path   string
		reject bool
	}{
		{"/api/auth/login", false}, {"/api/auth/login", true},
		{"/v0/management/api-call", false}, {"/v0/management/api-call", true},
		{"/unknown", true},
	} {
		t.Run(fmt.Sprintf("%s/reject=%t", tc.path, tc.reject), func(t *testing.T) {
			api := &API{Settings: settings.New(t.TempDir())}
			var handler http.Handler = http.HandlerFunc(api.handleAuthLogin)
			want := 400
			if tc.reject {
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "rejected", 403) })
				want = 403
			}
			srv := httptest.NewServer(controlRequests(handler, 50*time.Millisecond))
			defer srv.Close()
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 64\r\nContent-Type: application/json\r\n\r\n{", tc.path, srv.Listener.Addr()); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != want || time.Since(started) > time.Second {
				t.Fatalf("partial body was not bounded: %d after %s", resp.StatusCode, time.Since(started))
			}
		})
	}
}

func TestControlDeadlineDoesNotBoundInferenceStreamingBody(t *testing.T) {
	finished := make(chan error, 1)
	srv := httptest.NewServer(controlRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		finished <- err
		_, _ = w.Write(raw)
	}), 20*time.Millisecond))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /claude/v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Length: 2\r\n\r\na", srv.Listener.Addr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := io.Copy(conn, strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || string(raw) != "ab" || <-finished != nil {
		t.Fatalf("streaming request inherited control timeout: %q, %v", raw, err)
	}
}
