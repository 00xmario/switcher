package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"switcher/internal/phone"
	"switcher/internal/remote"
)

func phoneAPI(t *testing.T) (*API, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	api := &API{Phone: phone.New(filepath.Join(dir, "phone.json")), Tailnet: remote.NewTailnet(dir, "dev", 0)}
	mux := http.NewServeMux()
	api.registerPhoneRoutes(mux)
	return api, mux
}

func phoneRequest(mux http.Handler, method, path, remoteAddr string, body any) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(payload))
	r.RemoteAddr, r.Host = remoteAddr, "127.0.0.1:8787"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestPhoneAccessIsManagedOnlyFromThisMac(t *testing.T) {
	api, mux := phoneAPI(t)
	for _, remoteAddr := range []string{"192.168.1.20:5000", "192.0.2.1:1", "192.0.2.3:1", "100.101.102.103:443"} {
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{http.MethodGet, "/api/phone", nil},
			{http.MethodPost, "/api/phone/access", map[string]bool{"on": true}},
			{http.MethodPost, "/api/phone/approve", map[string]string{"code": "123456"}},
			{http.MethodPost, "/api/phone/deny", map[string]string{"id": "x"}},
			{http.MethodDelete, "/api/phone/devices/x", nil},
		} {
			if w := phoneRequest(mux, tc.method, tc.path, remoteAddr, tc.body); w.Code != http.StatusForbidden {
				t.Errorf("%s %s from %s: %d, want 403", tc.method, tc.path, remoteAddr, w.Code)
			}
		}
	}
	if api.Phone.Enabled() {
		t.Fatal("phone access was turned on from another device")
	}
}

func TestPhoneStatusExplainsWhatIsMissing(t *testing.T) {
	api, mux := phoneAPI(t)
	w := phoneRequest(mux, http.MethodPost, "/api/phone/access", "127.0.0.1:5000", map[string]bool{"on": true})
	if w.Code != http.StatusOK {
		t.Fatalf("turn on: %d %s", w.Code, w.Body.String())
	}
	var status map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if status["enabled"] != true || status["ready"] != false || status["fix"] != "sharing" {
		t.Fatalf("status %v", status)
	}
	if !api.Phone.Enabled() {
		t.Fatal("phone access is off")
	}
	if w := phoneRequest(mux, http.MethodPost, "/api/phone/approve", "127.0.0.1:5000", map[string]string{"code": "123456"}); w.Code != http.StatusNotFound {
		t.Fatalf("approve without a waiting phone: %d", w.Code)
	}
}
