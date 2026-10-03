package desktoprelay_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestSetupRestoreAndExplicitRevokeKeepAdmittedStreamVisibleUntilFinished(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	finish := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	var streaming atomic.Bool
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if !streaming.Load() {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("observed"))}, nil
		}
		if r.Header.Get("Authorization") != "Bearer token-A" {
			t.Error("fixture stream lost selected account")
		}
		pr, pw := io.Pipe()
		go func() {
			stop := context.AfterFunc(r.Context(), func() { _ = pw.CloseWithError(r.Context().Err()) })
			defer stop()
			defer pw.Close()
			_, _ = io.WriteString(pw, "event: delta\ndata: kept\n\n")
			select {
			case <-finish:
				_, _ = io.WriteString(pw, "event: message_stop\ndata: complete\n\n")
			case <-r.Context().Done():
			}
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
	})
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	path := filepath.Join(filepath.Dir(cfg.DataRoot), "settings.json")
	status, err := m.Configure(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	env := setupEnv(t, path)
	scope := desktoprelay.ScopeSetup{Scope: desktoprelay.Scope{ID: status.ScopeID}, ProxyURL: env["HTTPS_PROXY"], CAPath: env["NODE_EXTRA_CA_CERTS"]}
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err = m.Bind(context.Background(), scope.ID, sessionA, "A", observed(t, m, scope.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	streaming.Store(true)
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	first := make([]byte, len("event: delta\ndata: kept\n\n"))
	if _, err = io.ReadFull(r.Body, first); err != nil {
		t.Fatal(err)
	}
	if observed(t, m, scope.ID, sessionA).InFlight != 1 {
		t.Fatal("fixture did not admit stream")
	}
	if _, err = m.RestoreSetup(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Listening || observed(t, m, scope.ID, sessionA).InFlight != 1 {
		t.Fatal("restore stopped listener or revoked admitted stream")
	}
	if err = m.DeleteScope(scope.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.Sessions()) != 0 {
		t.Fatal("revocation did not remove observed sessions")
	}
	statusView := m.Status()
	if statusView.InFlight != 1 || !statusView.Listening {
		t.Fatal("revocation hid an admitted request that can still be aborted by stop")
	}
	public, err := json.Marshal(statusView)
	if err != nil || !strings.Contains(string(public), `"in_flight":1`) {
		t.Fatal("runtime request count is missing from public status JSON")
	}
	once.Do(func() { close(finish) })
	if got := drain(t, r); got != "event: message_stop\ndata: complete\n\n" {
		t.Fatal("restore/revoke interrupted admitted intercepted stream")
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().InFlight != 0 {
		if time.Now().After(deadline) {
			t.Fatal("completed revoked stream remained in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !m.Status().Listening {
		t.Fatal("stream completion stopped the listener")
	}
}
