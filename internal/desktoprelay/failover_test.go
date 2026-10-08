package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

// failoverSource is a credential source that knows which accounts ran out of
// usage and which account takes over.
type failoverSource struct {
	sourceFunc
	mu      sync.Mutex
	out     map[string]bool
	next    string
	moves   [][2]string
	checked []string
	uuids   map[string]string
}

// Account UUIDs as Anthropic would know them.
const (
	uuidA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	uuidB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func newFailoverSource(next string, out ...string) *failoverSource {
	s := &failoverSource{out: map[string]bool{}, next: next, uuids: map[string]string{uuidA: "A", uuidB: "B"}}
	s.sourceFunc = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		if id == "BROKEN" {
			return desktoprelay.Credential{}, errors.New("cannot prepare")
		}
		uuid := map[string]string{"A": uuidA, "B": uuidB}[id]
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id, AccountUUID: uuid}, nil
	}}
	for _, id := range out {
		s.out[id] = true
	}
	return s
}

func (s *failoverSource) AccountByUUID(uuid string) string { return s.uuids[uuid] }

func (s *failoverSource) OutOfUsage(_ context.Context, account string, status int, _ []byte, rejectedUntil time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, account)
	return status == 429 && (s.out[account] || !rejectedUntil.IsZero())
}

func (s *failoverSource) Takeover(_ context.Context, exclude string) string {
	if s.next == exclude {
		return ""
	}
	return s.next
}

func (s *failoverSource) TookOver(from, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moves = append(s.moves, [2]string{from, to})
}

// limited answers 429 for the given tokens, with Anthropic's rejected header
// when rejected is set, and echoes everything else.
func limited(rejected bool, tokens ...string) transportFunc {
	return func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		for _, token := range tokens {
			if r.Header.Get("Authorization") == "Bearer "+token {
				h := make(http.Header)
				if rejected {
					h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
					h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "4102444800")
				}
				return &http.Response{StatusCode: 429, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`))}, nil
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization") + " " + string(body)))}, nil
	}
}

func TestOutOfUsageAccountHandsTheRequestToAnother(t *testing.T) {
	cfg := fixtureConfig(t)
	source := newFailoverSource("B", "A")
	cfg.Source = source
	cfg.Transport = limited(false, "token-A")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	resp := send(t, c, br, "/v1/messages", `{"n":1}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || !strings.HasPrefix(got, "Bearer token-B ") || !strings.Contains(got, `"n":1`) {
		t.Fatalf("status %d body %q; Desktop must get B's answer, not A's refusal", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "B" || got.LastResponse == nil || got.LastResponse.AccountID != "B" {
		t.Fatalf("session %+v did not move to B", got)
	}
	if len(source.moves) != 1 || source.moves[0] != [2]string{"A", "B"} {
		t.Fatalf("moves %v", source.moves)
	}
	// Later requests stay on B without another refusal.
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-B") {
		t.Fatalf("next request %q", got)
	}
}

func TestDesktopsOwnLoginOutOfUsageMovesToASwitcherAccount(t *testing.T) {
	cfg := fixtureConfig(t)
	source := newFailoverSource("B")
	cfg.Source = source
	cfg.Transport = limited(true, "caller-token")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", `{"n":2}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 200 || !strings.HasPrefix(got, "Bearer token-B ") {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "B" {
		t.Fatalf("session %+v did not move to B", got)
	}
	if len(source.moves) != 1 || source.moves[0] != [2]string{"", "B"} {
		t.Fatalf("moves %v", source.moves)
	}
}

func TestOtherRateLimitsReachDesktopUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selected bool
		source   *failoverSource
	}{
		{"own login without Anthropic saying it is out", false, newFailoverSource("B")},
		{"a selected account with usage left", true, newFailoverSource("B")},
		{"no account has room", true, newFailoverSource("", "A")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = tc.source
			cfg.Transport = limited(false, "caller-token", "token-A")
			m, s := startFixture(t, cfg)
			c, br := tunnel(t, s)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			want := ""
			if tc.selected {
				want = "A"
				if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
					t.Fatal(err)
				}
			}
			resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
			if got := drain(t, resp); resp.StatusCode != 429 || !strings.Contains(got, "rate_limit_error") {
				t.Fatalf("status %d body %q", resp.StatusCode, got)
			}
			if got := observed(t, m, s.ID, sessionA); got.AccountID != want || len(tc.source.moves) != 0 {
				t.Fatalf("session moved: %+v moves %v", got, tc.source.moves)
			}
		})
	}
}

// A continued thread lives on the account that ran out, so Desktop is asked
// for the full conversation, which then goes to the new account.
func TestContinuedThreadAsksForTheFullConversationAfterAMove(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = newFailoverSource("B")
	cfg.Transport = limited(true, "caller-token")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", `{"thread":{"type":"continue"}}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 400 || !strings.Contains(got, "thread_unsupported_request") {
		t.Fatalf("status %d body %q", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA); got.AccountID != "B" {
		t.Fatalf("session %+v did not move to B", got)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{"messages":[]}`, sessionA, nil)); !strings.HasPrefix(got, "Bearer token-B") {
		t.Fatalf("full conversation went to %q", got)
	}
}

// A source without Failover keeps the relay's old behavior.
func TestWithoutFailoverRefusalsPassThrough(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Transport = limited(true, "caller-token")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	if drain(t, resp); resp.StatusCode != 429 || observed(t, m, s.ID, sessionA).AccountID != "" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = desktoprelay.ErrUnavailable
}

// A paired Mac's request on this host goes again on the account with the
// most room when its account ran out.
func TestHostHandsAPairedMacsRequestToAnotherAccount(t *testing.T) {
	cfg := fixtureConfig(t)
	source := newFailoverSource("B", "A")
	cfg.Source = source
	cfg.Transport = limited(false, "token-A")
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/remote/anthropic/v1/messages", strings.NewReader(body))
		w := httptest.NewRecorder()
		m.ServeAccount(w, r, "/v1/messages", "A")
		return w
	}
	if w := serve(`{"n":3}`); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "Bearer token-B ") {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	// A thread continued on the account that ran out cannot move; the paired
	// Mac is asked for the full conversation instead.
	if w := serve(`{"thread":{"type":"continue"}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "thread_unsupported_request") {
		t.Fatalf("continued thread: %d %s", w.Code, w.Body.String())
	}
}

func bodyWith(uuid string) string {
	return `{"metadata":{"user_id":"{\"session_id\":\"` + sessionA + `\",\"account_uuid\":\"` + uuid + `\"}"}}`
}

func TestATargetThatCannotBeUsedLeavesTheConversationAlone(t *testing.T) {
	cfg := fixtureConfig(t)
	source := newFailoverSource("BROKEN", "A")
	cfg.Source = source
	cfg.Transport = limited(false, "token-A")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	resp := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
	if got := drain(t, resp); resp.StatusCode != 429 || !strings.Contains(got, "rate_limit_error") {
		t.Fatalf("status %d body %q, want Anthropic's refusal", resp.StatusCode, got)
	}
	if got := observed(t, m, s.ID, sessionA).AccountID; got != "A" || len(source.moves) != 0 {
		t.Fatalf("conversation moved to %q", got)
	}
}

func TestTheResendCarriesTheNewAccountsIdentity(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = newFailoverSource("B", "A")
	cfg.Transport = limited(false, "token-A")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
	if _, err := m.Bind(context.Background(), s.ID, sessionA, "A", observed(t, m, s.ID, sessionA).Revision); err != nil {
		t.Fatal(err)
	}
	got := drain(t, send(t, c, br, "/v1/messages", bodyWith(uuidA), sessionA, nil))
	if !strings.HasPrefix(got, "Bearer token-B ") || !strings.Contains(got, uuidB) || strings.Contains(got, uuidA) {
		t.Fatalf("resend %q must carry B's account UUID", got)
	}
}

func TestDesktopsOwnLoginIsRecognizedAndNotReplacedByItself(t *testing.T) {
	cfg := fixtureConfig(t)
	source := newFailoverSource("B") // Desktop is signed in as B, the best account
	cfg.Source = source
	cfg.Transport = limited(true, "caller-token")
	m, s := startFixture(t, cfg)
	c, br := tunnel(t, s)
	resp := send(t, c, br, "/v1/messages", bodyWith(uuidB), sessionA, nil)
	if drain(t, resp); resp.StatusCode != 429 {
		t.Fatalf("status %d, want the refusal: no other account has room", resp.StatusCode)
	}
	if len(source.checked) != 1 || source.checked[0] != "B" || observed(t, m, s.ID, sessionA).AccountID != "" {
		t.Fatalf("checked %v; Desktop's login should be checked and parked as B", source.checked)
	}
}

func TestHostKeepsContinuedThreadsOnTheirAccount(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = newFailoverSource("B", "A")
	cfg.Transport = limited(false, "token-A")
	m, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	route := func(string) string { return "B" } // A ran out; B stands in
	serve := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/remote/anthropic/v1/messages", strings.NewReader(body))
		w := httptest.NewRecorder()
		m.ServeAccountRouted(w, r, "/v1/messages", "A", route)
		return w
	}
	if w := serve(`{"thread":{"type":"continue"}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "thread_unsupported_request") {
		t.Fatalf("continued thread: %d %s", w.Code, w.Body.String())
	}
	if w := serve(`{}`); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "Bearer token-B") {
		t.Fatalf("routed request: %d %s", w.Code, w.Body.String())
	}
}
