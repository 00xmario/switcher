package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

func TestStoreHasOneWriterAndCloseReleasesOwnership(t *testing.T) {
	cfg := fixtureConfig(t)
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err = other.Start(context.Background()); !errors.Is(err, desktoprelay.ErrBusy) {
		t.Fatalf("concurrent store writer: %v", err)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPostRenameFailureRetainsCommittedRevisionAndSuspendsAdmissions(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var fail atomic.Bool
	cfg.SyncDirectory = func(f *os.File) error {
		if fail.Load() {
			return errors.New("fixture directory fsync failure")
		}
		return f.Sync()
	}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	one, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err = m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatalf("durability failure hidden: %v", err)
	}
	view := observed(t, m, s.ID, sessionA)
	if view.AccountID != "B" || view.Revision != one.Revision+1 || m.Status().Listening || m.Status().Condition != "store_error" {
		t.Fatalf("uncertain commit rolled back or admitted: %+v %+v", view, m.Status())
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.SyncDirectory = nil
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err = other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	view = observed(t, other, s.ID, sessionA)
	if view.AccountID != "B" || view.Revision != one.Revision+1 {
		t.Fatalf("disk/memory disagreement %+v", view)
	}
}

func TestInterruptedStartupCanRepairCAExportFromCanonicalRecord(t *testing.T) {
	cfg := fixtureConfig(t)
	var syncs int
	cfg.SyncDirectory = func(f *os.File) error {
		syncs++
		if syncs == 2 {
			return errors.New("fixture enablement fsync failure")
		}
		return f.Sync()
	}
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(context.Background()); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatalf("failed startup %v", err)
	}
	if _, err = os.Stat(filepath.Join(cfg.DataRoot, "ca.pem")); !os.IsNotExist(err) {
		t.Fatal("CA exported before canonical enablement")
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.SyncDirectory = nil
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(context.Background())
	if err = other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !other.Status().Listening {
		t.Fatal("interrupted export prevented recovery")
	}
}
