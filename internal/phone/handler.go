package phone

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Header names the add-on sets; it drops any the browser sent itself.
const (
	keyHeader    = "X-Switcher-Phone-Key"
	nodeHeader   = "X-Switcher-Phone-Node"
	deviceHeader = "X-Switcher-Phone-Device"
	osHeader     = "X-Switcher-Phone-Os"
	userHeader   = "X-Switcher-Phone-User"
	originHeader = "X-Switcher-Phone-Origin"

	sessionCookie = "__Host-switcher_phone"
	pairCookie    = "__Host-switcher_pair"
	csrfHeader    = "X-Switcher-CSRF"
)

type approvedKey struct{}

// ApprovedPhone reports the approved phone a request to Switcher's API comes
// from. Only the phone listener sets it, once the phone's session, CSRF token
// and origin checked out; it lets such a phone switch Claude Code's login,
// which is otherwise kept to this Mac.
func ApprovedPhone(ctx context.Context) (string, bool) {
	name, ok := ctx.Value(approvedKey{}).(string)
	return name, ok
}

// innerAddr marks requests the phone listener makes to Switcher's API: they
// never count as coming from this Mac, so local-only routes refuse them.
const innerAddr = "192.0.2.3:1"

// Handler serves the phone listener. Only the running Tailscale add-on can
// reach it with the right key.
type Handler struct {
	Access *Access
	// Key returns the running add-on's secret, or "" when it is not running.
	Key func() string
	// Inner is Switcher's API as the local listener serves it, before its
	// browser gate. Only the fixed requests below are sent to it.
	Inner http.Handler
	// Static holds the web assets.
	Static fs.FS
	// Mac names this Mac on the page.
	Mac func() string

	once       sync.Once
	restricted http.Handler
}

// apiRoutes are the only requests the phone listener sends to Switcher's API.
// The router below enforces the list on its own, so a mistake in the switch
// in authenticated cannot reach anything else.
var apiRoutes = []string{
	"GET /api/state",
	"POST /api/usage/refresh",
	"POST /api/accounts/{id}/activate",
	"POST /api/accounts/{id}/refresh",
	"POST /api/accounts/{id}/use-reset",
}

func (h *Handler) api() http.Handler {
	h.once.Do(func() {
		mux := http.NewServeMux()
		for _, route := range apiRoutes {
			mux.Handle(route, h.Inner)
		}
		h.restricted = mux
	})
	return h.restricted
}

var assets = map[string]string{
	"/":            "phone.html",
	"/phone.js":    "phone.js",
	"/phone.css":   "phone.css",
	"/brand.js":    "brand.js",
	"/favicon.png": "favicon.png",
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	header.Set("Strict-Transport-Security", "max-age=31536000")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Cache-Control", "no-store")

	key := ""
	if h.Key != nil {
		key = h.Key()
	}
	got := r.Header.Get(keyHeader)
	if key == "" || len(got) != len(key) || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := Identity{Node: r.Header.Get(nodeHeader), Device: field(r, deviceHeader), OS: field(r, osHeader), User: field(r, userHeader)}
	origin := r.Header.Get(originHeader)
	if id.Node == "" || len(id.Node) > 128 || !strings.HasPrefix(origin, "https://") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site requests are not allowed", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != origin {
		http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
		return
	}

	if name, ok := assets[r.URL.Path]; ok && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		h.asset(w, r, name)
		return
	}
	if !h.Access.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": ErrOff.Error(), "enabled": false})
		return
	}
	switch {
	case r.URL.Path == "/api/session" && r.Method == http.MethodGet:
		h.session(w, r, id)
	case r.URL.Path == "/api/pair" && r.Method == http.MethodPost:
		h.pair(w, id)
	case r.URL.Path == "/api/pair" && r.Method == http.MethodGet:
		h.poll(w, r, id)
	default:
		h.authenticated(w, r, id)
	}
}

func (h *Handler) authenticated(w http.ResponseWriter, r *http.Request, id Identity) {
	cookie, _ := r.Cookie(sessionCookie)
	session := ""
	if cookie != nil {
		session = cookie.Value
	}
	device, csrf, ok := h.Access.Session(id, session)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "this phone is not approved", "paired": false})
		return
	}
	if r.Method != http.MethodGet {
		got := r.Header.Get(csrfHeader)
		if len(got) != len(csrf) || subtle.ConstantTimeCompare([]byte(got), []byte(csrf)) != 1 {
			http.Error(w, "missing csrf token", http.StatusForbidden)
			return
		}
	}
	// Requests to Switcher's API carry the approved phone in their context.
	// Only this handler sets it, after the checks above, and a context never
	// crosses the network.
	r = r.WithContext(context.WithValue(r.Context(), approvedKey{}, device.Name))
	path, method := r.URL.Path, r.Method
	switch {
	case path == "/api/state" && method == http.MethodGet:
		h.state(w, r)
	case path == "/api/usage/refresh" && method == http.MethodPost:
		h.forward(w, r, http.MethodPost, "/api/usage/refresh", nil)
	case path == "/api/logout" && method == http.MethodPost:
		_ = h.Access.Revoke(device.ID)
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		log.Printf("phone: %s signed out", device.Name)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case method == http.MethodPost && strings.HasPrefix(path, "/api/accounts/"):
		parts := strings.Split(strings.TrimPrefix(path, "/api/accounts/"), "/")
		if len(parts) != 2 || !validID(parts[0]) {
			http.NotFound(w, r)
			return
		}
		account := parts[0]
		switch parts[1] {
		case "use":
			h.forward(w, r, http.MethodPost, "/api/accounts/"+account+"/activate", nil)
		case "refresh":
			h.forward(w, r, http.MethodPost, "/api/accounts/"+account+"/refresh", nil)
		case "reset":
			var body struct {
				CreditID string `json:"credit_id"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil || !validCredit(body.CreditID) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "choose a banked reset from the refreshed account"})
				return
			}
			payload, _ := json.Marshal(map[string]string{"credit_id": body.CreditID})
			log.Printf("phone: %s used a banked reset", device.Name)
			h.forward(w, r, http.MethodPost, "/api/accounts/"+account+"/use-reset", payload)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request, id Identity) {
	out := map[string]any{"enabled": true, "paired": false, "device": id.Device}
	if h.Mac != nil {
		out["mac"] = h.Mac()
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if device, csrf, ok := h.Access.Session(id, cookie.Value); ok {
			out["paired"], out["csrf"], out["device"] = true, csrf, device.Name
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) pair(w http.ResponseWriter, id Identity) {
	code, poll, expires, err := h.Access.Request(id)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrTooMany) || errors.Is(err, ErrTooSoon) {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: pairCookie, Value: poll, Path: "/", MaxAge: int(codeTTL / time.Second),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	log.Printf("phone: %s asks for access", id.Device)
	writeJSON(w, http.StatusOK, map[string]any{"status": "waiting", "code": code, "expires_at": expires.Unix()})
}

func (h *Handler) poll(w http.ResponseWriter, r *http.Request, id Identity) {
	cookie, err := r.Cookie(pairCookie)
	if err != nil || cookie.Value == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
		return
	}
	result, err := h.Access.Poll(id, cookie.Value)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	switch result.Status {
	case "approved":
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: result.Session, Path: "/", MaxAge: int(maxSession / time.Second),
			Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.SetCookie(w, &http.Cookie{Name: pairCookie, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		log.Printf("phone: %s approved", id.Device)
		writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "csrf": result.CSRF})
	case "waiting":
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting", "code": result.Code, "expires_at": result.Expires.Unix()})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
	}
}

// state answers with a reduced copy of Switcher's state: accounts, their
// usage and banked resets. Keys, settings and Desktop details stay home.
func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	status, body := h.call(r, http.MethodGet, "/api/state", nil)
	if status != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Switcher could not read its accounts"})
		return
	}
	var full map[string]json.RawMessage
	if json.Unmarshal(body, &full) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "Switcher could not read its accounts"})
		return
	}
	out := map[string]any{}
	for _, key := range []string{"active", "order", "hidden", "version"} {
		if v, ok := full[key]; ok {
			out[key] = v
		}
	}
	var accounts []map[string]json.RawMessage
	_ = json.Unmarshal(full["accounts"], &accounts)
	kept := make([]map[string]json.RawMessage, 0, len(accounts))
	for _, account := range accounts {
		view := map[string]json.RawMessage{}
		for _, key := range []string{"id", "provider", "email", "plan", "active", "exhausted_until", "last_refresh", "usage",
			"reset_credits", "last_reset", "supports_banked_resets", "native_switch_available"} {
			if v, ok := account[key]; ok {
				view[key] = v
			}
		}
		kept = append(kept, view)
	}
	out["accounts"] = kept
	writeJSON(w, http.StatusOK, out)
}

// forward sends one fixed request to Switcher's API and answers with its
// status and a short summary.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, method, path string, body []byte) {
	status, answer := h.call(r, method, path, body)
	var parsed map[string]any
	_ = json.Unmarshal(answer, &parsed)
	out := map[string]any{}
	for _, key := range []string{"status", "error", "outcome", "active"} {
		if v, ok := parsed[key]; ok {
			out[key] = v
		}
	}
	if native, ok := parsed["native"].(map[string]any); ok {
		out["claude_code_switched"] = native["changed"] == true
	}
	if status >= 400 && out["error"] == nil {
		out["error"] = "Switcher refused that"
	}
	writeJSON(w, status, out)
}

func (h *Handler) call(r *http.Request, method, path string, body []byte) (int, []byte) {
	req, err := http.NewRequestWithContext(r.Context(), method, "http://switcher-phone"+path, bytes.NewReader(body))
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	req.RemoteAddr, req.Host = innerAddr, "switcher-phone"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	h.api().ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

func (h *Handler) asset(w http.ResponseWriter, r *http.Request, name string) {
	content, err := fs.ReadFile(h.Static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	types := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".png": "image/png"}
	for suffix, kind := range types {
		if strings.HasSuffix(name, suffix) {
			w.Header().Set("Content-Type", kind)
		}
	}
	_, _ = w.Write(content)
}

// recorder captures the inner API's answer.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) Flush()              {}
func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}
func (r *recorder) Write(p []byte) (int, error) {
	r.wrote = true
	if r.body.Len()+len(p) > 4<<20 {
		return 0, errors.New("answer too large")
	}
	return r.body.Write(p)
}

// field reads a URL-escaped identity header, keeping it short and printable.
func field(r *http.Request, name string) string {
	value, err := url.QueryUnescape(r.Header.Get(name))
	if err != nil {
		return ""
	}
	value = strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, value)
	if runes := []rune(value); len(runes) > 80 {
		value = string(runes[:80])
	}
	return strings.TrimSpace(value)
}

// validID accepts only the identifiers Switcher generates.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validCredit(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
