package desktoprelay

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

func authority(s string) (string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil || port != "443" || host == "" || s != net.JoinHostPort(host, "443") || host != strings.ToLower(host) || strings.ContainsAny(host, "%@/\\?# \t\r\n") {
		return "", errors.New("invalid HTTPS authority")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.String() != host {
			return "", errors.New("ambiguous IP literal")
		}
		return host, nil
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return "", errors.New("invalid DNS authority")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid DNS label")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid DNS label")
			}
		}
	}
	// Numeric forms such as 127.1 and 2130706433 must never reach a resolver.
	if strings.Trim(host, "0123456789.") == "" {
		return "", errors.New("ambiguous numeric authority")
	}
	return host, nil
}

var reserved = func() []*net.IPNet {
	var out []*net.IPNet
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "192.88.99.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.175.48.0/24", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8"} {
		_, n, _ := net.ParseCIDR(s)
		out = append(out, n)
	}
	return out
}()

func publicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return false
	}
	// Evaluate IPv4-mapped addresses as IPv4, never as a second routing syntax.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	} else if len(ip) != net.IPv6len || ip[0]&0xe0 != 0x20 {
		return false
	}
	for _, n := range reserved {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

func (m *Manager) dialPublic(ctx context.Context, network, addr string) (net.Conn, error) {
	host, err := authority(addr)
	if err != nil {
		return nil, err
	}
	if m.cfg.Transport != nil && m.cfg.DialContext == nil {
		return nil, errors.New("fixture blind egress disabled")
	}
	if m.cfg.DialContext != nil && m.cfg.LookupIP == nil && net.ParseIP(host) == nil {
		return nil, errors.New("fixture DNS disabled")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else if m.cfg.LookupIP != nil {
		ips, err = m.cfg.LookupIP(ctx, host)
	} else {
		ips, err = net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	if err != nil || len(ips) == 0 || len(ips) > 64 {
		return nil, errors.New("public DNS resolution failed")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("private or reserved destination")
		}
	}
	dial := m.cfg.DialContext
	if dial == nil {
		if m.cfg.Transport != nil {
			return nil, errors.New("fixture blind egress disabled")
		}
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	// Dial the validated numeric address, never re-resolve the hostname.
	return dial(ctx, network, net.JoinHostPort(ips[0].String(), "443"))
}
