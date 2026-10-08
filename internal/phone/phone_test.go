package phone

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

const (
	key    = "add-on-secret"
	origin = "https://switcher-mac.tail1.ts.net"
)

var myPhone = Identity{Node: "nPhone", Device: "marios-iphone", OS: "iOS", User: "me@example.com"}

type call struct{ method, path, remote, body string }

type harness struct {
	t      *testing.T
	access *Access
	h      *Handler
	mu     sync.Mutex
	calls  []call
	clock  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	x := &harness{t: t, clock: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	x.access = New(filepath.Join(t.TempDir(), "phone.json"))
	x.access.now = func() time.Time { return x.clock }
	if err := x.access.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		x.mu.Lock()
		x.calls = append(x.calls, call{r.Method, r.URL.Path, r.RemoteAddr, string(body)})
		x.mu.Unlock()
		if r.URL.Path == "/api/state" {
			_, _ = w.Write([]byte(`{"active":{"codex":"codex-a"},"version":"1.0.1","hub_management_key":"SECRET-KEY",
				"desktop_relay":{"on":true},"claude_code":{"x":1},"update":{},
				"accounts":[{"id":"codex-a","provider":"codex","email":"a@example.com","plan":"prolite","active":true,
				"usage":{"windows":[{"label":"Weekly","used_percent":100}]},"reset_credits":{"count":1,"next_id":"credit_1"},
				"auto_use_reset":"global","health":{"condition":"usage_current"},"quota_epoch":"e"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","outcome":"reset","account":{"email":"a@example.com"}}`))
	})
	x.h = &Handler{Access: x.access, Key: func() string { return key }, Inner: inner,
		Static: fstest.MapFS{"phone.html": {Data: []byte("<html>phone</html>")}, "phone.js": {Data: []byte("js")}},
		Mac:    func() string { return "Mac" }}
	return x
}

// do sends a request the way the add-on passes it on.
func (x *harness) do(method, path string, id Identity, cookies []*http.Cookie, header http.Header, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set(keyHeader, key)
	r.Header.Set(nodeHeader, id.Node)
	r.Header.Set(deviceHeader, id.Device)
	r.Header.Set(originHeader, origin)
	if method != http.MethodGet {
		r.Header.Set("Origin", origin)
	}
	for k, v := range header {
		r.Header.Set(k, v[0])
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c
		}
	}
	return nil
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

// pair walks a phone through approval and returns its session cookie and
// CSRF token.
func (x *harness) pair(id Identity) (*http.Cookie, string) {
	x.t.Helper()
	w := x.do(http.MethodPost, "/api/pair", id, nil, nil, "")
	if w.Code != http.StatusOK {
		x.t.Fatalf("pair: %d %s", w.Code, w.Body.String())
	}
	code := decode(x.t, w)["code"].(string)
	poll := cookie(w, pairCookie)
	if poll == nil || !poll.Secure || !poll.HttpOnly || poll.SameSite != http.SameSiteStrictMode {
		x.t.Fatalf("pairing cookie %+v", poll)
	}
	if got := decode(x.t, x.do(http.MethodGet, "/api/pair", id, []*http.Cookie{poll}, nil, ""))["status"]; got != "waiting" {
		x.t.Fatalf("before approval: %v", got)
	}
	if _, err := x.access.Approve(code[:3] + " " + code[3:]); err != nil {
		x.t.Fatal(err)
	}
	w = x.do(http.MethodGet, "/api/pair", id, []*http.Cookie{poll}, nil, "")
	answer := decode(x.t, w)
	session := cookie(w, sessionCookie)
	if answer["status"] != "approved" || session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteStrictMode {
		x.t.Fatalf("approval: %v cookie %+v", answer, session)
	}
	return session, answer["csrf"].(string)
}

func TestOnlyTheAddOnReachesThePhoneListener(t *testing.T) {
	x := newHarness(t)
	for _, tc := range []struct {
		name string
		edit func(r *http.Request)
	}{
		{"no key", func(r *http.Request) { r.Header.Del(keyHeader) }},
		{"wrong key", func(r *http.Request) { r.Header.Set(keyHeader, "add-on-secreT") }},
		{"no device", func(r *http.Request) { r.Header.Del(nodeHeader) }},
		{"no origin", func(r *http.Request) { r.Header.Del(originHeader) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set(keyHeader, key)
			r.Header.Set(nodeHeader, myPhone.Node)
			r.Header.Set(originHeader, origin)
			tc.edit(r)
			w := httptest.NewRecorder()
			x.h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", w.Code)
			}
		})
	}
	// Without a running add-on nothing gets in, not even with an empty key.
	x.h.Key = func() string { return "" }
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(nodeHeader, myPhone.Node)
	r.Header.Set(originHeader, origin)
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d with the add-on stopped", w.Code)
	}
}

func TestApprovedPhoneSeesAccountsWithoutSecrets(t *testing.T) {
	x := newHarness(t)
	session, _ := x.pair(myPhone)
	w := x.do(http.MethodGet, "/api/state", myPhone, []*http.Cookie{session}, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("state: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{"SECRET-KEY", "hub_management_key", "desktop_relay", "claude_code", "auto_use_reset", "quota_epoch"} {
		if strings.Contains(body, secret) {
			t.Fatalf("state leaks %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, `"next_id":"credit_1"`) || !strings.Contains(body, `"used_percent":100`) {
		t.Fatalf("state lacks usage or resets: %s", body)
	}
	if x.calls[0].remote != innerAddr {
		t.Fatalf("inner request from %q; it must never look like this Mac", x.calls[0].remote)
	}
}

func TestSessionWorksOnlyFromThePhoneItWasIssuedTo(t *testing.T) {
	x := newHarness(t)
	session, _ := x.pair(myPhone)
	other := Identity{Node: "nLaptop", Device: "someone-else"}
	if w := x.do(http.MethodGet, "/api/state", other, []*http.Cookie{session}, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("stolen cookie from another device: %d", w.Code)
	}
	if w := x.do(http.MethodGet, "/api/state", myPhone, nil, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no cookie: %d", w.Code)
	}
	forged := &http.Cookie{Name: sessionCookie, Value: strings.Repeat("a", 64)}
	if w := x.do(http.MethodGet, "/api/state", myPhone, []*http.Cookie{forged}, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie: %d", w.Code)
	}
}

func TestActionsNeedCSRFAndTheSameOrigin(t *testing.T) {
	x := newHarness(t)
	session, csrf := x.pair(myPhone)
	jar := []*http.Cookie{session}
	reset := `{"credit_id":"credit_1"}`
	if w := x.do(http.MethodPost, "/api/accounts/codex-a/reset", myPhone, jar, nil, reset); w.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", w.Code)
	}
	if w := x.do(http.MethodPost, "/api/accounts/codex-a/reset", myPhone, jar, http.Header{csrfHeader: {csrf}, "Origin": {"https://evil.example"}}, reset); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", w.Code)
	}
	if w := x.do(http.MethodPost, "/api/accounts/codex-a/reset", myPhone, jar, http.Header{csrfHeader: {csrf}, "Sec-Fetch-Site": {"cross-site"}}, reset); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", w.Code)
	}
	if len(x.calls) != 0 {
		t.Fatalf("refused requests reached Switcher: %v", x.calls)
	}
	w := x.do(http.MethodPost, "/api/accounts/codex-a/reset", myPhone, jar, http.Header{csrfHeader: {csrf}}, reset)
	if w.Code != http.StatusOK || decode(t, w)["outcome"] != "reset" {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "a@example.com") {
		t.Fatalf("action answer carries account details: %s", w.Body.String())
	}
	got := x.calls[0]
	if got.method != http.MethodPost || got.path != "/api/accounts/codex-a/use-reset" || got.body != reset || got.remote != innerAddr {
		t.Fatalf("forwarded %+v", got)
	}
}

func TestOnlyTheFixedActionsAreReachable(t *testing.T) {
	x := newHarness(t)
	session, csrf := x.pair(myPhone)
	jar := []*http.Cookie{session}
	csrfHeaders := http.Header{csrfHeader: {csrf}}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/auth/password"},
		{http.MethodPatch, "/api/settings"},
		{http.MethodDelete, "/api/accounts/codex-a"},
		{http.MethodPost, "/api/accounts/codex-a/activate"},
		{http.MethodPost, "/api/accounts/codex-a/use-reset"},
		{http.MethodPost, "/api/accounts/../settings/use"},
		{http.MethodPost, "/api/accounts/codex-a/use/x"},
		{http.MethodPost, "/api/login"},
		{http.MethodGet, "/api/remote"},
		{http.MethodGet, "/api/phone"},
		{http.MethodPost, "/api/phone/approve"},
		{http.MethodGet, "/v0/management/auth-files"},
		{http.MethodPost, "/codex/v1/responses"},
		{http.MethodGet, "/app.js"},
		{http.MethodGet, "/index.html"},
	} {
		if w := x.do(tc.method, tc.path, myPhone, jar, csrfHeaders, "{}"); w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if len(x.calls) != 0 {
		t.Fatalf("unlisted routes reached Switcher: %v", x.calls)
	}
	if w := x.do(http.MethodPost, "/api/accounts/codex-a/reset", myPhone, jar, csrfHeaders, `{"credit_id":"x\"}"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("odd credit id: %d", w.Code)
	}
}

func TestPhoneAccessOffClosesEverythingButThePage(t *testing.T) {
	x := newHarness(t)
	session, _ := x.pair(myPhone)
	if err := x.access.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if w := x.do(http.MethodGet, "/api/state", myPhone, []*http.Cookie{session}, nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("state while off: %d", w.Code)
	}
	if w := x.do(http.MethodPost, "/api/pair", myPhone, nil, nil, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pairing while off: %d", w.Code)
	}
	if w := x.do(http.MethodGet, "/", myPhone, nil, nil, ""); w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("page while off: %d", w.Code)
	}
}

func TestRevokedOrIdlePhonesMustPairAgain(t *testing.T) {
	x := newHarness(t)
	session, _ := x.pair(myPhone)
	devices := x.access.Devices()
	if len(devices) != 1 || devices[0].Name != "marios-iphone" {
		t.Fatalf("devices %+v", devices)
	}
	if err := x.access.Revoke(devices[0].ID); err != nil {
		t.Fatal(err)
	}
	if w := x.do(http.MethodGet, "/api/state", myPhone, []*http.Cookie{session}, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked: %d", w.Code)
	}

	x.clock = x.clock.Add(requestGap)
	session, _ = x.pair(myPhone)
	x.clock = x.clock.Add(31 * 24 * time.Hour)
	if w := x.do(http.MethodGet, "/api/state", myPhone, []*http.Cookie{session}, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("idle for 31 days: %d", w.Code)
	}
}

func TestApprovalNeedsTheCodeShownOnThePhone(t *testing.T) {
	x := newHarness(t)
	w := x.do(http.MethodPost, "/api/pair", myPhone, nil, nil, "")
	code := decode(t, w)["code"].(string)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, err := x.access.Approve(wrong); !errors.Is(err, ErrNoCode) {
		t.Fatalf("wrong code: %v", err)
	}
	for i := 0; i < maxFailures; i++ {
		_, _ = x.access.Approve(wrong)
	}
	if _, err := x.access.Approve(code); !errors.Is(err, ErrTooManyMiss) {
		t.Fatalf("after many wrong codes: %v", err)
	}
	x.clock = x.clock.Add(codeTTL + time.Second)
	if _, err := x.access.Approve(code); !errors.Is(err, ErrNoCode) {
		t.Fatalf("expired code: %v", err)
	}
	if got := decode(t, x.do(http.MethodGet, "/api/pair", myPhone, []*http.Cookie{cookie(w, pairCookie)}, nil, ""))["status"]; got != "expired" {
		t.Fatalf("poll after expiry: %v", got)
	}
}

func TestAnotherDeviceCannotCollectAnApproval(t *testing.T) {
	x := newHarness(t)
	w := x.do(http.MethodPost, "/api/pair", myPhone, nil, nil, "")
	code, poll := decode(t, w)["code"].(string), cookie(w, pairCookie)
	if _, err := x.access.Approve(code); err != nil {
		t.Fatal(err)
	}
	thief := Identity{Node: "nThief", Device: "thief"}
	w = x.do(http.MethodGet, "/api/pair", thief, []*http.Cookie{poll}, nil, "")
	if decode(t, w)["status"] == "approved" || cookie(w, sessionCookie) != nil {
		t.Fatalf("another device collected the approval: %s", w.Body.String())
	}
	if len(x.access.Devices()) != 0 {
		t.Fatal("a device was added")
	}
}

func TestWaitingPhonesAreLimited(t *testing.T) {
	x := newHarness(t)
	for i := 0; i < maxPending; i++ {
		if w := x.do(http.MethodPost, "/api/pair", Identity{Node: "n" + string(rune('a'+i))}, nil, nil, ""); w.Code != http.StatusOK {
			t.Fatalf("pair %d: %d", i, w.Code)
		}
	}
	if w := x.do(http.MethodPost, "/api/pair", Identity{Node: "nz"}, nil, nil, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("one too many: %d", w.Code)
	}
	// Asking again right away is refused; a little later it replaces the
	// phone's request.
	if w := x.do(http.MethodPost, "/api/pair", Identity{Node: "na"}, nil, nil, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("same phone right away: %d", w.Code)
	}
	x.clock = x.clock.Add(requestGap)
	if w := x.do(http.MethodPost, "/api/pair", Identity{Node: "na"}, nil, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("same phone again: %d", w.Code)
	}
	if got := len(x.access.Waiting()); got != maxPending {
		t.Fatalf("waiting %d", got)
	}
}

func TestSavedStateHoldsNoSessionAndIsPrivate(t *testing.T) {
	x := newHarness(t)
	session, _ := x.pair(myPhone)
	info, err := os.Stat(x.access.path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	body, _ := os.ReadFile(x.access.path)
	if strings.Contains(string(body), session.Value) {
		t.Fatal("the session itself is saved")
	}
	reloaded := New(x.access.path)
	if _, _, ok := reloaded.Session(myPhone, session.Value); !ok {
		t.Fatal("session lost after a restart")
	}
}

func TestTheAPIRouterAdmitsOnlyThePhoneRoutes(t *testing.T) {
	x := newHarness(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/api/accounts/codex-a"},
		{http.MethodPost, "/api/update"},
		{http.MethodPost, "/api/login"},
		{http.MethodPatch, "/api/accounts/codex-a"},
		{http.MethodPost, "/api/accounts/codex-a/recheck"},
		{http.MethodGet, "/api/remote"},
	} {
		if status, _ := x.h.call(r, tc.method, tc.path, nil); status < 400 {
			t.Errorf("%s %s: %d", tc.method, tc.path, status)
		}
	}
	if len(x.calls) != 0 {
		t.Fatalf("the router let through %v", x.calls)
	}
	if status, _ := x.h.call(r, http.MethodPost, "/api/accounts/codex-a/use-reset", []byte(`{}`)); status != http.StatusOK || len(x.calls) != 1 {
		t.Fatalf("a phone route was refused: %d", status)
	}
}
