package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"switcher/internal/phone"
)

// Phone access is managed only from this Mac: the phone listener has its own
// handler, and paired Macs cannot reach these routes.
func (a *API) registerPhoneRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/phone", a.handlePhoneStatus)
	mux.HandleFunc("POST /api/phone/access", a.handlePhoneAccess)
	mux.HandleFunc("POST /api/phone/approve", a.handlePhoneApprove)
	mux.HandleFunc("POST /api/phone/deny", a.handlePhoneDeny)
	mux.HandleFunc("DELETE /api/phone/devices/{id}", a.handlePhoneRevoke)
}

func (a *API) phoneControl(w http.ResponseWriter, r *http.Request) bool {
	if a.Phone == nil || a.Tailnet == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "phone access is unavailable"})
		return false
	}
	return loopbackOnly(w, r)
}

const tailnetDNSSettings = "https://login.tailscale.com/admin/dns"

func (a *API) handlePhoneStatus(w http.ResponseWriter, r *http.Request) {
	if !a.phoneControl(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, a.phoneStatus())
}

func (a *API) phoneStatus() map[string]any {
	tailnet := a.Tailnet.Status()
	enabled := a.Phone.Enabled()
	url := ""
	if tailnet.DNSName != "" {
		url = "https://" + tailnet.DNSName
	}
	// The first thing still missing, in the order the user fixes them.
	problem, fix := "", ""
	switch {
	case !tailnet.Enabled:
		problem, fix = "Turn on the Tailscale add-on under Share between Macs", "sharing"
	case !tailnet.Running:
		problem = "The Tailscale add-on is starting"
		if tailnet.Error != "" {
			problem = tailnet.Error
		}
	case tailnet.State != "Running":
		problem, fix = "Sign in to Tailscale under Share between Macs", "sharing"
	case !tailnet.HTTPS:
		problem, fix = "Turn on MagicDNS and HTTPS certificates in your tailnet's DNS settings", tailnetDNSSettings
	case enabled && !tailnet.Phone:
		problem = "Starting the phone dashboard"
		if tailnet.PhoneError != "" {
			problem = tailnet.PhoneError
		}
	}
	return map[string]any{
		"enabled": enabled,
		"ready":   enabled && tailnet.Phone,
		"url":     url,
		"tailnet": map[string]any{"enabled": tailnet.Enabled, "running": tailnet.Running, "state": tailnet.State,
			"https": tailnet.HTTPS, "phone": tailnet.Phone, "name": tailnet.Name, "tailnet": tailnet.Tailnet},
		"problem": problem,
		"fix":     fix,
		"waiting": a.Phone.Waiting(),
		"devices": a.Phone.Devices(),
	}
}

func (a *API) handlePhoneAccess(w http.ResponseWriter, r *http.Request) {
	if !a.phoneControl(w, r) {
		return
	}
	var body struct {
		On bool `json:"on"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := a.Phone.SetEnabled(body.On); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save phone access"})
		return
	}
	a.Tailnet.SyncPhone()
	writeJSON(w, http.StatusOK, a.phoneStatus())
}

func (a *API) handlePhoneApprove(w http.ResponseWriter, r *http.Request) {
	if !a.phoneControl(w, r) {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil || len(body.Code) > 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter the code shown on your phone"})
		return
	}
	approved, err := a.Phone.Approve(body.Code)
	if err != nil {
		status := http.StatusNotFound
		switch {
		case errors.Is(err, phone.ErrTooManyMiss):
			status = http.StatusTooManyRequests
		case errors.Is(err, phone.ErrOff):
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "approved": approved})
}

func (a *API) handlePhoneDeny(w http.ResponseWriter, r *http.Request) {
	if !a.phoneControl(w, r) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := a.Phone.Deny(body.ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handlePhoneRevoke(w http.ResponseWriter, r *http.Request) {
	if !a.phoneControl(w, r) {
		return
	}
	if err := a.Phone.Revoke(r.PathValue("id")); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, phone.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
