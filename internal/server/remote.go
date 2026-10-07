package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"switcher/internal/remote"
)

func (a *API) registerRemoteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/remote", a.handleRemoteStatus)
	mux.HandleFunc("POST /api/remote/host", a.handleRemoteHost)
	mux.HandleFunc("POST /api/remote/host/pairing", a.handleRemotePairing)
	mux.HandleFunc("DELETE /api/remote/host/devices/{id}", a.handleRemoteRevoke)
	mux.HandleFunc("GET /api/remote/discover", a.handleRemoteDiscover)
	mux.HandleFunc("POST /api/remote/connect", a.handleRemoteConnect)
	mux.HandleFunc("POST /api/remote/disconnect", a.handleRemoteDisconnect)
}

// remoteControl limits sharing and pairing to a browser on this Mac.
func (a *API) remoteControl(w http.ResponseWriter, r *http.Request) bool {
	if a.RemoteHost == nil || a.RemoteClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sharing is unavailable"})
		return false
	}
	return loopbackOnly(w, r)
}

func (a *API) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": a.RemoteHost.Status(), "client": a.RemoteClient.Status(), "device_name": remote.MachineName()})
}

func (a *API) handleRemoteHost(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// A Mac that uses another Switcher cannot share onward; that would loop.
	if body.Enabled && a.RemoteClient.Connected() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "disconnect from the other Switcher before sharing this one"})
		return
	}
	if err := a.RemoteHost.SetEnabled(body.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save the sharing setting"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": a.RemoteHost.Status()})
}

func (a *API) handleRemotePairing(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	code, err := a.RemoteHost.NewPairingCode()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, code)
}

func (a *API) handleRemoteRevoke(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	if err := a.RemoteHost.Revoke(r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": a.RemoteHost.Status()})
}

func (a *API) handleRemoteDiscover(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	found := []remote.Found{}
	for _, f := range remote.Discover(ctx, a.RemoteHost.Fingerprint()) {
		if !f.Self {
			found = append(found, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": found})
}

func (a *API) handleRemoteConnect(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	var body struct {
		Address string `json:"address"`
		Port    int    `json:"port"`
		Code    string `json:"code"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if a.RemoteHost.Status().Enabled {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "turn off sharing on this Mac before using another Switcher"})
		return
	}
	status, err := a.RemoteClient.Connect(r.Context(), body.Address, body.Port, body.Code, remote.MachineName())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": status})
}

func (a *API) handleRemoteDisconnect(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	if err := a.RemoteClient.Disconnect(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not forget the other Switcher"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": a.RemoteClient.Status()})
}
