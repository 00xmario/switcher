package desktoprelay

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
)

type ingressContextKey struct{}

// net/http assigns CONNECT's Request.Host from its authority and discards the
// wire Host header. Capture just that header before parsing to fence conflicts.
// TLS and pipelined bytes still travel through net/http's hijack buffer.
type ingressConn struct {
	net.Conn
	initial   []byte
	parsed    bool
	host      string
	hostCount int
	closeOnce sync.Once
	release   func()
}

func (c *ingressConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.parsed && n > 0 {
		room := maxHeaders + 4096 - len(c.initial)
		if room > n {
			room = n
		}
		c.initial = append(c.initial, p[:room]...)
		if end := bytes.Index(c.initial, []byte("\r\n\r\n")); end >= 0 {
			for _, line := range strings.Split(string(c.initial[:end]), "\r\n")[1:] {
				if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
					c.hostCount = -100
					continue
				}
				name, value, ok := strings.Cut(line, ":")
				if ok && strings.EqualFold(name, "Host") {
					c.hostCount++
					c.host = strings.TrimSpace(value)
				}
			}
			c.parsed = true
			c.initial = nil
		} else if len(c.initial) == maxHeaders+4096 {
			c.parsed = true
			c.initial = nil
		}
	}
	return n, err
}
func (c *ingressConn) Close() error { err := c.Conn.Close(); c.closeOnce.Do(c.release); return err }
func (c *ingressConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

type ingressListener struct {
	net.Listener
	slots chan struct{}
}

func (l *ingressListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &ingressConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			c.Close()
		}
	}
}
func ingressContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, ingressContextKey{}, c.(*ingressConn))
}
