package desktoprelay

import (
	"context"
	"net"
	"time"
)

// dial connects to a CONNECT destination. The listener only accepts
// authenticated loopback clients, which could reach the same hosts directly.
func (m *Manager) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if m.cfg.DialContext != nil {
		return m.cfg.DialContext(ctx, network, addr)
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}
