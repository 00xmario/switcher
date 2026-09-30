package claude

import (
	"encoding/json"
	"testing"
)

func TestClaudePlanUsesReportedTier(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x"}}`, "claude_max_5x"},
		{`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`, "claude_max_20x"},
		{`{"organization":{"organization_type":"claude_max"}}`, "claude_max"},
		{`{"organization":{"organization_type":"claude_pro"}}`, "claude_pro"},
		{`{"has_claude_max":true}`, "claude_max"},
		{`{}`, ""},
	} {
		var profile claudeProfile
		if err := json.Unmarshal([]byte(tc.raw), &profile); err != nil {
			t.Fatal(err)
		}
		if got := profile.plan(); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.raw, got, tc.want)
		}
	}
}
