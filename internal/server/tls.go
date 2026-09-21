package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ensureTLSCert loads or creates the self-signed certificate used by the
// LAN listener. The key persists so the certificate (and any trust decision
// the user made) stays valid across restarts.
// LANAddress is the machine's primary non-loopback IPv4 address.
func LANAddress() string { return lanAddress() }

func EnsureTLSCert(root string, lanIP string, port int) (tls.Certificate, error) {
	dir := filepath.Join(root, "tls")
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if _, err := os.Stat(certPath); err == nil {
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			// The address we are about to bind must be in the SANs: the
			// machine's LAN IP can change under DHCP while the cert
			// persists for a decade.
			if certMatchesIP(cert, lanIP) {
				return cert, nil
			}
		}
		// Corrupt, mismatched, or stale for this address: regenerate.
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	ips := []net.IP{net.ParseIP("127.0.0.1")}
	if lan := lanAddress(); lan != "" {
		if ip := net.ParseIP(lan); ip != nil {
			ips = append(ips, ip)
		}
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Switcher local"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	return pair, nil
}

// lanAddress returns the primary non-loopback IPv4 address, or "" when the
// machine has none (or the interfaces are unreachable).
// certMatchesIP reports whether the certificate covers the given IP.
func certMatchesIP(cert tls.Certificate, ip string) bool {
	if len(cert.Certificate) == 0 {
		return false
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return false
	}
	for _, candidate := range parsed.IPAddresses {
		if candidate.String() == ip {
			return true
		}
	}
	return false
}

func lanAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}
