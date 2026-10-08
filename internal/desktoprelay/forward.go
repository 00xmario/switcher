package desktoprelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, key := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(key))
		}
	}
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}

func (m *Manager) transport() http.RoundTripper {
	if m.cfg.Transport != nil {
		return m.cfg.Transport
	}
	return &http.Transport{Proxy: nil, DialContext: m.dial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: originHost, RootCAs: m.cfg.FixtureTLSRoots}, TLSHandshakeTimeout: 10 * time.Second, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, DisableCompression: true}
}

// forward relays one decrypted api.anthropic.com request. For a session with a
// selected account it sends that account's bearer token, the OAuth beta and its
// account_uuid, exactly as Claude Code logged in to that account would. All
// other bytes pass through unchanged, and errors and rate limits are
// Anthropic's answer, not the relay's.
func (m *Manager) forward(run *runtime, scope string, w http.ResponseWriter, r *http.Request) {
	run.inFlight.Add(1)
	defer run.inFlight.Add(-1)
	if !m.scopeExists(scope) {
		relayError(w, http.StatusForbidden, "permission_error", "This Switcher relay profile was revoked", "")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	ctx := r.Context()
	message := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
	if !message && !(r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens") {
		m.send(w, r, r.URL.Path, body, "", run.transport, nil, nil)
		return
	}
	view := identity(r, body)
	task := m.route(ctx, scope, view)
	defer m.finish(task)
	// A thread continued right after a switch lives on the previous account.
	// Claude Code answers this code by resending the full conversation
	// without a thread.
	if !m.threadOnAccount(task, view.Thread) {
		relayError(w, http.StatusBadRequest, "invalid_request_error", "This message thread was started on another account; resend the full conversation.", "thread_unsupported_request")
		return
	}
	record := func(credential Credential, status int) {
		if message {
			m.recordResponse(task, credential, status)
		}
	}
	// Connected to another Switcher: that host applies its own account.
	if remote := m.remoteInference(); remote != nil {
		if resp, ok, err := remote(ctx, task.AccountID, r, body); ok {
			if err != nil {
				if ctx.Err() == nil {
					relayError(w, http.StatusBadGateway, "api_error", err.Error(), "")
				}
				return
			}
			record(Credential{AccountID: task.AccountID}, resp.StatusCode)
			stream(w, resp)
			return
		}
	}
	var limit limitFunc
	if message {
		limit = m.takeover(ctx, task, view)
	}
	m.send(w, r, r.URL.Path, body, task.AccountID, run.transport, record, limit)
}

// limitFunc decides what happens to a request Anthropic refused with 429:
// next is the account to send it to again; handled false streams the refusal
// to Desktop as it is. An empty next with handled true means the thread lives
// on the old account and Desktop must resend the full conversation.
type limitFunc func(account string, resp *http.Response, body []byte) (next string, handled bool)

// takeover moves a conversation whose account ran out of usage, Desktop's
// own login included, to the Switcher account with the most usage left. Only
// a refusal that means the account is out counts: Anthropic's limit headers
// say so for any login, and for a Switcher account its usage is checked too.
// Other rate limits pass through as Anthropic sent them.
func (m *Manager) takeover(ctx context.Context, task Session, view identityView) limitFunc {
	failover, ok := m.cfg.Source.(Failover)
	if !ok || task.SessionID == "" {
		return nil
	}
	return func(account string, resp *http.Response, body []byte) (string, bool) {
		from := account
		// Desktop's own login may be one of the Switcher accounts; then it
		// is checked, parked and passed over like a picked one.
		if from == "" && view.AccountUUID != "" {
			from = failover.AccountByUUID(view.AccountUUID)
		}
		next, ok := m.handOver(ctx, failover, from, resp, body)
		if !ok || m.moveTo(task, next, view.Thread) != nil {
			return "", false
		}
		failover.TookOver(from, next)
		log.Printf("desktop relay: a conversation's account ran out of usage; it moved to the account with the most usage left")
		if view.Thread == "continue" {
			return "", true
		}
		return next, true
	}
}

// handOver decides whether a 429 means from ran out of usage, and which
// account takes over. The new account must be ready to use: a conversation
// never moves to an account whose credential cannot be prepared.
func (m *Manager) handOver(ctx context.Context, failover Failover, from string, resp *http.Response, body []byte) (string, bool) {
	until, rejected := usageRejected(resp.Header, time.Now())
	out := rejected
	if from != "" {
		out = failover.OutOfUsage(ctx, from, resp.StatusCode, body, until)
	}
	if !out {
		return "", false
	}
	next := failover.Takeover(ctx, from)
	if next == "" || next == from {
		return "", false
	}
	if _, err := m.prepare(ctx, next); err != nil {
		return "", false
	}
	return next, true
}

// usageRejected reads Anthropic's unified limit headers: a five-hour or
// weekly window marked rejected means the login is out of usage until the
// latest of their resets.
func usageRejected(h http.Header, now time.Time) (time.Time, bool) {
	var latest time.Time
	rejected := false
	for _, window := range []string{"5h", "7d"} {
		if !strings.EqualFold(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-"+window+"-Status")), "rejected") {
			continue
		}
		rejected = true
		if seconds, err := strconv.ParseInt(strings.TrimSpace(h.Get("Anthropic-Ratelimit-Unified-"+window+"-Reset")), 10, 64); err == nil {
			if t := time.Unix(seconds, 0); t.After(now) && t.After(latest) {
				latest = t
			}
		}
	}
	if rejected && latest.IsZero() {
		latest = now.Add(time.Hour)
	}
	return latest, rejected
}

// ServeAccount relays one Anthropic API request for a paired Switcher with
// the given account's credential. path is the Anthropic path, such as
// /v1/messages.
func (m *Manager) ServeAccount(w http.ResponseWriter, r *http.Request, path, account string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	m.serveAccount(w, r, path, body, account, nil)
}

// ServeAccountRouted is ServeAccount with route choosing the account this
// host really uses, such as a stand-in for an account that ran out. A thread
// continued on the account it asked for cannot move, so the paired Mac is
// asked to resend the full conversation instead.
func (m *Manager) ServeAccountRouted(w http.ResponseWriter, r *http.Request, path, account string, route func(string) string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	m.serveAccount(w, r, path, body, account, route)
}

func (m *Manager) serveAccount(w http.ResponseWriter, r *http.Request, path string, body []byte, account string, route func(string) string) {
	m.hostOnce.Do(func() { m.hostTransport = m.transport() })
	message := path == "/v1/messages"
	thread := ""
	if message {
		thread = identity(r, body).Thread
	}
	if route != nil {
		if next := route(account); next != "" && next != account {
			if thread == "continue" {
				relayError(w, http.StatusBadRequest, "invalid_request_error", "This conversation moved to another account; resend the full conversation.", "thread_unsupported_request")
				return
			}
			account = next
		}
	}
	var limit limitFunc
	if message && account != "" {
		limit = m.hostTakeover(r.Context(), thread)
	}
	m.send(w, r, path, body, account, m.hostTransport, nil, limit)
}

// hostTakeover sends a paired Mac's request again on the account with the most
// usage left when its account ran out, or asks for the full conversation when
// it continued a thread there. The paired Mac keeps its own conversation
// picks; the route steers its next requests on this host.
func (m *Manager) hostTakeover(ctx context.Context, thread string) limitFunc {
	failover, ok := m.cfg.Source.(Failover)
	if !ok {
		return nil
	}
	return func(account string, resp *http.Response, body []byte) (string, bool) {
		next, ok := m.handOver(ctx, failover, account, resp, body)
		if !ok {
			return "", false
		}
		failover.TookOver(account, next)
		if thread == "continue" {
			return "", true
		}
		return next, true
	}
}

// send forwards one request to api.anthropic.com, with account's credential or,
// for an empty account, the caller's own.
func (m *Manager) send(w http.ResponseWriter, r *http.Request, path string, body []byte, account string, transport http.RoundTripper, record func(Credential, int), limit limitFunc) {
	ctx := r.Context()
	if record == nil {
		record = func(Credential, int) {}
	}
	original := body
	var credential Credential
	if account != "" {
		var err error
		if credential, err = m.prepare(ctx, account); err != nil {
			replyCredentialFailure(w, err)
			return
		}
		if r.Method == http.MethodPost && path == "/v1/messages" && credential.AccountUUID != "" && !strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
			body = withAccountUUID(body, credential.AccountUUID)
		}
	}
	u := &url.URL{Scheme: "https", Host: originHost, Path: path, RawQuery: r.URL.RawQuery}
	if path == r.URL.Path {
		u.RawPath = r.URL.RawPath
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		relayError(w, http.StatusBadRequest, "invalid_request_error", "Switcher could not forward this request", "")
		return
	}
	out.Header = r.Header.Clone()
	stripHop(out.Header)
	if account != "" {
		out.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		out.Header.Del("X-Api-Key")
		ensureOAuthBeta(out.Header)
	}
	resp, err := transport.RoundTrip(out)
	if err != nil {
		if ctx.Err() == nil {
			relayError(w, http.StatusBadGateway, "api_error", "Switcher could not reach api.anthropic.com", "")
		}
		return
	}
	// A rejected selected token gets one refresh and one retry, like the CLI.
	// If that fails the selected account needs attention in Switcher; passing
	// the 401 on would make Claude Code doubt its own login instead.
	if account != "" && resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		fresh, err := m.cfg.Source.RefreshRejected(ctx, account, credential.AccessToken)
		if err == nil && (!usable(fresh, account) || fresh.AccessToken == credential.AccessToken) {
			err = ErrUnavailable
		}
		if err != nil {
			record(credential, http.StatusUnauthorized)
			replyCredentialFailure(w, err)
			return
		}
		retry := out.Clone(ctx)
		retry.Body = io.NopCloser(bytes.NewReader(body))
		retry.Header.Set("Authorization", "Bearer "+fresh.AccessToken)
		if resp, err = transport.RoundTrip(retry); err != nil {
			if ctx.Err() == nil {
				relayError(w, http.StatusBadGateway, "api_error", "Switcher could not reach api.anthropic.com", "")
			}
			return
		}
		credential = fresh
	}
	// An account that ran out of usage hands the request to another one
	// before Desktop sees any of the refusal.
	if limit != nil && resp.StatusCode == http.StatusTooManyRequests {
		const most = 64 << 10
		head, _ := io.ReadAll(io.LimitReader(resp.Body, most))
		rest := resp.Body
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), rest), rest}
		if len(head) < most {
			if next, handled := limit(account, resp, head); handled {
				resp.Body.Close()
				record(credential, resp.StatusCode)
				if next == "" {
					relayError(w, http.StatusBadRequest, "invalid_request_error", "This conversation moved to another account; resend the full conversation.", "thread_unsupported_request")
					return
				}
				m.send(w, r, path, original, next, transport, record, nil)
				return
			}
		}
	}
	record(credential, resp.StatusCode)
	stream(w, resp)
}

// stream copies a response as it arrives, flushing every chunk.
func stream(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	rc.Flush()
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return
			}
			rc.Flush()
		}
		if err != nil {
			if err != io.EOF {
				// Abort the connection so a truncated stream is never mistaken
				// for a complete response.
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

func (m *Manager) prepare(ctx context.Context, account string) (Credential, error) {
	if m.cfg.Source == nil {
		return Credential{}, ErrUnavailable
	}
	c, err := m.cfg.Source.Prepare(ctx, account)
	if err == nil && !usable(c, account) {
		err = ErrUnavailable
	}
	return c, err
}

func (m *Manager) scopeExists(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.state.Scopes[id]
	return ok
}

// ensureOAuthBeta adds the OAuth beta an OAuth bearer requires, as Claude Code
// itself sends it. A caller that authenticated with an API key lacks it.
func ensureOAuthBeta(h http.Header) {
	for _, v := range h.Values("Anthropic-Beta") {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == "oauth-2025-04-20" {
				return
			}
		}
	}
	if v := h.Get("Anthropic-Beta"); v != "" {
		h.Set("Anthropic-Beta", v+",oauth-2025-04-20")
	} else {
		h.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
}

// withAccountUUID sets metadata.user_id's account_uuid to the account whose
// token is sent, which is what Claude Code sends when logged in to that
// account. All other bytes stay as they are; unexpected shapes are left alone.
func withAccountUUID(body []byte, uuid string) []byte {
	var top struct {
		Metadata struct {
			UserID json.RawMessage `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &top) != nil || len(top.Metadata.UserID) == 0 {
		return body
	}
	var inner string
	var user struct {
		AccountUUID string `json:"account_uuid"`
	}
	if json.Unmarshal(top.Metadata.UserID, &inner) != nil || json.Unmarshal([]byte(inner), &user) != nil || user.AccountUUID == "" || user.AccountUUID == uuid {
		return body
	}
	old := `"account_uuid":"` + user.AccountUUID + `"`
	if strings.Count(inner, old) != 1 {
		return body
	}
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if enc.Encode(strings.Replace(inner, old, `"account_uuid":"`+uuid+`"`, 1)) != nil {
		return body
	}
	return bytes.Replace(body, top.Metadata.UserID, bytes.TrimSuffix(encoded.Bytes(), []byte("\n")), 1)
}

func usable(c Credential, account string) bool {
	return c.AccountID == account && c.AccessToken != ""
}

func relayError(w http.ResponseWriter, status int, kind, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	e := map[string]any{"type": kind, "message": message}
	if code != "" {
		e["details"] = map[string]string{"error_code": code}
	}
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": e})
}
