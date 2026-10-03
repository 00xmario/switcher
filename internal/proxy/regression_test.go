package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/googleauth"
	"switcher/internal/provider"
	"switcher/internal/store"
)

type regressionTransport func(*http.Request) (*http.Response, error)

func (f regressionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func regressionResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func mockForwarding(t *testing.T, transport regressionTransport) {
	t.Helper()
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = previous })
}

func mockUpstream() *httptest.Server { return &httptest.Server{URL: "https://fixture.invalid"} }

func TestTransientRefreshFailureDoesNotParkOrSwitch(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "401", true: "expired"}[expired], func(t *testing.T) {
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("X-Fake-Account") == "a" {
					return regressionResponse(401, `{}`), nil
				}
				return regressionResponse(200, "other-account"), nil
			})
			m := newManager(t, mockUpstream(), account("a"), account("b"))
			m.providers["fake"] = &healthProvider{fakeProvider: m.providers["fake"].(*fakeProvider),
				expired: func(a store.Account) bool { return expired && a.ID == "a" },
				refresh: func(context.Context, *store.Account) error { return errors.New("temporary refresh outage") }}
			if err := m.Activate("a"); err != nil {
				t.Fatal(err)
			}
			response := doRequest(t, m, "/v1/responses")
			if response.Code != http.StatusServiceUnavailable || m.ActiveID("fake") != "a" {
				t.Fatalf("transient outage switched billing: status=%d active=%s", response.Code, m.ActiveID("fake"))
			}
			if _, parked := m.Exhausted("a"); parked {
				t.Fatal("transient outage parked the account")
			}
		})
	}
}

type rejectedTokenProvider struct {
	*healthProvider
	refreshAfter401 func(context.Context, *store.Account, string) error
}

func (p *rejectedTokenProvider) RefreshAfter401(ctx context.Context, a *store.Account, rejected string) error {
	return p.refreshAfter401(ctx, a, rejected)
}

func TestRejectedTokenHookIsUsedByForwardingAndUsage(t *testing.T) {
	for _, mode := range []string{"forward", "usage"} {
		t.Run(mode, func(t *testing.T) {
			m := newManager(t, mockUpstream(), account("a"))
			p := &rejectedTokenProvider{healthProvider: &healthProvider{fakeProvider: m.providers["fake"].(*fakeProvider),
				refresh: func(context.Context, *store.Account) error {
					return errors.New("ordinary refresh must not handle a rejected token")
				},
				usage: func(_ context.Context, a store.Account) (provider.Usage, error) {
					if a.Token.AccessToken != "successor" {
						return provider.Usage{}, provider.ErrUsageAuthRequired
					}
					return goodUsage(), nil
				}}}
			p.refreshAfter401 = func(_ context.Context, a *store.Account, rejected string) error {
				if rejected != "a-token" || a.Token.AccessToken != rejected {
					return errors.New("wrong rejected generation")
				}
				a.Token.AccessToken = "successor"
				return nil
			}
			m.providers["fake"] = p
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer successor" {
					return regressionResponse(401, `{}`), nil
				}
				return regressionResponse(200, "ok"), nil
			})
			if mode == "forward" {
				if response := doRequest(t, m, "/v1/responses"); response.Code != 200 {
					t.Fatalf("rejected-token recovery status=%d body=%s", response.Code, response.Body.String())
				}
			} else if usage := m.RefreshUsage(context.Background(), account("a")); !usage.Available {
				t.Fatal("usage did not recover the rejected token")
			}
		})
	}
}

func TestRefreshOnlySelectedAccountIsMintedBeforeForwarding(t *testing.T) {
	selected := account("b")
	selected.Token = store.Token{RefreshToken: "github-fixture"}
	m := newManager(t, mockUpstream(), account("a"), selected)
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Fake-Account") != "b" || r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Errorf("wrong identity or unminted credential: %v", r.Header)
		}
		return regressionResponse(200, "ok"), nil
	})
	if response := doRequest(t, m, "/v1/responses"); response.Code != 200 || m.ActiveID("fake") != "b" {
		t.Fatalf("refresh-only selection lost: status=%d active=%s", response.Code, m.ActiveID("fake"))
	}
}

func TestFailoverCanMintARefreshOnlyAccount(t *testing.T) {
	refreshable := account("b")
	refreshable.Token = store.Token{RefreshToken: "github-fixture"}
	m := newManager(t, mockUpstream(), account("a"), refreshable, account("c"))
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Fake-Account") == "a" {
			return regressionResponse(429, usageLimit(time.Now().Add(time.Hour).Unix())), nil
		}
		if r.Header.Get("X-Fake-Account") != "b" || r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Errorf("failover skipped refresh-only identity: %v", r.Header)
		}
		return regressionResponse(200, "ok"), nil
	})
	if response := doRequest(t, m, "/v1/responses"); response.Code != 200 || m.ActiveID("fake") != "b" {
		t.Fatalf("refresh-only failover lost: status=%d active=%s", response.Code, m.ActiveID("fake"))
	}
}

func TestDelayedExhaustionCannotUndoNewerAccountDecisions(t *testing.T) {
	for _, operation := range []string{"manual switch", "relogin", "delete"} {
		t.Run(operation, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") == "Bearer a-token" {
					close(entered)
					<-release
					return regressionResponse(429, usageLimit(time.Now().Add(time.Hour).Unix())), nil
				}
				return regressionResponse(200, "ok"), nil
			})
			m := newManager(t, mockUpstream(), account("a"), account("b"), account("c"))
			if err := m.Activate("a"); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- doRequest(t, m, "/v1/responses") }()
			<-entered
			var err error
			want := "a"
			switch operation {
			case "manual switch":
				want, err = "c", m.Activate("c")
			case "relogin":
				a := account("a")
				a.Token.AccessToken = "new-login"
				err = m.ReplaceReloginAccount(&a, "a")
			case "delete":
				want, err = "b", m.DeleteAccount("a")
			}
			close(release)
			response := <-done
			if err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || m.ActiveID("fake") != want {
				t.Fatalf("late 429 undid %s: status=%d active=%s want=%s", operation, response.Code, m.ActiveID("fake"), want)
			}
			if _, parked := m.Exhausted("a"); parked {
				t.Fatal("superseded response parked the old account generation")
			}
		})
	}
}

func TestFailoverBudgetIncludesEveryAccountAndResetRetry(t *testing.T) {
	for _, useReset := range []bool{false, true} {
		t.Run(map[bool]string{false: "fifth account", true: "fourth account reset"}[useReset], func(t *testing.T) {
			accounts := []store.Account{account("a"), account("b"), account("c"), account("d")}
			if !useReset {
				accounts = append(accounts, account("e"))
			}
			m, p := newCreditManager(t, mockUpstream(), func(store.Account) bool { return useReset }, accounts...)
			p.grant("d")
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				id := r.Header.Get("X-Fake-Account")
				if id == "e" || id == "d" && p.spends() > 0 {
					return regressionResponse(200, "ok"), nil
				}
				return regressionResponse(429, usageLimit(time.Now().Add(time.Hour).Unix())), nil
			})
			response := doRequest(t, m, "/v1/responses")
			awaitResetWorker(t, m, "d")
			if response.Code != 200 {
				t.Fatalf("recoverable request failed: status=%d spends=%d", response.Code, p.spends())
			}
		})
	}
}

func awaitResetWorker(t *testing.T, m *Manager, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for event := m.LastReset(id); event != nil && event.Pending; event = m.LastReset(id) {
		if time.Now().After(deadline) {
			t.Fatal("fixture reset worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEveryAccountExhaustedPreservesUpstreamError(t *testing.T) {
	body := usageLimit(time.Now().Add(time.Hour).Unix())
	mockForwarding(t, func(*http.Request) (*http.Response, error) {
		response := regressionResponse(429, body)
		response.Header.Set("Retry-After", "3600")
		response.Header.Set("Connection", "X-Upstream-Hop")
		response.Header.Set("X-Upstream-Hop", "must-not-relay")
		return response, nil
	})
	m := newManager(t, mockUpstream(), account("a"))
	response := doRequest(t, m, "/v1/responses")
	if response.Code != 429 || response.Body.String() != body || response.Header().Get("Retry-After") != "3600" || response.Header().Get("X-Upstream-Hop") != "" {
		t.Fatalf("upstream exhaustion changed: status=%d body=%s headers=%v", response.Code, response.Body.String(), response.Header())
	}
}

type blockedCreditProvider struct {
	*fakeCreditProvider
	entered, release chan struct{}
	listed           atomic.Bool
}

func (p *blockedCreditProvider) ListResetCredits(ctx context.Context, a store.Account) ([]provider.ResetCredit, error) {
	if p.listed.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.release
	}
	return p.fakeCreditProvider.ListResetCredits(ctx, a)
}

func TestAutomaticResetRechecksFailoverBeforeRedemption(t *testing.T) {
	m, p := newCreditManager(t, mockUpstream(), func(store.Account) bool { return true }, account("a"))
	p.grant("a")
	blocked := &blockedCreditProvider{fakeCreditProvider: p, entered: make(chan struct{}), release: make(chan struct{})}
	m.providers["fake"] = blocked
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Fake-Account") == "a" && p.spends() == 0 {
			return regressionResponse(429, usageLimit(time.Now().Add(time.Hour).Unix())), nil
		}
		return regressionResponse(200, "ok"), nil
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- doRequest(t, m, "/v1/responses") }()
	<-blocked.entered
	err := m.ReplaceAccount(account("b"))
	close(blocked.release)
	response := <-done
	awaitResetWorker(t, m, "a")
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || p.spends() != 0 || m.ActiveID("fake") != "b" {
		t.Fatalf("spent instead of failing over: status=%d spends=%d active=%s", response.Code, p.spends(), m.ActiveID("fake"))
	}
}

type adoptingProvider struct{ *healthProvider }

func (*adoptingProvider) NativeEnabled() bool             { return true }
func (*adoptingProvider) NativeStatus() claudecode.Status { return claudecode.Status{Available: true} }
func (*adoptingProvider) SwitchNative(context.Context, string, func(string) error) (claudecode.SwitchResult, error) {
	return claudecode.SwitchResult{}, provider.ErrUnsupported
}
func (*adoptingProvider) UsagePollInterval() time.Duration { return 3 * time.Minute }
func (*adoptingProvider) SyncNative(_ context.Context, a *store.Account) (bool, error) {
	if a.Token.AccessToken == "native-successor" {
		return false, nil
	}
	a.Token.AccessToken, a.Token.RefreshToken = "native-successor", "native-refresh-successor"
	return true, nil
}

func TestNativeSuccessorInvalidatesRejectedHealthAndQuotaBackoff(t *testing.T) {
	m := newManager(t, nil, account("a"))
	p := &adoptingProvider{healthProvider: &healthProvider{fakeProvider: &fakeProvider{id: "fake"},
		usage: func(context.Context, store.Account) (provider.Usage, error) { return goodUsage(), nil }}}
	m.providers["fake"] = p
	m.mu.Lock()
	h := m.healthLocked("a")
	h.relogin, h.lastChecked, h.lastSuccess, h.retryAt = true, time.Now(), time.Now(), time.Now().Add(time.Hour)
	m.lastUsage["a"] = provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "old", UsedPercent: 99}}}
	m.mu.Unlock()
	usage := m.RefreshUsage(context.Background(), account("a"))
	if !usage.Available || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 37 || m.AccountHealth("a").Condition != "usage_current" {
		t.Fatalf("successor retained predecessor status: usage=%+v health=%+v", usage, m.AccountHealth("a"))
	}
}

func TestUsageFailureSuppressesOnlyRolledWindows(t *testing.T) {
	m := healthFixture(t, &healthProvider{usage: func(context.Context, store.Account) (provider.Usage, error) {
		return provider.Usage{}, provider.ErrUsageUnavailable
	}})
	m.mu.Lock()
	m.lastUsage["a"] = provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "Session", UsedPercent: 100, ResetsAt: time.Now().Add(-time.Minute).Unix()},
		{Label: "Weekly", UsedPercent: 60, ResetsAt: time.Now().Add(time.Hour).Unix()},
	}}
	m.mu.Unlock()
	usage := m.RefreshUsage(context.Background(), account("a"))
	if len(usage.Windows) != 1 || usage.Windows[0].Label != "Weekly" || usage.Windows[0].UsedPercent != 60 {
		t.Fatalf("mixed rollover retained expired quota or lost weekly quota: %+v", usage)
	}
}

func TestGoogleReloginPreservesProjectOnDiscoveryFailure(t *testing.T) {
	for _, providerID := range []string{"gemini", "antigravity"} {
		for _, replacement := range []string{"", "new-project"} {
			t.Run(providerID+"/"+replacement, func(t *testing.T) {
				old := account("a")
				old.Provider, old.CreatedAt = providerID, 100
				no := false
				old.AutoUseReset = &no
				old.Token.Extra = map[string]any{"project_id": "saved-project"}
				m := newManager(t, nil, old)
				incoming := account("new-id")
				incoming.Provider, incoming.Email, incoming.CreatedAt = providerID, old.Email, 999
				status := googleauth.ProjectDiscoveryFailed
				if replacement != "" {
					status = googleauth.ProjectDiscoveryDiscovered
				}
				incoming.Token.Extra = map[string]any{"project_id": replacement, googleauth.ProjectDiscoveryStatusKey: status}
				if err := m.ReplaceReloginAccount(&incoming, "a"); err != nil {
					t.Fatal(err)
				}
				saved, err := m.store.Get("a")
				want := replacement
				if want == "" {
					want = "saved-project"
				}
				if err != nil || saved.Token.Extra["project_id"] != want || saved.CreatedAt != 100 || saved.AutoUseReset == nil || *saved.AutoUseReset {
					t.Fatalf("relogin lost project/history/preference: project=%v created=%d error=%v", saved.Token.Extra["project_id"], saved.CreatedAt, err)
				}
			})
		}
	}
}

func TestForwardingStripsAlternateCredentialsAndPreservesFeatures(t *testing.T) {
	m := newManager(t, mockUpstream(), account("a"))
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		for _, header := range []string{"X-Api-Key", "Api-Key", "X-Goog-Api-Key", "ChatGPT-Account-Id", "Cookie", "X-Switcher-CSRF", "X-Management-Key"} {
			if r.Header.Get(header) != "" {
				t.Errorf("client credential %s leaked upstream", header)
			}
		}
		if r.Header.Get("Authorization") != "Bearer a-token" || r.Header.Get("X-Feature") != "preserve" || r.Header.Get("Anthropic-Beta") != "feature-beta" {
			t.Errorf("selected auth or feature headers lost: %v", r.Header)
		}
		return regressionResponse(200, "ok"), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/fake/v1/responses", strings.NewReader(`{}`))
	for _, header := range []string{"Authorization", "X-Api-Key", "Api-Key", "X-Goog-Api-Key", "ChatGPT-Account-Id", "Cookie", "X-Switcher-CSRF", "X-Management-Key"} {
		request.Header.Set(header, "client-secret")
	}
	request.Header.Set("X-Feature", "preserve")
	request.Header.Set("Anthropic-Beta", "feature-beta")
	response := httptest.NewRecorder()
	m.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestCredentialPreparationLockWaitRespectsCancellation(t *testing.T) {
	m := newManager(t, nil, account("a"))
	lock := m.refreshLock("a")
	lock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.PrepareAccount(ctx, "a"); done <- err }()
	select {
	case err := <-done:
		lock.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("credential preparation error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		lock.Unlock()
		<-done
		t.Fatal("credential preparation outlived its deadline while waiting for the account lock")
	}
}
