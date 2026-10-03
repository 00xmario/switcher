package login

import (
	"context"
	"errors"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/store"
)

type blockedLoginProvider struct {
	*reloginProvider
	entered chan struct{}
	release chan struct{}
}

func (p *blockedLoginProvider) LoginExchange(context.Context, string, string) (store.Account, error) {
	close(p.entered)
	<-p.release
	return p.account, nil
}

func (p *blockedLoginProvider) DeviceStart(context.Context) (provider.LoginInfo, func(context.Context) (store.Account, error), error) {
	return provider.LoginInfo{State: "device-state", Kind: "device"}, func(context.Context) (store.Account, error) {
		close(p.entered)
		<-p.release
		return p.account, nil
	}, nil
}

func TestOutcomeStaysPendingDuringBrowserExchange(t *testing.T) {
	p := &blockedLoginProvider{reloginProvider: &reloginProvider{account: store.Account{ID: "a"}}, entered: make(chan struct{}), release: make(chan struct{})}
	m := New(func(*store.Account, string) error { return nil })
	handle, err := m.Start(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Complete(context.Background(), handle.State, "code") }()
	<-p.entered
	_, pollErr, finished := m.Outcome(handle.State, time.Millisecond)
	duplicateErr := m.Complete(context.Background(), handle.State, "duplicate")
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if finished || pollErr != nil {
		t.Fatalf("in-flight exchange reported finished=%v error=%v", finished, pollErr)
	}
	if !errors.Is(duplicateErr, ErrUnknown) {
		t.Fatalf("duplicate callback error = %v", duplicateErr)
	}
	if a, err, finished := m.Outcome(handle.State, 0); !finished || err != nil || a.ID != "a" {
		t.Fatalf("completion lost: account=%s finished=%v error=%v", a.ID, finished, err)
	}
}

func TestBrowserCallbacksCannotClaimDeviceLogin(t *testing.T) {
	p := &blockedLoginProvider{reloginProvider: &reloginProvider{account: store.Account{ID: "a"}}, entered: make(chan struct{}), release: make(chan struct{})}
	m := New(func(*store.Account, string) error { return nil })
	handle, err := m.StartDevice(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	<-p.entered
	for _, state := range []string{"", handle.State} {
		if err := m.Complete(context.Background(), state, "browser-code"); !errors.Is(err, ErrUnknown) {
			t.Fatalf("device callback error = %v", err)
		}
	}
	if state := m.SinglePending(); state != "" {
		t.Fatalf("device flow was exposed as a browser callback target: %q", state)
	}
	close(p.release)
	if a, err, finished := m.Outcome(handle.State, time.Second); !finished || err != nil || a.ID != "a" {
		t.Fatalf("device completion lost: account=%s finished=%v error=%v", a.ID, finished, err)
	}
}
