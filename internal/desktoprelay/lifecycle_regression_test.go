package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestFinishedTimedOutShutdownCanRestartWithoutSecondClose(t *testing.T) {
	for _, stop := range []bool{true, false} {
		t.Run(map[bool]string{true: "stop", false: "close"}[stop], func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var block atomic.Bool
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				if block.Swap(false) {
					close(entered)
					<-release
					return nil, r.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
			if err != nil {
				t.Fatal(err)
			}
			block.Store(true)
			r, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
			r.Header.Set("X-Claude-Code-Session-Id", sessionA)
			r.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
			if err = r.Write(c); err != nil {
				t.Fatal(err)
			}
			await(t, entered)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if stop {
				err = m.Stop(ctx)
			} else {
				err = m.Close(ctx)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("shutdown result %v", err)
			}
			if status := m.Status(); status.Listening || status.Enabled == stop {
				t.Fatalf("shutdown preference %+v", status)
			}
			if err = m.Start(context.Background()); !errors.Is(err, desktoprelay.ErrBusy) {
				t.Fatalf("still-draining runtime not fenced: %v", err)
			}
			once.Do(func() { close(release) })
			deadline := time.Now().Add(2 * time.Second)
			for {
				err = m.Start(context.Background())
				if err == nil {
					break
				}
				if !errors.Is(err, desktoprelay.ErrBusy) || time.Now().After(deadline) {
					t.Fatalf("finished runtime was not reclaimed without Close: %v", err)
				}
				<-time.After(time.Millisecond)
			}
			if status := m.Status(); !status.Enabled || !status.Listening {
				t.Fatalf("reclaimed startup %+v", status)
			}
			view := observed(t, m, s.ID, sessionA)
			if view.AccountID != "A" || view.Revision != bound.Revision || view.InFlight != 0 {
				t.Fatalf("reclamation lost binding or telemetry %+v", view)
			}
		})
	}
}
