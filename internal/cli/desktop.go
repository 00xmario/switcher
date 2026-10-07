package cli

import (
	"fmt"
	"net/http"
)

// desktop is the Claude Desktop relay as the command line reports it.
type desktop struct {
	Connected bool   `json:"connected"`
	Listening bool   `json:"listening"`
	InFlight  int    `json:"in_flight"`
	Condition string `json:"condition,omitempty"`
	Settings  string `json:"settings_path,omitempty"`
	Restart   bool   `json:"restart_required,omitempty"`
}

func (d desktop) text() string {
	switch {
	case d.Connected && d.Listening:
		text := "connected"
		if d.InFlight > 0 {
			text += fmt.Sprintf(" · %d request%s in flight", d.InFlight, plural(d.InFlight))
		}
		if d.Restart {
			text += " · restart Claude Desktop to finish"
		}
		return text
	case d.Connected:
		return "connected, but the relay is stopped"
	case d.Condition == "changed":
		return "settings changed outside Switcher; run: switcher desktop connect"
	}
	return "not connected"
}

func (c *ctx) desktopStatus() (desktop, error) {
	header, err := c.cl.desktopHeader()
	if err != nil {
		return desktop{}, err
	}
	var answer struct {
		Status struct {
			Listening bool `json:"listening"`
			InFlight  int  `json:"in_flight"`
		} `json:"status"`
		Setup struct {
			Configured bool   `json:"configured"`
			Condition  string `json:"condition"`
			Path       string `json:"settings_path"`
			Restart    bool   `json:"restart_required"`
		} `json:"setup"`
	}
	if err := c.cl.callWith(http.MethodGet, "/api/desktop-relay", nil, &answer, header); err != nil {
		return desktop{}, err
	}
	return desktop{Connected: answer.Setup.Configured, Listening: answer.Status.Listening, InFlight: answer.Status.InFlight,
		Condition: answer.Setup.Condition, Settings: answer.Setup.Path, Restart: answer.Setup.Restart}, nil
}

func runDesktop(c *ctx, args []string) error {
	fs := c.flags("desktop")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	action := "status"
	if len(pos) == 1 {
		action = pos[0]
	}
	path, done := "", ""
	var body any = struct{}{}
	switch action {
	case "status":
		d, err := c.desktopStatus()
		if err != nil {
			return err
		}
		c.result(d, "Claude Desktop: "+d.text())
		return nil
	case "connect":
		path, done = "/api/desktop-relay/configure", "Connected. Restart Claude Desktop when your current work is done: switcher desktop restart --yes"
	case "disconnect":
		path, done = "/api/desktop-relay/restore", "Disconnected. Restart Claude Desktop when your current work is done: switcher desktop restart --yes"
	case "restart":
		if !*yes {
			return usagef("this quits and reopens Claude Desktop, interrupting its running work; add --yes to confirm")
		}
		path, done, body = "/api/desktop-relay/restart-desktop", "Claude Desktop is restarting.", map[string]bool{"confirmed": true}
	default:
		return usagef("unknown action %q; use status, connect, disconnect or restart", action)
	}
	header, err := c.cl.desktopHeader()
	if err != nil {
		return err
	}
	if err := c.cl.callWith(http.MethodPost, path, body, nil, header); err != nil {
		return err
	}
	d, err := c.desktopStatus()
	if err != nil {
		return err
	}
	c.result(d, done)
	return nil
}
