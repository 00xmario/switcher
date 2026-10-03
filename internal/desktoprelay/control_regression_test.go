package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestBindCancellationDuringFinalLockWaitCannotPersist(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "deadline"}[expire], func(t *testing.T) {
			cfg := fixtureConfig(t)
			prepared, returnPreparation, preparingReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
			var prepOnce, writeOnce sync.Once
			defer prepOnce.Do(func() { close(returnPreparation) })
			defer writeOnce.Do(func() { close(releaseWrite) })
			var holdNext atomic.Bool
			cfg.SyncDirectory = func(f *os.File) error {
				if holdNext.CompareAndSwap(true, false) {
					close(writeEntered)
					<-releaseWrite
				}
				return f.Sync()
			}
			cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
				if id == "B" {
					close(prepared)
					<-returnPreparation
					close(preparingReturned)
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			bound, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if expire {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.Bind(ctx, s.ID, sessionA, "B", bound.Revision); result <- err }()
			await(t, prepared)
			holdNext.Store(true)
			writeResult := make(chan error, 1)
			go func() { _, err := m.CreateScope("hold durable directory sync"); writeResult <- err }()
			await(t, writeEntered)
			prepOnce.Do(func() { close(returnPreparation) })
			await(t, preparingReturned)
			// The durable write owns the selection lock while the completed
			// preparer queues for final mutation. Cancel before releasing that lock.
			select {
			case err := <-result:
				t.Fatalf("binding escaped the durable-write lock: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if expire {
				await(t, ctx.Done())
			} else {
				cancel()
			}
			writeOnce.Do(func() { close(releaseWrite) })
			if err = <-writeResult; err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, ctx.Err()) {
				t.Fatalf("cancelled final-lock bind committed: %v, want %v", err, ctx.Err())
			}
			view := observed(t, m, s.ID, sessionA)
			if view.AccountID != "A" || view.Revision != bound.Revision {
				t.Fatalf("cancelled bind changed state %+v", view)
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
			view = observed(t, other, s.ID, sessionA)
			if view.AccountID != "A" || view.Revision != bound.Revision {
				t.Fatalf("cancelled bind persisted %+v", view)
			}
		})
	}
}
