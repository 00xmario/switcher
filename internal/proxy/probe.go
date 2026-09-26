package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProbeCodexResult describes only the Switcher-to-Codex route. It does not
// certify the native Codex CLI's configuration, login, or effective model.
type ProbeCodexResult struct {
	Outcome     string `json:"outcome"`
	Method      string `json:"method"`
	Model       string `json:"model"`
	Route       string `json:"route"`
	AccountID   string `json:"account_id,omitempty"`
	Fingerprint string `json:"-"`
}

const CodexProbeModel = "gpt-5.5"
const CodexProbeRoute = "/codex/v1/responses"
const probeMaxBytes = 1 << 20
const probeTimeout = 20 * time.Second

func (m *Manager) probeFingerprintLocked() (string, string) {
	id := m.active["codex"]
	if id == "" {
		return "", ""
	}
	parts := []string{m.probeBootID, id,
		strconv.FormatUint(m.selectionRevision["codex"], 10),
		strconv.FormatUint(m.accountRevision[id], 10)}
	sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(sum[:]), id
}

// ProbeFingerprint is a passive, process-scoped marker. After a restart the
// new boot identity makes previously saved evidence historical; changes to
// Switcher-managed account credentials and selection do so within a process.
func (m *Manager) ProbeFingerprint() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probeFingerprintLocked()
}

func directProbeClient() *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	return &http.Client{
		Transport:     transport,
		Timeout:       probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirects disabled") },
	}
}

func completedProbeStream(body io.Reader) bool {
	scanner := bufio.NewScanner(io.LimitReader(body, probeMaxBytes+1))
	scanner.Buffer(make([]byte, 4<<10), probeMaxBytes)
	var event string
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if event == "response.failed" || event == "response.incomplete" {
				return false
			}
			if event == "response.completed" {
				var complete struct {
					Type     string `json:"type"`
					Response struct {
						Status string `json:"status"`
					} `json:"response"`
				}
				return json.Unmarshal([]byte(data.String()), &complete) == nil &&
					complete.Type == "response.completed" && complete.Response.Status == "completed"
			}
			event = ""
			data.Reset()
		}
	}
	return false
}

// ProbeCodex sends at most one bounded inference request through the exact
// Codex provider mapping and auth. It never calls normal routing/failover.
func (m *Manager) ProbeCodex(ctx context.Context) ProbeCodexResult {
	result := ProbeCodexResult{Outcome: "unavailable", Method: "switcher_proxy",
		Model: CodexProbeModel, Route: CodexProbeRoute}
	if !m.probeBusy.CompareAndSwap(false, true) {
		result.Outcome = "busy"
		return result
	}
	defer m.probeBusy.Store(false)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	fingerprint, id := m.ProbeFingerprint()
	if id == "" {
		result.Outcome = "no_active_account"
		return result
	}
	lock := m.refreshLock(id)
	for !lock.TryLock() {
		select {
		case <-ctx.Done():
			result.Outcome = "timeout_or_cancelled"
			return result
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer lock.Unlock()
	m.mu.Lock()
	if current, currentID := m.probeFingerprintLocked(); current != fingerprint || currentID != id {
		m.mu.Unlock()
		result.Outcome = "selection_changed"
		return result
	}
	gen := m.generation[id]
	m.mu.Unlock()
	a, err := m.store.Get(id)
	if err != nil || a.Provider != "codex" {
		result.Outcome = "account_unavailable"
		return result
	}
	prov := m.providers["codex"]
	if prov == nil {
		result.Outcome = "unsupported_route"
		return result
	}
	if prov.IsExpired(a) {
		if err := prov.Refresh(ctx, &a); err != nil {
			m.recordRefresh(id, gen, err)
			result.Outcome = "refresh_failed"
			return result
		}
		if err := m.store.Save(a); err != nil {
			result.Outcome = "account_unavailable"
			return result
		}
		m.mu.Lock()
		m.accountRevision[id]++
		m.mu.Unlock()
		m.recordRefresh(id, gen, nil)
	}
	if a.Token.AccessToken == "" {
		result.Outcome = "refresh_required"
		return result
	}
	fingerprint, _ = m.ProbeFingerprint()
	target := prov.UpstreamURL("/v1/responses")
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "chatgpt.com" || parsed.Path != "/backend-api/codex/responses" || parsed.RawQuery != "" {
		result.Outcome = "unsupported_route"
		return result
	}
	body := []byte(`{"model":"` + CodexProbeModel + `","store":false,"stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"Reply OK."}]}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		result.Outcome = "unsupported_route"
		return result
	}
	// A Responses POST must never be replayed by the HTTP transport after a
	// refused stream or reconnect. HTTP/2 is disabled on the direct client.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if err := prov.ApplyAuth(req, a); err != nil {
		result.Outcome = "account_unavailable"
		return result
	}
	client := m.probeClient
	if client == nil {
		client = directProbeClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result.Outcome = "timeout_or_cancelled"
		} else {
			result.Outcome = "transport_error"
		}
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			result.Outcome = "upstream_unauthorized"
		case http.StatusTooManyRequests:
			result.Outcome = "quota_or_rate_limit"
		case http.StatusBadRequest, http.StatusNotFound:
			result.Outcome = "model_or_request_rejected"
		default:
			result.Outcome = "upstream_error"
		}
		return result
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") || !completedProbeStream(resp.Body) {
		if ctx.Err() != nil {
			result.Outcome = "timeout_or_cancelled"
		} else {
			result.Outcome = "incomplete_response"
		}
		return result
	}
	m.mu.Lock()
	current, currentID := m.probeFingerprintLocked()
	m.mu.Unlock()
	if current != fingerprint || currentID != id {
		result.Outcome = "selection_changed"
		return result
	}
	result.Outcome, result.AccountID, result.Fingerprint = "success", id, fingerprint
	return result
}
