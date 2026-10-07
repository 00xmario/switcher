package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const shareHelp = `One Mac (the host) keeps the accounts; your other Macs use them through it.

  switcher share on        share this Mac's Switcher on your network
  switcher share code      show a pairing code, valid for ten minutes
  switcher share devices   list paired Macs
  switcher share revoke <id|name> --yes
  switcher share off

On the other Mac: switcher connect <host address> --code <code>.
`

func runShare(c *ctx, args []string) error {
	fs := c.flags("share")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 0, 2)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) > 0 {
		action = pos[0]
	}
	switch action {
	case "status", "devices":
		if len(pos) > 1 {
			return usagef("unexpected argument %q", pos[1])
		}
	case "on", "off":
		if err := c.cl.call(http.MethodPost, "/api/remote/host", map[string]bool{"enabled": action == "on"}, nil); err != nil {
			return err
		}
	case "code":
		var code struct {
			Code      string `json:"code"`
			ExpiresAt string `json:"expires_at"`
		}
		if err := c.cl.call(http.MethodPost, "/api/remote/host/pairing", nil, &code); err != nil {
			return err
		}
		r, err := c.remote()
		if err != nil {
			return err
		}
		address := ""
		if r.Host != nil && len(r.Host.LAN) > 0 {
			address = r.Host.LAN[0]
		}
		c.result(map[string]any{"code": code.Code, "expires_at": code.ExpiresAt, "host": r.Host},
			fmt.Sprintf("Pairing code: %s (valid for ten minutes)\nOn the other Mac: switcher connect %s --code %s", code.Code, orDefault(address, "<this Mac's address>"), code.Code))
		return nil
	case "revoke":
		if len(pos) < 2 {
			return usagef("missing device; see: switcher share devices")
		}
		r, err := c.remote()
		if err != nil {
			return err
		}
		if r.Host == nil {
			return errors.New("sharing is unavailable on this Mac")
		}
		id := ""
		for _, d := range r.Host.Devices {
			if d.ID == pos[1] || strings.EqualFold(d.Name, pos[1]) {
				if id != "" {
					return fmt.Errorf("more than one Mac is named %q; use its id", pos[1])
				}
				id = d.ID
			}
		}
		if id == "" {
			return fmt.Errorf("no paired Mac matches %q", pos[1])
		}
		if !*yes {
			return usagef("that Mac has to pair again to use this one; add --yes to confirm")
		}
		if err := c.cl.call(http.MethodDelete, "/api/remote/host/devices/"+url.PathEscape(id), nil, nil); err != nil {
			return err
		}
	default:
		return usagef("unknown action %q; use on, off, code, devices or revoke", action)
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	if c.json {
		c.emit(map[string]any{"host": r.Host, "client": r.Client})
		return nil
	}
	lines := []string{"Sharing: " + sharingText(r)}
	if r.Host != nil && r.Host.Enabled {
		addresses := append(append([]string{}, r.Host.LAN...), r.Host.Tailscale...)
		if len(addresses) > 0 {
			lines = append(lines, "Reachable at "+strings.Join(addresses, ", "))
		}
		for _, d := range r.Host.Devices {
			lines = append(lines, fmt.Sprintf("  %s (%s)", d.Name, d.ID))
		}
		if action == "on" {
			lines = append(lines, "Pair a Mac with: switcher share code")
		}
	}
	fmt.Fprintln(c.out, strings.Join(lines, "\n"))
	return nil
}

func runConnect(c *ctx, args []string) error {
	fs := c.flags("connect")
	code := fs.String("code", "", "")
	port := fs.Int("port", 8788, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *code == "" {
		return usagef("missing --code; show one on the other Mac with: switcher share code")
	}
	var answer struct {
		Client struct {
			HostName string `json:"host_name"`
			Address  string `json:"address"`
		} `json:"client"`
	}
	body := map[string]any{"address": pos[0], "port": *port, "code": *code}
	if err := c.cl.call(http.MethodPost, "/api/remote/connect", body, &answer); err != nil {
		return err
	}
	c.result(answer, fmt.Sprintf("Connected to %s. This Mac now uses its accounts.", answer.Client.HostName))
	return nil
}

func runDisconnect(c *ctx, args []string) error {
	fs := c.flags("disconnect")
	yes := fs.Bool("yes", false, "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	if !*yes {
		return usagef("this Mac forgets the other Switcher and needs a new pairing code to use it again; add --yes to confirm")
	}
	if err := c.cl.call(http.MethodPost, "/api/remote/disconnect", nil, nil); err != nil {
		return err
	}
	c.result(map[string]bool{"connected": false}, "Disconnected. This Mac uses its own accounts again.")
	return nil
}

func runDiscover(c *ctx, args []string) error {
	if _, err := c.parse(c.flags("discover"), args, 0, 0); err != nil {
		return err
	}
	var answer struct {
		Hosts []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
			Port    int    `json:"port"`
		} `json:"hosts"`
	}
	if err := c.cl.call(http.MethodGet, "/api/remote/discover", nil, &answer); err != nil {
		return err
	}
	if c.json {
		c.emit(answer)
		return nil
	}
	if len(answer.Hosts) == 0 {
		fmt.Fprintln(c.out, "No sharing Switcher found. Turn on sharing on the other Mac (switcher share on), or connect by its address.")
		return nil
	}
	var rows [][]string
	for _, h := range answer.Hosts {
		rows = append(rows, []string{h.Name, h.Address})
	}
	fmt.Fprintln(c.out, table(rows)+"\n\nConnect with: switcher connect <address> --code <code from the other Mac>")
	return nil
}

func runAway(c *ctx, args []string) error {
	fs := c.flags("away")
	noWait := fs.Bool("no-wait", false, "")
	yes := fs.Bool("yes", false, "")
	timeout := fs.Duration("timeout", 15*time.Minute, "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) == 1 {
		action = pos[0]
	}
	switch action {
	case "status":
	case "on":
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet", map[string]bool{"enabled": true}, nil); err != nil {
			return err
		}
	case "off":
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet", map[string]bool{"enabled": false}, nil); err != nil {
			return err
		}
	case "remove":
		if !*yes {
			return usagef("this signs this Mac out of Tailscale and deletes the add-on; add --yes to confirm")
		}
		if err := c.cl.call(http.MethodPost, "/api/remote/tailnet/remove", nil, nil); err != nil {
			return err
		}
	default:
		return usagef("unknown action %q; use on, off or remove", action)
	}
	r, err := c.remote()
	if err != nil {
		return err
	}
	if r.Tailnet == nil {
		return errors.New("the Tailscale add-on is unavailable on this Switcher")
	}
	if action == "on" && !*noWait {
		// Download, then sign-in: show the link once and wait for it.
		shown := ""
		err = wait(*timeout, time.Second, func() (bool, error) {
			if r, err = c.remote(); err != nil {
				return false, err
			}
			t := r.Tailnet
			switch {
			case t == nil || !t.Enabled:
				return false, errors.New("the add-on was turned off")
			case t.Error != "" && !t.Downloading && !t.Running:
				return false, errors.New(t.Error)
			case t.State == "Running":
				return true, nil
			case t.State == "NeedsLogin" && t.AuthURL != "" && t.AuthURL != shown:
				shown = t.AuthURL
				if c.json {
					c.emit(map[string]any{"event": "sign_in", "auth_url": t.AuthURL})
				}
				c.printf("Sign in with Tailscale (the same account on all your Macs):\n\n  %s\n\nWaiting…\n", t.AuthURL)
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	out := map[string]any{"tailnet": r.Tailnet}
	if action == "on" && !*noWait {
		out["event"] = "done"
	}
	c.result(out, "Away from home: "+awayText(r))
	return nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
