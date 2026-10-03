package claude

import (
	"errors"
	"net/http"
	"os"
	"testing"
)

// Every test must explicitly supply a fixture transport. An overlooked client
// must fail locally, never send credentials or requests to a real provider.
func TestMain(m *testing.M) {
	transport := http.DefaultTransport
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		if (req.URL.Hostname() != "127.0.0.1" && req.URL.Hostname() != "::1") || req.URL.Port() == "8787" {
			return nil, errors.New("only isolated loopback fixtures are allowed in Claude tests")
		}
		return transport.RoundTrip(req)
	})
	blocked := &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("HTTP egress disabled in Claude package tests")
	})}
	http.DefaultClient = blocked
	oauthHTTPClient = blocked
	os.Exit(m.Run())
}
