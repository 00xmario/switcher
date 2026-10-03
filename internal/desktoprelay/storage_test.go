package desktoprelay_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestSetupUsesPrivateConstrainedCAAndNeverPersistsOAuth(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	for _, path := range []string{cfg.DataRoot, filepath.Join(cfg.DataRoot, "state.json"), s.CAPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if info.IsDir() {
			mode = 0700
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("unsafe mode %s %o", path, info.Mode().Perm())
		}
	}
	b, err := os.ReadFile(s.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatal("CA export invalid")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical || len(ca.PermittedDNSDomains) != 1 || ca.PermittedDNSDomains[0] != "api.anthropic.com" || len(ca.ExcludedDNSDomains) != 1 || ca.ExcludedDNSDomains[0] != ".api.anthropic.com" {
		t.Fatalf("CA not exact-host constrained %+v", ca)
	}
	c, br := tunnel(t, s)
	peer := c.ConnectionState().PeerCertificates[0]
	if len(peer.DNSNames) != 1 || peer.DNSNames[0] != "api.anthropic.com" || len(peer.IPAddresses) != 0 {
		t.Fatal("leaf issued extra names")
	}
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(cfg.DataRoot, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "token-A") || strings.Contains(string(b), "caller-token") {
		t.Fatal("OAuth persisted")
	}
}

func TestCorruptUnsafeOrSymlinkStoresFailClosed(t *testing.T) {
	for _, kind := range []string{"corrupt", "duplicate-state", "world-readable", "state-symlink", "ca-symlink", "root-symlink", "parent-symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg := fixtureConfig(t)
			m, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(cfg.DataRoot, "state.json")
			switch kind {
			case "duplicate-state":
				b, _ := os.ReadFile(state)
				b = bytes.Replace(b, []byte(`"enabled":true`), []byte(`"enabled":false,"enabled":true`), 1)
				os.WriteFile(state, b, 0600)
			case "corrupt":
				os.WriteFile(state, []byte(`{"version":1}`), 0600)
			case "world-readable":
				os.Chmod(state, 0644)
			case "state-symlink", "ca-symlink":
				target := state
				if kind == "ca-symlink" {
					target = filepath.Join(cfg.DataRoot, "ca.pem")
				}
				b, _ := os.ReadFile(target)
				copy := filepath.Join(filepath.Dir(cfg.DataRoot), "outside")
				os.WriteFile(copy, b, 0600)
				os.Remove(target)
				os.Symlink(copy, target)
			case "root-symlink":
				real := cfg.DataRoot + "-real"
				os.Rename(cfg.DataRoot, real)
				os.Symlink(real, cfg.DataRoot)
			case "parent-symlink":
				link := filepath.Join(filepath.Dir(cfg.DataRoot), "alias")
				os.Symlink(cfg.DataRoot, link)
				cfg.DataRoot = filepath.Join(link, "child")
			}
			other, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(context.Background())
			if err = other.Start(context.Background()); !errors.Is(err, desktoprelay.ErrUnavailable) {
				t.Fatalf("unsafe store started: %v", err)
			}
			if other.Status().Listening {
				t.Fatal("unsafe store listening")
			}
		})
	}
}

func TestRevokedCapabilityCannotAdmitNewOrExistingTunnelRequests(t *testing.T) {
	cfg := fixtureConfig(t)
	var mu sync.Mutex
	calls := 0
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if err := m.DeleteScope(s.ID); err != nil {
		t.Fatal(err)
	}
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	drain(t, r)
	if r.StatusCode != 403 {
		t.Fatal("existing revoked tunnel admitted")
	}
	_, _, r = connectRaw(t, s, "api.anthropic.com:443", "setup")
	if r.StatusCode != 407 {
		t.Fatal("deleted capability admitted")
	}
	newScope, err := m.CreateScope("rotated")
	if err != nil {
		t.Fatal(err)
	}
	oldURL, _ := url.Parse(s.ProxyURL)
	newURL, _ := url.Parse(newScope.ProxyURL)
	if oldURL.User.String() == newURL.User.String() || s.ID == newScope.ID {
		t.Fatal("capability reused")
	}
	c2, br2 := tunnel(t, newScope)
	drain(t, send(t, c2, br2, "/v1/messages", `{}`, sessionA, nil))
	if len(m.Scopes()) != 1 || len(m.Sessions()) != 1 || m.Sessions()[0].ScopeID != newScope.ID {
		t.Fatal("revocation retained old bindings")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("revoked egress %d", calls)
	}
}

func TestBindingsSurviveCloseAndResumeWithOriginalCapability(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization")))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	bound, err := m.Bind(context.Background(), s.ID, sessionA, "B", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err = other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := observed(t, other, s.ID, sessionA)
	if view.AccountID != "B" || view.Revision != bound.Revision || view.InFlight != 0 {
		t.Fatalf("restart task %+v", view)
	}
	u, _ := url.Parse(s.ProxyURL)
	u.Host = other.Status().Address
	s.ProxyURL = u.String()
	c2, br2 := tunnel(t, s)
	if b := drain(t, send(t, c2, br2, "/v1/messages", `{}`, sessionA, nil)); b != "Bearer token-B" {
		t.Fatal("restart binding lost")
	}
}

func TestConcurrentLifecycleOperationsRemainConsistent(t *testing.T) {
	cfg := fixtureConfig(t)
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			switch i % 4 {
			case 0:
				err = m.Start(context.Background())
			case 1:
				err = m.Resume(context.Background())
			case 2:
				err = m.Stop(context.Background())
			case 3:
				err = m.Close(context.Background())
			}
			if err != nil {
				t.Error(err)
			}
			m.Status()
			m.Scopes()
			m.Sessions()
		}(i)
	}
	wg.Wait()
	if err = m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().Enabled || m.Status().Listening {
		t.Fatal("final stop inconsistent")
	}
}
