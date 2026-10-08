package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const phoneHelp = `Open Switcher on your phone at this Mac's Tailscale address. Only your own
Tailscale devices reach it, and only phones you approve here get in.

  switcher phone                 show the address and approved phones
  switcher phone on              turn phone access on
  switcher phone approve <code>  let in the phone that shows this code
  switcher phone deny <id>       turn a waiting phone away
  switcher phone revoke <id|name> --yes
  switcher phone off

It needs the Tailscale add-on (switcher away on), MagicDNS and HTTPS
certificates in your tailnet, and the Tailscale app on your phone.

Like every HTTPS certificate, the one Tailscale issues for the address is
listed in public certificate logs: that shows the address's name, not a way
in. Turning access off keeps approved phones, and they get back in when it is
turned on again; revoke a phone you lost.
`

type phoneStatus struct {
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	URL     string `json:"url"`
	Problem string `json:"problem"`
	Fix     string `json:"fix"`
	Waiting []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		OS       string `json:"os"`
		Approved bool   `json:"approved"`
	} `json:"waiting"`
	Devices []struct {
		ID       string    `json:"id"`
		Name     string    `json:"name"`
		OS       string    `json:"os"`
		LastSeen time.Time `json:"last_seen"`
	} `json:"devices"`
}

func runPhone(c *ctx, args []string) error {
	fs := c.flags("phone")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 0, 2)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) > 0 {
		action = pos[0]
	}
	var st phoneStatus
	if err := c.cl.call(http.MethodGet, "/api/phone", nil, &st); err != nil {
		return err
	}
	human := ""
	switch action {
	case "status", "devices":
		if len(pos) > 1 {
			return usagef("unexpected argument %q", pos[1])
		}
	case "on", "off":
		if len(pos) > 1 {
			return usagef("unexpected argument %q", pos[1])
		}
		if err := c.cl.call(http.MethodPost, "/api/phone/access", map[string]bool{"on": action == "on"}, &st); err != nil {
			return err
		}
	case "approve":
		if len(pos) < 2 {
			return usagef("missing code; it is shown on your phone")
		}
		var answer struct {
			Approved struct {
				Name string `json:"name"`
			} `json:"approved"`
		}
		if err := c.cl.call(http.MethodPost, "/api/phone/approve", map[string]string{"code": pos[1]}, &answer); err != nil {
			return err
		}
		human = fmt.Sprintf("Approved %s. It opens Switcher in a moment.", orDefault(answer.Approved.Name, "the phone"))
	case "deny":
		if len(pos) < 2 {
			return usagef("missing phone; see: switcher phone")
		}
		if err := c.cl.call(http.MethodPost, "/api/phone/deny", map[string]string{"id": pos[1]}, nil); err != nil {
			return err
		}
	case "revoke":
		if len(pos) < 2 {
			return usagef("missing phone; see: switcher phone")
		}
		id := ""
		for _, d := range st.Devices {
			if d.ID == pos[1] || strings.EqualFold(d.Name, pos[1]) {
				if id != "" {
					return fmt.Errorf("more than one phone is named %q; use its id", pos[1])
				}
				id = d.ID
			}
		}
		if id == "" {
			return fmt.Errorf("no approved phone matches %q", pos[1])
		}
		if !*yes {
			return usagef("that phone has to be approved again to use Switcher; add --yes to confirm")
		}
		if err := c.cl.call(http.MethodDelete, "/api/phone/devices/"+url.PathEscape(id), nil, nil); err != nil {
			return err
		}
	default:
		return usagef("unknown action %q; use on, off, approve, deny or revoke", action)
	}
	if action != "status" && action != "devices" && action != "on" && action != "off" {
		if err := c.cl.call(http.MethodGet, "/api/phone", nil, &st); err != nil {
			return err
		}
	}
	if c.json {
		c.emit(st)
		return nil
	}
	lines := []string{}
	if human != "" {
		lines = append(lines, human)
	}
	switch {
	case !st.Enabled:
		lines = append(lines, "Phone access: off")
		if len(st.Devices) > 0 {
			lines = append(lines, "Approved phones get back in when it is turned on; revoke a lost one with: switcher phone revoke <name> --yes")
		}
	case st.Ready:
		lines = append(lines, "Phone access: on at "+st.URL)
	default:
		lines = append(lines, "Phone access: on, not reachable yet")
	}
	if st.Problem != "" {
		next := st.Problem
		if strings.HasPrefix(st.Fix, "https://") {
			next += ": " + st.Fix
		} else if st.Fix == "sharing" {
			next += " (switcher away on)"
		}
		lines = append(lines, "Next: "+next)
	}
	for _, w := range st.Waiting {
		state := "waiting for its code"
		if w.Approved {
			state = "approved, opening"
		}
		lines = append(lines, fmt.Sprintf("  %s (%s) %s", orDefault(w.Name, "phone"), w.ID, state))
	}
	if len(st.Devices) > 0 {
		lines = append(lines, "Approved phones:")
		for _, d := range st.Devices {
			lines = append(lines, fmt.Sprintf("  %s (%s), last used %s", d.Name, d.ID, d.LastSeen.Local().Format("Jan 2 15:04")))
		}
	}
	if st.Enabled && st.Ready && len(st.Devices) == 0 && len(st.Waiting) == 0 {
		lines = append(lines, "Open the address on your phone, then: switcher phone approve <code>")
	}
	if action == "on" {
		lines = append(lines, "Note: the address's name is listed in public certificate logs, like every HTTPS certificate.")
	}
	fmt.Fprintln(c.out, strings.Join(lines, "\n"))
	return nil
}
