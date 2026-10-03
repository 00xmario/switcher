package desktoprelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxBodyBytes = 16 << 20

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
	return &http.Transport{Proxy: nil, DialContext: m.dialPublic, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: originHost, RootCAs: m.cfg.FixtureTLSRoots}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 2 * time.Minute, MaxResponseHeaderBytes: maxHeaders, MaxIdleConns: 32, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 64, IdleConnTimeout: 90 * time.Second, DisableCompression: true, ForceAttemptHTTP2: false}
}

func (m *Manager) forward(run *runtime, scope string, w http.ResponseWriter, r *http.Request) {
	closeBody := m.guardBody(w, r)
	defer closeBody()
	select {
	case run.requests <- struct{}{}:
		defer func() { closeBody(); <-run.requests }()
	default:
		w.Header().Set("Connection", "close")
		relayError(w, 503, "api_error", "relay request limit reached", "")
		return
	}
	if !m.scopeExists(scope) {
		w.Header().Set("Connection", "close")
		http.Error(w, "scope revoked", 403)
		return
	}
	if (r.Host != originHost && r.Host != originHost+":443") || r.URL.IsAbs() || r.URL.Host != "" || !safePath(r.URL.EscapedPath()) {
		w.Header().Set("Connection", "close")
		http.Error(w, "invalid origin request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	stop := context.AfterFunc(run.ctx, cancel)
	defer stop()
	var responseSequence uint64
	message := r.Method == http.MethodPost && r.URL.Path == "/v1/messages"
	if message {
		var err error
		responseSequence, err = m.nextResponseSequence()
		if err != nil {
			relayError(w, 503, "api_error", "response observation unavailable", "")
			return
		}
	}
	rc := http.NewResponseController(w)
	// A per-write stall deadline must not leak into the next keepalive turn.
	defer rc.SetWriteDeadline(time.Time{})
	if r.ContentLength > MaxBodyBytes {
		w.Header().Set("Connection", "close")
		relayError(w, 413, "invalid_request_error", "request body exceeds relay bounds", "")
		return
	}
	rc.SetReadDeadline(time.Now().Add(m.bodyReadTimeout()))
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil || len(body) > MaxBodyBytes {
		// Preserve an expired/error deadline. On a size refusal the body is
		// incomplete, so bound its remaining drain separately from body reads.
		if err == nil {
			rc.SetReadDeadline(time.Now().Add(m.rejectDrainTimeout()))
		}
		w.Header().Set("Connection", "close")
		http.Error(w, "request body exceeds relay bounds", 413)
		return
	}
	closeBody()
	rc.SetReadDeadline(time.Time{})
	var task Session
	var credential Credential
	inference := r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens")
	if inference {
		v, e := identity(r, body)
		if e != nil {
			if errors.Is(e, errDecodedBounds) {
				relayError(w, 413, "invalid_request_error", "decoded request body exceeds relay bounds", "")
				return
			}
			relayError(w, 400, "invalid_request_error", "ambiguous or unsupported session identity", "session_identity_invalid")
			return
		}
		task, e = m.observe(ctx, scope, v)
		if e != nil {
			var association *AssociationError
			if errors.As(e, &association) {
				relayError(w, 503, "api_error", association.Error(), association.Code)
				return
			}
			relayError(w, 503, "api_error", "task observation unavailable", "")
			return
		}
		if task.AccountID != "" {
			if v.Thread {
				relayError(w, 400, "invalid_request_error", "message threads are not supported on selected-account routes", "thread_unsupported_request")
				return
			}
			if !oauthBeta(r.Header) {
				relayError(w, 400, "invalid_request_error", "selected OAuth requires the caller oauth-2025-04-20 beta", "oauth_client_incompatible")
				return
			}
			if m.cfg.Source == nil {
				replyCredentialFailure(w, nil)
				return
			}
			prepCtx, prepCancel := context.WithTimeout(ctx, 30*time.Second)
			credential, e = m.cfg.Source.Prepare(prepCtx, task.AccountID)
			prepCancel()
			if e != nil || !usable(credential, task.AccountID) {
				replyCredentialFailure(w, e)
				return
			}
		}
	}
	if err := m.revalidateAssociation(ctx, task); err != nil {
		var association *AssociationError
		code := ""
		if errors.As(err, &association) {
			code = association.Code
		}
		relayError(w, 503, "api_error", "conversation association could not be verified", code)
		return
	}
	release, e := m.admit(scope, task)
	if e != nil {
		status := 409
		if !errors.Is(e, ErrConflict) {
			status = 403
		}
		relayError(w, status, "api_error", "request admission changed", "")
		return
	}
	defer release()
	u := &url.URL{Scheme: "https", Host: originHost, Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery, ForceQuery: r.URL.ForceQuery}
	out, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	out.Host = originHost
	out.Header = r.Header.Clone()
	stripHop(out.Header)
	// No implicit transport retry, including caller idempotency-key requests.
	out.GetBody = nil
	if len(body) == 0 {
		// http.NoBody is replayable even without GetBody. An explicit empty
		// reader makes outgoingLength unknown and is not rewindable. net/http
		// still emits an empty wire body after its normal EOF probe.
		out.Body = io.NopCloser(bytes.NewReader(nil))
	}
	if task.AccountID != "" {
		if !oauthBeta(out.Header) {
			relayError(w, 400, "invalid_request_error", "selected OAuth requires an end-to-end OAuth beta", "oauth_client_incompatible")
			return
		}
		out.Header.Set("Authorization", "Bearer "+credential.AccessToken)
		out.Header.Del("X-Api-Key")
	}
	resp, err := run.transport.RoundTrip(out)
	if err != nil {
		http.Error(w, "upstream unavailable", 502)
		return
	}
	if message {
		m.recordResponse(task, credential, resp.StatusCode, responseSequence)
	}
	if task.AccountID != "" && resp.StatusCode == 401 && m.bindingCurrent(task) {
		refreshCtx, refreshCancel := context.WithTimeout(ctx, 30*time.Second)
		fresh, refreshErr := m.cfg.Source.RefreshRejected(refreshCtx, task.AccountID, credential.AccessToken)
		refreshCancel()
		if errors.Is(refreshErr, ErrNotFound) {
			resp.Body.Close()
			replyCredentialFailure(w, refreshErr)
			return
		}
		// A completed selection wins over a delayed refresh. The original
		// response remains available if retry admission is refused.
		if refreshErr == nil && usable(fresh, task.AccountID) && fresh.AccessToken != credential.AccessToken && m.revalidateAssociation(ctx, task) == nil {
			retryRelease, admitErr := m.admit(scope, task)
			if admitErr == nil {
				retry := out.Clone(ctx)
				retry.Body = io.NopCloser(bytes.NewReader(body))
				retry.Header.Set("Authorization", "Bearer "+fresh.AccessToken)
				resp.Body.Close()
				credential = fresh
				resp, err = run.transport.RoundTrip(retry)
				retryRelease()
				if err != nil {
					relayError(w, 502, "api_error", "upstream unavailable", "")
					return
				}
				if message {
					m.recordResponse(task, credential, resp.StatusCode, responseSequence)
				}
			}
		}
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, v := range resp.Header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.WriteHeader(resp.StatusCode)
	rc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	rc.Flush()
	buf := make([]byte, 32<<10)
	idle := time.AfterFunc(2*time.Minute, cancel)
	defer idle.Stop()
	for {
		n, err := resp.Body.Read(buf)
		idle.Reset(2 * time.Minute)
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(2 * time.Minute))
			if _, e := w.Write(buf[:n]); e != nil {
				return
			}
			if e := rc.Flush(); e != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

func safePath(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "%\\") || strings.Contains(path, "//") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func oauthBeta(h http.Header) bool {
	for _, v := range h.Values("Anthropic-Beta") {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) == "oauth-2025-04-20" {
				return true
			}
		}
	}
	return false
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
