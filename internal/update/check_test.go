package update

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
)

type checkResult struct {
	state State
	err   error
}

type observedContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *observedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func startFixtureCheck(c *Checker, ctx context.Context) (*observedContext, <-chan checkResult) {
	observed := &observedContext{Context: ctx, entered: make(chan struct{})}
	result := make(chan checkResult, 1)
	go func() {
		state, err := c.Check(observed)
		result <- checkResult{state, err}
	}()
	return observed, result
}

func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("fixture check did not reach expected boundary")
	}
}

func awaitCheck(t *testing.T, ch <-chan checkResult) checkResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("fixture check did not complete")
		return checkResult{}
	}
}

func TestCheckFailureKeepsLastGoodAndFreshness(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	previous := time.Now().Add(-7 * time.Hour)
	c.lastCheck = previous
	fault := errors.New("fixture metadata unavailable")
	c.metadataFetch = func(context.Context) (string, error) { return "", fault }
	state, err := c.Check(context.Background())
	if !errors.Is(err, fault) {
		t.Errorf("error=%v, want metadata failure", err)
	}
	if state != (State{Latest: "v0.5.6", Available: true}) {
		t.Errorf("last-good state=%+v", state)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastCheck.Equal(previous) {
		t.Error("failed check marked cached metadata fresh")
	}
}

func TestInstallRejectsSameOrOlderReleaseBeforeIO(t *testing.T) {
	for _, tag := range []string{"v0.5.6", "0.5.6", "v0.5.5", "v0.4.99"} {
		t.Run(tag, func(t *testing.T) {
			c := New("v0.5.6")
			c.latest = tag
			calls := 0
			ops := installOps{executable: func() (string, error) {
				calls++
				return "", errors.New("fixture I/O forbidden")
			}}
			err := c.installAndRestart(ops)
			if err == nil || !strings.Contains(err.Error(), "not newer") {
				t.Errorf("error=%v, want same/older release rejection", err)
			}
			if calls != 0 {
				t.Error("same/older release reached installation I/O")
			}
		})
	}
}

func TestCheckAlwaysRefreshesAndNormalizesValidMetadata(t *testing.T) {
	c := New("0.5.6")
	calls := 0
	c.metadataFetch = func(ctx context.Context) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Error("metadata fetch lacks a bounded deadline")
		}
		calls++
		if calls == 1 {
			return " 0.5.6\n", nil
		}
		return "v0.5.7", nil
	}
	first, err := c.Check(context.Background())
	if err != nil || first != (State{Latest: "v0.5.6"}) {
		t.Fatalf("first check=%+v error=%v", first, err)
	}
	second, err := c.Check(context.Background())
	if err != nil || second != (State{Latest: "v0.5.7", Available: true}) || calls != 2 {
		t.Errorf("fresh check=%+v error=%v calls=%d", second, err, calls)
	}
}

func TestCheckRejectsMalformedMetadataWithoutPublishing(t *testing.T) {
	for _, tag := range []string{"", "v0.5", "--flag", "v0.5.7garbage", "v0.5.7\nv0.5.8"} {
		t.Run(tag, func(t *testing.T) {
			c := New("0.5.5")
			c.latest = "v0.5.6"
			previous := time.Now().Add(-7 * time.Hour)
			c.lastCheck = previous
			c.metadataFetch = func(context.Context) (string, error) { return tag, nil }
			state, err := c.Check(context.Background())
			if err == nil || state != (State{Latest: "v0.5.6", Available: true}) {
				t.Errorf("malformed metadata: state=%+v error=%v", state, err)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.latest != "v0.5.6" || !c.lastCheck.Equal(previous) {
				t.Error("malformed metadata changed the last-good cache")
			}
		})
	}
}

func TestCheckCoalescesConcurrentSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			c := New("0.5.5")
			c.latest = "v0.5.6"
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int32
			fault := errors.New("fixture shared failure")
			c.metadataFetch = func(context.Context) (string, error) {
				calls.Add(1)
				<-release
				if fail {
					return "", fault
				}
				return "v0.5.7", nil
			}
			var results []<-chan checkResult
			for i := 0; i < 8; i++ {
				ctx, result := startFixtureCheck(c, context.Background())
				awaitSignal(t, ctx.entered)
				results = append(results, result)
			}
			unblock()
			for _, result := range results {
				got := awaitCheck(t, result)
				if fail {
					if !errors.Is(got.err, fault) || got.state.Latest != "v0.5.6" {
						t.Errorf("shared failure=%+v", got)
					}
				} else if got.err != nil || got.state != (State{Latest: "v0.5.7", Available: true}) {
					t.Errorf("shared success=%+v", got)
				}
			}
			if calls.Load() != 1 {
				t.Errorf("metadata fetches=%d, want 1", calls.Load())
			}
		})
	}
}

func TestCheckWaitsForBackgroundRefreshAndCacheReadsStayResponsive(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	release := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	c.metadataFetch = func(context.Context) (string, error) {
		close(started)
		<-release
		return "v0.5.7", nil
	}
	if state := c.State(); state != (State{Latest: "v0.5.6", Available: true}) {
		t.Fatalf("background cache=%+v", state)
	}
	awaitSignal(t, started)
	latest := make(chan string, 1)
	go func() { latest <- c.Latest() }()
	select {
	case tag := <-latest:
		if tag != "v0.5.6" {
			t.Errorf("cached latest=%q", tag)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata fetch held the cache mutex")
	}
	ctx, result := startFixtureCheck(c, context.Background())
	awaitSignal(t, ctx.entered)
	select {
	case got := <-result:
		t.Fatalf("explicit check returned before background refresh: %+v", got)
	default:
	}
	unblock()
	if got := awaitCheck(t, result); got.err != nil || got.state.Latest != "v0.5.7" {
		t.Errorf("background refresh result=%+v", got)
	}
}

func TestCheckCanceledCallerDoesNotStartFetch(t *testing.T) {
	c := New("0.5.5")
	var calls atomic.Int32
	c.metadataFetch = func(context.Context) (string, error) { calls.Add(1); return "v0.5.7", nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Check(ctx)
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Errorf("canceled check: error=%v calls=%d", err, calls.Load())
	}
}

func TestCheckWaiterCancellationDoesNotAbortSharedFetch(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	c.metadataFetch = func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "v0.5.7", nil
		}
	}
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstCtx, first := startFixtureCheck(c, caller)
	awaitSignal(t, firstCtx.entered)
	secondCtx, second := startFixtureCheck(c, context.Background())
	awaitSignal(t, secondCtx.entered)
	cancel()
	if got := awaitCheck(t, first); !errors.Is(got.err, context.Canceled) || got.state.Latest != "v0.5.6" {
		t.Errorf("canceled waiter=%+v", got)
	}
	unblock()
	if got := awaitCheck(t, second); got.err != nil || got.state.Latest != "v0.5.7" {
		t.Errorf("remaining waiter=%+v", got)
	}
}

func TestCheckHonorsCallerDeadline(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	release := make(chan struct{})
	defer close(release)
	c.metadataFetch = func(ctx context.Context) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-release:
			return "v0.5.7", nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	state, err := c.Check(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || state.Latest != "v0.5.6" || time.Since(start) > time.Second {
		t.Errorf("caller deadline: state=%+v error=%v elapsed=%v", state, err, time.Since(start))
	}
}

func TestRefreshOnceCompatibilityAndExplicitWaiterFailure(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	release := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	fault := errors.New("fixture legacy refresh failure")
	c.metadataFetch = func(context.Context) (string, error) {
		close(started)
		<-release
		return "", fault
	}
	finished := make(chan struct{})
	go func() { c.RefreshOnce(); close(finished) }()
	awaitSignal(t, started)
	skipped := make(chan struct{})
	go func() { c.RefreshOnce(); close(skipped) }()
	awaitSignal(t, skipped)
	ctx, result := startFixtureCheck(c, context.Background())
	awaitSignal(t, ctx.entered)
	unblock()
	if got := awaitCheck(t, result); !errors.Is(got.err, fault) || got.state.Latest != "v0.5.6" {
		t.Errorf("explicit waiter did not receive legacy refresh failure: %+v", got)
	}
	awaitSignal(t, finished)
}

func TestCheckTimeoutClearsFlightAndDiscardsLateResult(t *testing.T) {
	c := New("0.5.5")
	c.latest = "v0.5.6"
	previous := time.Now().Add(-7 * time.Hour)
	c.lastCheck = previous
	c.metadataTimeout = 25 * time.Millisecond
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	c.metadataFetch = func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			defer close(finished)
			<-release // Deliberately ignores cancellation to test the outer bound.
			return "v9.0.0", nil
		}
		return "v0.5.7", nil
	}
	start := time.Now()
	state, err := c.Check(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || state.Latest != "v0.5.6" || time.Since(start) > time.Second {
		t.Fatalf("bounded check: state=%+v error=%v elapsed=%v", state, err, time.Since(start))
	}
	c.mu.Lock()
	unchanged := c.lastCheck.Equal(previous)
	c.mu.Unlock()
	if !unchanged {
		t.Error("timeout marked metadata fresh")
	}
	if state, err := c.Check(context.Background()); err != nil || state.Latest != "v0.5.7" {
		t.Fatalf("retry after timeout: state=%+v error=%v", state, err)
	}
	unblock()
	awaitSignal(t, finished)
	if tag := c.Latest(); tag != "v0.5.7" {
		t.Errorf("late result overwrote newer metadata: %q", tag)
	}
}

type metadataTransport func(*http.Request) (*http.Response, error)

func (f metadataTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type chunkedVersionReader struct{ io.Reader }

func (r chunkedVersionReader) Read(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return r.Reader.Read(p)
}

func TestFallbackVersionIsFullyReadAndValidatedBeforePublication(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want string
	}{
		{"valid chunked", "0.5.7\n", 200, "v0.5.7"},
		{"empty", "", 200, ""},
		{"malformed", "v0.5.7garbage", 200, ""},
		{"multiple versions", "v0.5.7\nv0.5.8", 200, ""},
		{"oversized", "v0.5.7" + strings.Repeat(" ", 65), 200, ""},
		{"HTTP failure", "0.5.7", 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New("0.5.5")
			c.latest = "v0.5.6"
			client := &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != "https://raw.githubusercontent.com/"+Repo+"/main/VERSION" {
					t.Errorf("unexpected metadata request: %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: tc.code, Body: io.NopCloser(chunkedVersionReader{strings.NewReader(tc.body)})}, nil
			})}
			c.metadataFetch = func(ctx context.Context) (string, error) {
				return fetchReleaseMetadata(ctx, func(context.Context) ([]byte, error) {
					return nil, errors.New("fixture gh unavailable")
				}, client)
			}
			state, err := c.Check(context.Background())
			if tc.want != "" {
				if err != nil || state.Latest != tc.want {
					t.Errorf("valid fallback: state=%+v error=%v", state, err)
				}
			} else if err == nil || state.Latest != "v0.5.6" || !strings.Contains(err.Error(), "VERSION fallback") {
				t.Errorf("invalid fallback: state=%+v error=%v", state, err)
			}
		})
	}
}

func TestReleaseMetadataPrefersValidTagAndFallsBackOnMalformedTag(t *testing.T) {
	for _, tc := range []struct {
		tag       string
		want      string
		wantCalls int
	}{
		{"v0.5.7\n", "v0.5.7", 0},
		{"not-a-release", "v0.5.8", 1},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			c := New("0.5.5")
			calls := 0
			client := &http.Client{Transport: metadataTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("0.5.8\n"))}, nil
			})}
			c.metadataFetch = func(ctx context.Context) (string, error) {
				return fetchReleaseMetadata(ctx, func(context.Context) ([]byte, error) { return []byte(tc.tag), nil }, client)
			}
			state, err := c.Check(context.Background())
			if err != nil || state.Latest != tc.want || calls != tc.wantCalls {
				t.Errorf("release metadata: state=%+v error=%v fallback calls=%d", state, err, calls)
			}
		})
	}
}
