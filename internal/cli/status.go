package cli

import (
	"fmt"
	"net/http"
	"strings"
)

// remoteStatus is GET /api/remote: sharing, the host in use and the
// Tailscale add-on.
type remoteStatus struct {
	Host *struct {
		Enabled   bool     `json:"enabled"`
		Listening bool     `json:"listening"`
		Name      string   `json:"name"`
		Port      int      `json:"port"`
		LAN       []string `json:"lan"`
		Tailscale []string `json:"tailscale"`
		Error     string   `json:"error,omitempty"`
		Pairing   *struct {
			Code      string `json:"code"`
			ExpiresAt string `json:"expires_at"`
		} `json:"pairing,omitempty"`
		Devices []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			LastSeen string `json:"last_seen,omitempty"`
		} `json:"devices"`
	} `json:"host,omitempty"`
	Client struct {
		Connected bool   `json:"connected"`
		HostName  string `json:"host_name,omitempty"`
		Address   string `json:"address,omitempty"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
	} `json:"client"`
	Tailnet *struct {
		Installed   bool   `json:"installed"`
		Enabled     bool   `json:"enabled"`
		Running     bool   `json:"running"`
		Downloading bool   `json:"downloading,omitempty"`
		State       string `json:"state,omitempty"`
		AuthURL     string `json:"auth_url,omitempty"`
		DNSName     string `json:"dns_name,omitempty"`
		IP          string `json:"ip,omitempty"`
		Error       string `json:"error,omitempty"`
	} `json:"tailnet,omitempty"`
}

func (c *ctx) remote() (remoteStatus, error) {
	var st remoteStatus
	err := c.cl.call(http.MethodGet, "/api/remote", nil, &st)
	return st, err
}

func sharingText(r remoteStatus) string {
	switch {
	case r.Client.Connected && r.Client.Reachable:
		return fmt.Sprintf("using the accounts of %s (%s)", r.Client.HostName, r.Client.Address)
	case r.Client.Connected:
		return fmt.Sprintf("using %s, which is unreachable right now", r.Client.HostName)
	case r.Host != nil && r.Host.Enabled:
		text := fmt.Sprintf("on · %d paired Mac%s", len(r.Host.Devices), plural(len(r.Host.Devices)))
		if r.Host.Pairing != nil {
			text += " · code " + r.Host.Pairing.Code
		}
		if r.Host.Error != "" {
			text += " · " + r.Host.Error
		}
		return text
	}
	return "off"
}

func awayText(r remoteStatus) string {
	t := r.Tailnet
	switch {
	case t == nil || !t.Enabled:
		return "off"
	case t.Downloading:
		return "downloading the Tailscale add-on"
	case t.Error != "":
		return t.Error
	case t.State == "Running":
		return "on Tailscale as " + t.DNSName
	case t.State == "NeedsLogin" && t.AuthURL != "":
		return "sign in with Tailscale: " + t.AuthURL
	}
	return "starting"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func runStatus(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("status"), args, 0, 0); err != nil {
		return err
	}
	st, accounts, err := c.state()
	if err != nil {
		return err
	}
	remote, remoteErr := c.remote()
	desktop, desktopErr := c.desktopStatus()
	out := map[string]any{"version": st.Version, "url": c.url, "accounts": accounts}
	if st.Update != nil && st.Update.Available {
		out["update"] = st.Update.Latest
	}
	if remoteErr == nil {
		out["sharing"] = remote
	}
	if desktopErr == nil {
		out["desktop"] = desktop
	}
	if c.json {
		c.emit(out)
		return nil
	}
	head := fmt.Sprintf("Switcher %s · %s", st.Version, c.url)
	if st.Update != nil && st.Update.Available {
		head += fmt.Sprintf(" · update %s available", st.Update.Latest)
	}
	lines := []string{head, "", accountTable(accounts), ""}
	if remoteErr == nil {
		lines = append(lines, "Sharing         "+sharingText(remote))
	}
	if desktopErr == nil {
		lines = append(lines, "Claude Desktop  "+desktop.text())
	}
	if remoteErr == nil {
		lines = append(lines, "Away from home  "+awayText(remote))
	}
	fmt.Fprintln(c.out, strings.Join(lines, "\n"))
	return nil
}
