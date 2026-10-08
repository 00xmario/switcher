package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Account is one account as the command line reports it.
type Account struct {
	ID         string   `json:"id"`
	Provider   string   `json:"provider"`
	Email      string   `json:"email"`
	Plan       string   `json:"plan,omitempty"`
	Active     bool     `json:"active"`
	ClaudeCode bool     `json:"claude_code,omitempty"`
	Health     string   `json:"health,omitempty"`
	Usage      []Window `json:"usage"`
	// BankedResets counts the account's unused usage-limit resets (Codex).
	BankedResets int `json:"banked_resets,omitempty"`
	nextReset    string
}

// Window is one usage window, such as a five-hour session or a week.
type Window struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at,omitempty"`
}

type serverState struct {
	Version string `json:"version"`
	Update  *struct {
		Latest    string `json:"latest"`
		Available bool   `json:"update_available"`
	} `json:"update"`
	Order    []string `json:"order"`
	Accounts []struct {
		ID           string `json:"id"`
		Provider     string `json:"provider"`
		Email        string `json:"email"`
		Plan         string `json:"plan"`
		Active       bool   `json:"active"`
		NativeActive bool   `json:"native_active"`
		Usage        *struct {
			Windows []Window `json:"windows"`
		} `json:"usage"`
		Health *struct {
			Condition string `json:"condition"`
		} `json:"health"`
		ResetCredits *struct {
			Count  int    `json:"count"`
			NextID string `json:"next_id"`
		} `json:"reset_credits"`
	} `json:"accounts"`
	DesktopRelay map[string]any `json:"desktop_relay"`
}

func (c *ctx) state() (serverState, []Account, error) {
	var st serverState
	if err := c.cl.call(http.MethodGet, "/api/state", nil, &st); err != nil {
		return st, nil, err
	}
	accounts := make([]Account, 0, len(st.Accounts))
	for _, a := range st.Accounts {
		account := Account{ID: a.ID, Provider: a.Provider, Email: a.Email, Plan: a.Plan, Active: a.Active, ClaudeCode: a.NativeActive, Usage: []Window{}}
		if a.Usage != nil && a.Usage.Windows != nil {
			account.Usage = a.Usage.Windows
		}
		if a.Health != nil {
			account.Health = a.Health.Condition
		}
		if a.ResetCredits != nil {
			account.BankedResets, account.nextReset = a.ResetCredits.Count, a.ResetCredits.NextID
		}
		accounts = append(accounts, account)
	}
	// Providers in the dashboard's order, active account first.
	rank := func(p string) int {
		if i := slices.Index(st.Order, p); i >= 0 {
			return i
		}
		return len(st.Order)
	}
	slices.SortStableFunc(accounts, func(a, b Account) int {
		if d := rank(a.Provider) - rank(b.Provider); d != 0 {
			return d
		}
		if a.Provider != b.Provider {
			return strings.Compare(a.Provider, b.Provider)
		}
		if a.Active != b.Active {
			if a.Active {
				return -1
			}
			return 1
		}
		return 0
	})
	return st, accounts, nil
}

// resolve finds one account by id, provider:email, email, the start of its id
// or a unique part of its email.
func resolve(ref string, accounts []Account) (Account, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Account{}, usagef("missing account")
	}
	for _, a := range accounts {
		if a.ID == ref {
			return a, nil
		}
	}
	provider, email, scoped := strings.Cut(ref, ":")
	matchers := []func(Account) bool{
		func(a Account) bool { return scoped && a.Provider == provider && strings.EqualFold(a.Email, email) },
		func(a Account) bool { return strings.EqualFold(a.Email, ref) },
		func(a Account) bool { return strings.HasPrefix(a.ID, ref) },
		func(a Account) bool { return strings.Contains(strings.ToLower(a.Email), strings.ToLower(ref)) },
	}
	for _, match := range matchers {
		var found []Account
		for _, a := range accounts {
			if match(a) {
				found = append(found, a)
			}
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], nil
		}
		names := make([]string, len(found))
		for i, a := range found {
			names[i] = fmt.Sprintf("%s:%s (%s)", a.Provider, a.Email, a.ID)
		}
		return Account{}, fmt.Errorf("%q matches %d accounts: %s", ref, len(found), strings.Join(names, ", "))
	}
	return Account{}, fmt.Errorf("no account matches %q; run 'switcher accounts' to list them", ref)
}

func usageText(windows []Window) string {
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, fmt.Sprintf("%s %.0f%%", w.Label, w.UsedPercent))
	}
	return strings.Join(parts, "  ")
}

func healthText(condition string) string {
	switch condition {
	case "", "usage_current", "checking":
		return ""
	case "needs_relogin":
		return "needs relogin"
	case "usage_unavailable":
		return "usage unavailable"
	}
	return strings.ReplaceAll(condition, "_", " ")
}

// accountTable lists accounts by provider, the active one marked.
func accountTable(accounts []Account) string {
	if len(accounts) == 0 {
		return "No accounts yet. Add one with: switcher login claude"
	}
	var rows [][]string
	last := ""
	for _, a := range accounts {
		if a.Provider != last {
			if last != "" {
				rows = append(rows, nil)
			}
			rows = append(rows, []string{providerName(a.Provider)})
			last = a.Provider
		}
		mark := " "
		if a.Active {
			mark = "●"
		}
		notes := []string{}
		if a.ClaudeCode {
			notes = append(notes, "Claude Code")
		}
		if h := healthText(a.Health); h != "" {
			notes = append(notes, h)
		}
		plan := planNames[a.Plan]
		if plan == "" {
			plan = a.Plan
		}
		row := []string{"  " + mark + " " + a.Email, plan, usageText(a.Usage)}
		if len(notes) > 0 {
			row = append(row, "· "+strings.Join(notes, ", "))
		}
		rows = append(rows, row)
	}
	return table(rows)
}

func runAccounts(c *ctx, args []string) error {
	fs := c.flags("accounts")
	provider := fs.String("provider", "", "")
	if _, err := c.parse(fs, args, 0, 0); err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	if *provider != "" {
		if err := knownProvider(*provider); err != nil {
			return err
		}
		accounts = slices.DeleteFunc(accounts, func(a Account) bool { return a.Provider != *provider })
	}
	c.result(map[string]any{"accounts": accounts}, accountTable(accounts))
	return nil
}

func runUse(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("use"), args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	var answer struct {
		Native *struct {
			Changed bool `json:"changed"`
		} `json:"native"`
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/activate", nil, &answer); err != nil {
		return err
	}
	account.Active = true
	switched := answer.Native != nil && answer.Native.Changed
	human := fmt.Sprintf("%s now uses %s.", providerName(account.Provider), account.Email)
	if switched {
		human += " Claude Code switched too."
	} else if answer.Native != nil {
		human += " Claude Code already used it."
	}
	c.result(map[string]any{"account": account, "claude_code_switched": switched}, human)
	return nil
}

func runRemove(c *ctx, args []string) error {
	fs := c.flags("remove")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	if !*yes {
		return usagef("this removes %s:%s from Switcher; add --yes to confirm", account.Provider, account.Email)
	}
	if err := c.cl.call(http.MethodDelete, "/api/accounts/"+url.PathEscape(account.ID), nil, nil); err != nil {
		return err
	}
	c.result(map[string]any{"removed": account}, fmt.Sprintf("Removed %s account %s.", providerName(account.Provider), account.Email))
	return nil
}

const resetHelp = `Spends one banked usage-limit reset (Codex) on an account, so it can be used
again right away. Name the account, or a provider for its active account:

  switcher reset codex --yes
  switcher reset me@example.com --yes

Banked resets cannot be given back, so the command asks for --yes.
`

func runReset(c *ctx, args []string) error {
	fs := c.flags("reset")
	yes := fs.Bool("yes", false, "")
	pos, err := c.parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	var account Account
	if _, provider := providerNames[pos[0]]; provider {
		found := false
		for _, a := range accounts {
			if a.Provider == pos[0] && a.Active {
				account, found = a, true
			}
		}
		if !found {
			return fmt.Errorf("no %s account is in use", providerName(pos[0]))
		}
	} else if account, err = resolve(pos[0], accounts); err != nil {
		return err
	}
	if account.nextReset == "" {
		return fmt.Errorf("%s has no banked reset", account.Email)
	}
	if !*yes {
		plural := "s"
		if account.BankedResets == 1 {
			plural = ""
		}
		return usagef("this spends one of the %d banked reset%s of %s; add --yes to confirm", account.BankedResets, plural, account.Email)
	}
	var answer struct {
		Outcome string `json:"outcome"`
		Usage   *struct {
			Windows []Window `json:"windows"`
		} `json:"usage"`
		Account *struct {
			ResetCredits *struct {
				Count int `json:"count"`
			} `json:"reset_credits"`
		} `json:"account"`
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/use-reset", map[string]string{"credit_id": account.nextReset}, &answer); err != nil {
		return err
	}
	left := 0
	if answer.Account != nil && answer.Account.ResetCredits != nil {
		left = answer.Account.ResetCredits.Count
	}
	human := map[string]string{
		"reset":            fmt.Sprintf("Used a banked reset on %s.", account.Email),
		"already_redeemed": fmt.Sprintf("That reset was already used on %s.", account.Email),
		"nothing_to_reset": fmt.Sprintf("%s has nothing to reset right now; the reset was kept.", account.Email),
	}[answer.Outcome]
	if human == "" {
		human = fmt.Sprintf("The provider answered %q for %s.", answer.Outcome, account.Email)
	}
	human += fmt.Sprintf(" %d banked reset%s left.", left, map[bool]string{true: "", false: "s"}[left == 1])
	c.result(map[string]any{"account": account.ID, "outcome": answer.Outcome, "banked_resets_left": left}, human)
	return nil
}

func runRefresh(c *ctx, args []string) error {
	pos, err := c.parse(c.flags("refresh"), args, 0, 1)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		if err := c.cl.call(http.MethodPost, "/api/usage/refresh", nil, nil); err != nil {
			return err
		}
		c.result(map[string]any{"refreshed": "all"}, "Refreshing usage for every account.")
		return nil
	}
	_, accounts, err := c.state()
	if err != nil {
		return err
	}
	account, err := resolve(pos[0], accounts)
	if err != nil {
		return err
	}
	if err := c.cl.call(http.MethodPost, "/api/accounts/"+url.PathEscape(account.ID)+"/refresh", nil, nil); err != nil {
		return err
	}
	c.result(map[string]any{"refreshed": account.ID}, fmt.Sprintf("Refreshed usage for %s.", account.Email))
	return nil
}
