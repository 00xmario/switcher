package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture synchronization timed out")
	}
}

func TestBindPreparationDoesNotHoldBindingLockOrOverwriteNewerRevision(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		if id == "B" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return desktoprelay.Credential{}, ctx.Err()
			}
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
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
	result := make(chan error, 1)
	go func() { _, err := m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); result <- err }()
	await(t, entered)
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "A" || got.Revision != one.Revision {
		t.Fatalf("intermediate account write %+v", got)
	}
	unbound, err := m.Unbind(s.ID, sessionA, one.Revision)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("stale binding result: %v", err)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "" || got.Revision != unbound.Revision {
		t.Fatalf("stale prepare won: %+v", got)
	}
}

func TestRequestPreparationRechecksBindingBeforeNetworkAdmission(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	var calls atomic.Int32
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		if id == "A" && block.Load() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return desktoprelay.Credential{}, ctx.Err()
			}
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	one, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	result := make(chan int, 1)
	go func() { r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil); drain(t, r); result <- r.StatusCode }()
	await(t, entered)
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); err != nil {
		t.Fatal(err)
	}
	close(release)
	if status := <-result; status != 409 {
		t.Fatalf("stale admission = %d", status)
	}
	if calls.Load() != 1 {
		t.Fatal("stale credential sent")
	}
}

func TestAdmittedSSEStaysOnOldAccountWhileFutureRequestsSwitch(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	tokens := make(chan string, 4)
	finish := make(chan struct{})
	var selected atomic.Bool
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		token := r.Header.Get("Authorization")
		tokens <- token
		if selected.Load() && token == "Bearer token-A" {
			pr, pw := io.Pipe()
			go func() {
				stop := context.AfterFunc(r.Context(), func() { pw.CloseWithError(r.Context().Err()) })
				defer stop()
				defer pw.Close()
				io.WriteString(pw, "event: content_block_delta\ndata: {\"signed\":\"unchanged\"}\n\n")
				select {
				case <-finish:
					io.WriteString(pw, "event: message_stop\ndata: exact-tool-result\n\n")
				case <-r.Context().Done():
				}
			}()
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("future"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	<-tokens
	one, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	selected.Store(true)
	r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	first := make([]byte, len("event: content_block_delta\ndata: {\"signed\":\"unchanged\"}\n\n"))
	if _, err := io.ReadFull(r.Body, first); err != nil {
		t.Fatal(err)
	}
	if got := observed(t, m, s.ID, sessionA); got.InFlight != 1 {
		t.Fatalf("stream not tracked %+v", got)
	}
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); err != nil {
		t.Fatal(err)
	}
	c2, br2 := tunnel(t, s)
	if b := drain(t, send(t, c2, br2, "/v1/messages", `{}`, sessionA, nil)); b != "future" {
		t.Fatal(b)
	}
	close(finish)
	if b := drain(t, r); b != "event: message_stop\ndata: exact-tool-result\n\n" {
		t.Fatalf("old stream replaced: %q", b)
	}
	if a, b := <-tokens, <-tokens; a != "Bearer token-A" || b != "Bearer token-B" {
		t.Fatalf("account snapshots %q %q", a, b)
	}
	if b := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); b != "future" {
		t.Fatal("completed streaming tunnel could not be reused")
	}
	if token := <-tokens; token != "Bearer token-B" {
		t.Fatal("reused stream kept old selection")
	}
}

func TestScopeDeletionWinsOverPendingBindPreparation(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return desktoprelay.Credential{}, ctx.Err()
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-A"}, nil
	}}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	revision := observed(t, m, s.ID, sessionA).Revision
	result := make(chan error, 1)
	go func() { _, err := m.Bind(context.Background(), s.ID, sessionA, "A", revision); result <- err }()
	await(t, entered)
	if err := m.DeleteScope(s.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, desktoprelay.ErrNotFound) || len(m.Sessions()) != 0 {
		t.Fatalf("deleted task attached: %v %v", err, m.Sessions())
	}
}

func TestOldRuntimeManagementPreparationCannotCommitAfterRestart(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var sends atomic.Int32
	cfg.Source = sourceFunc{prepare: func(ctx context.Context, id string) (desktoprelay.Credential, error) {
		close(entered)
		<-release
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-A"}, nil
	}}
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	revision := observed(t, m, s.ID, sessionA).Revision
	result := make(chan error, 1)
	go func() { _, err := m.Bind(context.Background(), s.ID, sessionA, "A", revision); result <- err }()
	await(t, entered)
	// Management preparation is not an admitted network stream. Shutdown may
	// finish, but the delayed preparer must never persist into the next run.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("old runtime preparer committed after restart")
	}
	if view := observed(t, m, s.ID, sessionA); view.AccountID != "" || view.Revision != revision || sends.Load() != 1 {
		t.Fatalf("late preparer modified task %+v", view)
	}
}

func TestShutdownDeadlineFencesRestartUntilNonCooperativeTransportExits(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if block.Load() {
			close(entered)
			<-release
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	r, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	r.Header.Set("X-Claude-Code-Session-Id", sessionA)
	r.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	if err := r.Write(c); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown deadline ignored %v", err)
	}
	if err := m.Start(context.Background()); !errors.Is(err, desktoprelay.ErrBusy) {
		t.Fatalf("overlapping runtime allowed %v", err)
	}
	close(release)
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStreamingBackpressureDoesNotBufferAnUnboundedUpstream(t *testing.T) {
	cfg := fixtureConfig(t)
	var readBytes atomic.Int64
	cancelled := make(chan struct{})
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		go func() { <-r.Context().Done(); close(cancelled) }()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&countedStream{ctx: r.Context(), bytes: &readBytes})}, nil
	})
	_, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	r := send(t, c, br, "/v1/messages", `{}`, "", nil)
	<-time.After(100 * time.Millisecond)
	c.Close()
	await(t, cancelled)
	r.Body.Close()
	if n := readBytes.Load(); n >= 32<<20 {
		t.Fatalf("stalled client allowed %d buffered bytes", n)
	}
}

type countedStream struct {
	ctx   context.Context
	bytes *atomic.Int64
}

func TestConcurrentStreamLimitRejectsWithoutAnotherUpstream(t *testing.T) {
	cfg := fixtureConfig(t)
	var sends atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		pr, pw := io.Pipe()
		go func() { <-r.Context().Done(); pw.CloseWithError(r.Context().Err()) }()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}, nil
	})
	m, s := startFixture(t, cfg)
	responses := make([]*http.Response, 0, 32)
	for i := 0; i < 32; i++ {
		c, br := tunnel(t, s)
		r := send(t, c, br, "/v1/messages", `{}`, "", nil)
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		responses = append(responses, r)
	}
	c, br := tunnel(t, s)
	r := send(t, c, br, "/v1/messages", `{}`, "", nil)
	drain(t, r)
	if r.StatusCode != 503 || sends.Load() != 32 {
		t.Fatalf("stream cap bypassed %d %d", r.StatusCode, sends.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range responses {
		r.Body.Close()
	}
}

func (s *countedStream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	for i := range p {
		p[i] = 'x'
	}
	s.bytes.Add(int64(len(p)))
	return len(p), nil
}

func TestSelectionDuring401RefreshPreventsRetryWithStaleRevision(t *testing.T) {
	cfg := fixtureConfig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	src := fixtureSource().(sourceFunc)
	src.refresh = func(ctx context.Context, id, old string) (desktoprelay.Credential, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return desktoprelay.Credential{}, ctx.Err()
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "new-A"}, nil
	}
	cfg.Source = src
	var selected atomic.Bool
	var calls atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		status := 200
		if selected.Load() {
			status = 401
		}
		return &http.Response{StatusCode: status, Header: http.Header{"X-Rejection": []string{"original"}}, Body: io.NopCloser(strings.NewReader("original response"))}, nil
	})
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	one, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision)
	if err != nil {
		t.Fatal(err)
	}
	selected.Store(true)
	result := make(chan int, 1)
	go func() {
		r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
		if b := drain(t, r); b != "original response" || r.Header.Get("X-Rejection") != "original" {
			t.Errorf("rejection replaced %q", b)
		}
		result <- r.StatusCode
	}()
	await(t, entered)
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "B", one.Revision); err != nil {
		t.Fatal(err)
	}
	close(release)
	if status := <-result; status != 401 || calls.Load() != 2 {
		t.Fatalf("stale refresh replay: status %d calls %d", status, calls.Load())
	}
}

func TestClientDisconnectAndShutdownCancelOwnedUpstreamRequests(t *testing.T) {
	for _, action := range []string{"disconnect", "stop"} {
		t.Run(action, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cancelled := make(chan struct{})
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				pr, pw := io.Pipe()
				go func() { <-r.Context().Done(); pw.CloseWithError(r.Context().Err()); close(cancelled) }()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: pr}, nil
			})
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
			if action == "disconnect" {
				c.Close()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := m.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			await(t, cancelled)
			r.Body.Close()
		})
	}
}
