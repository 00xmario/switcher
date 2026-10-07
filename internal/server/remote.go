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
	mux.HandleFunc("POST /api/remote/tailnet", a.handleTailnet)
	mux.HandleFunc("POST /api/remote/tailnet/remove", a.handleTailnetRemove)
}

// remoteControl limits sharing and pairing to a browser on this Mac.
func (a *API) remoteControl(w http.ResponseWriter, r *http.Request) bool {
	if a.RemoteClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sharing is unavailable"})
		return false
	}
	return loopbackOnly(w, r)
}

// hostControl is remoteControl for sharing this Mac, which needs the host.
func (a *API) hostControl(w http.ResponseWriter, r *http.Request) bool {
	if !a.remoteControl(w, r) {
		return false
	}
	if a.RemoteHost == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "sharing this Mac is unavailable"})
		return false
	}
	return true
}

func (a *API) hosting() bool { return a.RemoteHost != nil && a.RemoteHost.Status().Enabled }

func (a *API) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	status := map[string]any{"client": a.RemoteClient.Status(), "device_name": remote.MachineName()}
	if a.RemoteHost != nil {
		status["host"] = a.RemoteHost.Status()
	}
	if a.Tailnet != nil {
		status["tailnet"] = a.Tailnet.Status()
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *API) handleRemoteHost(w http.ResponseWriter, r *http.Request) {
	if !a.hostControl(w, r) {
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
	if a.Tailnet != nil {
		go a.Tailnet.SyncForwarding()
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": a.RemoteHost.Status()})
}

func (a *API) handleRemotePairing(w http.ResponseWriter, r *http.Request) {
	if !a.hostControl(w, r) {
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
	if !a.hostControl(w, r) {
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
	self := ""
	if a.RemoteHost != nil {
		self = a.RemoteHost.Fingerprint()
	}
	found := []remote.Found{}
	nearby := map[string]bool{}
	for _, f := range remote.Discover(ctx, self) {
		if !f.Self {
			found = append(found, f)
			nearby[remote.TailnetHostname(f.Name)] = true
		}
	}
	// Switchers in the user's tailnet, reachable from anywhere. One already
	// found nearby is listed once, by its faster local address.
	if a.Tailnet != nil {
		for _, p := range a.Tailnet.Status().Peers {
			if p.Online && p.DNSName != "" && !nearby[p.Name] {
				found = append(found, remote.Found{Name: p.Name + " (Tailscale)", Address: p.DNSName, Port: remote.DefaultPort})
			}
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
	if a.hosting() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "turn off sharing on this Mac before using another Switcher"})
		return
	}
	if a.RemoteClient.Connected() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "disconnect from the other Switcher first"})
		return
	}
	status, err := a.RemoteClient.Connect(r.Context(), body.Address, body.Port, body.Code, remote.MachineName())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": status})
}

func (a *API) handleTailnet(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	if a.Tailnet == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the Tailscale add-on is unavailable"})
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if !body.Enabled {
		a.Tailnet.Disable()
		writeJSON(w, http.StatusOK, map[string]any{"tailnet": a.Tailnet.Status()})
		return
	}
	// Downloading takes a while; the UI polls the status.
	go a.Tailnet.Enable(context.Background())
	time.Sleep(150 * time.Millisecond)
	writeJSON(w, http.StatusAccepted, map[string]any{"tailnet": a.Tailnet.Status()})
}

func (a *API) handleTailnetRemove(w http.ResponseWriter, r *http.Request) {
	if !a.remoteControl(w, r) {
		return
	}
	if a.Tailnet == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the Tailscale add-on is unavailable"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.Tailnet.Remove(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not remove the Tailscale add-on"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tailnet": a.Tailnet.Status()})
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
