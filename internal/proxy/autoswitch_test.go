package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/store"
)

// autoNative is a Claude provider whose native login (Claude Code) is held
// in memory.
type autoNative struct {
	*fakeProvider
	mu       sync.Mutex
	status   claudecode.Status
	switched []string
	fail     error
}

func (p *autoNative) NativeEnabled() bool { return true }
func (p *autoNative) NativeStatus() claudecode.Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}
func (p *autoNative) SwitchNative(_ context.Context, id string, commit func(string) error) (claudecode.SwitchResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.switched = append(p.switched, id)
	if p.fail != nil {
		return claudecode.SwitchResult{}, p.fail
	}
	if err := commit("receipt-" + id); err != nil {
		return claudecode.SwitchResult{}, err
	}
	p.status.ActiveID = id
	return claudecode.SwitchResult{Changed: true, Message: "switched"}, nil
}
func (p *autoNative) SyncNative(context.Context, *store.Account) (bool, error) { return false, nil }

func claudeAccount(id string) store.Account {
	return store.Account{ID: id, Provider: "claude", Email: id + "@example.com",
		Token: store.Token{AccessToken: id + "-token", AccountID: "uuid-" + id}}
}

func windows(session, weekly int) provider.Usage {
	reset := time.Now().Add(time.Hour).Unix()
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{
		{Label: "Session", UsedPercent: session, ResetsAt: reset}, {Label: "Weekly", UsedPercent: weekly, ResetsAt: reset}}}
}

func autoManager(t *testing.T, on bool, accounts ...store.Account) (*Manager, *autoNative) {
	t.Helper()
	st := store.New(t.TempDir())
	for _, a := range accounts {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}
	native := &autoNative{fakeProvider: &fakeProvider{id: "claude"}, status: claudecode.Status{Condition: "ready", ActiveID: accounts[0].ID}}
	m, err := New(st, map[string]provider.Provider{"claude": native}, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	m.SetClaudeAutoSwitch(func() bool { return on })
	return m, native
}

func (m *Manager) setUsage(id string, u provider.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastUsage[id] = u
}

func TestClaudeCodeMovesToTheAccountWithTheMostUsageLeft(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	m.setUsage("a", windows(100, 40))
	m.setUsage("b", windows(60, 70))
	m.setUsage("c", windows(10, 20))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "c" {
		t.Fatalf("switched %v, want c", native.switched)
	}
	if m.ActiveID("claude") != "c" {
		t.Fatalf("proxy route %q", m.ActiveID("claude"))
	}
	if event := m.LastAutoSwitch(); event == nil || event.From != "a" || event.To != "c" || event.Where != "claude_code" {
		t.Fatalf("event %+v", event)
	}
	// c has room, so nothing happens next time.
	m.AutoSwitchNative()
	if len(native.switched) != 1 {
		t.Fatalf("switched again: %v", native.switched)
	}
}

func TestClaudeCodeStaysWhenItShould(t *testing.T) {
	for _, tc := range []struct {
		name  string
		on    bool
		ready string
		setup func(m *Manager)
		extra []store.Account
	}{
		{"switching is off", false, "ready", func(m *Manager) { m.setUsage("a", windows(100, 0)); m.setUsage("b", windows(0, 0)) }, nil},
		{"the account still has room", true, "ready", func(m *Manager) { m.setUsage("a", windows(99, 50)); m.setUsage("b", windows(0, 0)) }, nil},
		{"no other account has room", true, "ready", func(m *Manager) { m.setUsage("a", windows(100, 0)); m.setUsage("b", windows(30, 100)) }, nil},
		{"the other account's usage is unknown", true, "ready", func(m *Manager) { m.setUsage("a", windows(100, 0)) }, nil},
		{"Claude Code is signed in with an account Switcher does not manage", true, "unmanaged", func(m *Manager) { m.setUsage("a", windows(100, 0)); m.setUsage("b", windows(0, 0)) }, nil},
		{"a model's own window is not the account", true, "ready", func(m *Manager) {
			u := windows(10, 10)
			u.Windows = append(u.Windows, provider.UsageWindow{Label: "Opus", UsedPercent: 100})
			m.setUsage("a", u)
			m.setUsage("b", windows(0, 0))
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, native := autoManager(t, tc.on, claudeAccount("a"), claudeAccount("b"))
			native.status.Condition = tc.ready
			tc.setup(m)
			m.AutoSwitchNative()
			if len(native.switched) != 0 {
				t.Fatalf("switched %v", native.switched)
			}
		})
	}
}

func TestClaudeCodeSkipsAnAccountItsLoginCannotCarry(t *testing.T) {
	noIdentity := claudeAccount("b")
	noIdentity.Token.AccountID = ""
	m, native := autoManager(t, true, claudeAccount("a"), noIdentity, claudeAccount("c"))
	m.setUsage("a", windows(100, 0))
	m.setUsage("b", windows(0, 0))
	m.setUsage("c", windows(50, 50))
	m.AutoSwitchNative()
	if len(native.switched) != 1 || native.switched[0] != "c" {
		t.Fatalf("switched %v, want c", native.switched)
	}
}

func TestAFailedAutomaticSwitchWaitsBeforeTryingAgain(t *testing.T) {
	m, native := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	native.fail = errors.New("Claude Code is holding its credential lock")
	m.setUsage("a", windows(100, 0))
	m.setUsage("b", windows(0, 0))
	m.AutoSwitchNative()
	m.AutoSwitchNative()
	if len(native.switched) != 1 {
		t.Fatalf("tried %d times within the cooldown", len(native.switched))
	}
	if m.LastAutoSwitch() != nil {
		t.Fatal("a failed switch was reported")
	}
}

func TestDesktopTakeoverPicksTheMostRoomAndParksTheOutAccount(t *testing.T) {
	m, _ := autoManager(t, true, claudeAccount("a"), claudeAccount("b"), claudeAccount("c"))
	m.setUsage("b", windows(80, 10))
	m.setUsage("c", windows(30, 30))
	src := NewDesktopCredentialSource(m)
	until := time.Now().Add(2 * time.Hour)
	if !src.OutOfUsage(context.Background(), "a", 429, []byte(`{}`), until) {
		t.Fatal("Anthropic's rejected header was not taken as out of usage")
	}
	if got, parked := m.Exhausted("a"); !parked || got.Unix() != until.Unix() {
		t.Fatalf("a parked until %v (%v)", got, parked)
	}
	if next := src.Takeover(context.Background(), "a"); next != "c" {
		t.Fatalf("takeover %q, want c", next)
	}
	src.TookOver("", "c")
	if event := m.LastAutoSwitch(); event == nil || event.Where != "desktop" || event.To != "c" {
		t.Fatalf("event %+v", event)
	}
	m.SetClaudeAutoSwitch(func() bool { return false })
	if next := src.Takeover(context.Background(), "a"); next != "" {
		t.Fatalf("takeover %q while switching is off", next)
	}
}

func TestAPairedMacsRequestAvoidsAnAccountThatRanOut(t *testing.T) {
	m, _ := autoManager(t, true, claudeAccount("a"), claudeAccount("b"))
	m.setUsage("a", windows(100, 0))
	m.setUsage("b", windows(20, 20))
	if got := m.ClaudeRoute("a"); got != "b" {
		t.Fatalf("route %q, want b", got)
	}
	if got := m.ClaudeRoute("b"); got != "b" {
		t.Fatalf("route %q, want b", got)
	}
	m.SetClaudeAutoSwitch(func() bool { return false })
	if got := m.ClaudeRoute("a"); got != "a" {
		t.Fatalf("route %q with switching off", got)
	}
}
