package desktoprelay_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"switcher/internal/desktoprelay"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureConfig(t *testing.T) desktoprelay.Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return desktoprelay.Config{DataRoot: filepath.Join(root, "relay"), Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fixture egress disabled") })}
}

func TestLifecycleRequiresExplicitEnableAndPersistsPreference(t *testing.T) {
	cfg := fixtureConfig(t)
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if m.Status().Enabled || m.Status().Listening {
		t.Fatal("new manager enabled")
	}
	if err := m.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.DataRoot); !os.IsNotExist(err) {
		t.Fatalf("disabled construction created store: %v", err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Listening || m.Status().Validation != "fixture_tested" {
		t.Fatalf("status: %+v", m.Status())
	}
	scope, err := m.CreateScope("future desktop")
	if err != nil {
		t.Fatal(err)
	}
	if scope.ID == "" || scope.Env["HTTPS_PROXY"] != scope.ProxyURL || scope.Env["NODE_EXTRA_CA_CERTS"] != scope.CAPath {
		t.Fatalf("setup: %+v", scope)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	m2, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close(ctx)
	if err := m2.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if !m2.Status().Listening || len(m2.Scopes()) != 1 {
		t.Fatalf("resume lost preference/scope: %+v", m2.Status())
	}
	if err := m2.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m2.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if m2.Status().Enabled || m2.Status().Listening {
		t.Fatal("stop did not persist disablement")
	}
}
