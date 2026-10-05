package desktoprelay

import (
	"encoding/json"
	"testing"
)

func TestWithLocalNoProxyMergesWithoutDuplicates(t *testing.T) {
	for raw, want := range map[string]string{
		``:                                   "localhost,127.0.0.1,::1",
		`""`:                                 "localhost,127.0.0.1,::1",
		`"corp.example, LOCALHOST"`:          "corp.example,LOCALHOST,127.0.0.1,::1",
		`"localhost,127.0.0.1,::1,10.0.0.5"`: "localhost,127.0.0.1,::1,10.0.0.5",
	} {
		var got string
		if err := json.Unmarshal(withLocalNoProxy(json.RawMessage(raw)), &got); err != nil || got != want {
			t.Fatalf("%s: got %q, want %q", raw, got, want)
		}
	}
	if string(withLocalNoProxy(json.RawMessage(`7`))) != `7` {
		t.Fatal("non-string NO_PROXY was rewritten")
	}
}
