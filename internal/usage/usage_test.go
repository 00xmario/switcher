package usage

import (
	"os"
	"testing"
)

// TestDebugCodexProbe is a scratch test: set CODEX_PROBE_FILE to parse a
// real rollout and print what the parser accepts.
func TestDebugCodexProbe(t *testing.T) {
	path := os.Getenv("CODEX_PROBE_FILE")
	if path == "" {
		t.Skip("no probe file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st := &codexScanState{}
	var last []byte
	n, lines := 0, 0
	for _, line := range splitAllLines(data) {
		lines++
		rec := parseCodexLine(line, st)
		if rec != nil {
			n++
			last = line
		}
	}
	t.Logf("lines=%d accepted=%d model=%s session=%s", lines, n, st.model, st.sessionID)
	if n > 0 {
		t.Logf("last accepted line: %.300s", last)
	}
	if n == 0 {
		t.Logf("MODEL EMPTY: %v", st.model == "")
	}
}

// splitAllLines splits on \n without the streaming reader.
func splitAllLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := range data {
		if data[i] == '\n' {
			line := data[start:i]
			line = trimCR(line)
			if len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		return b[:len(b)-1]
	}
	return b
}

// TestDebugScanFileProbe scans one real file through the streaming reader.
func TestDebugScanFileProbe(t *testing.T) {
	path := os.Getenv("CODEX_SCAN_FILE")
	if path == "" {
		t.Skip("no scan file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := LoadScanCache("/tmp/probe-scan-cache.json")
	count := 0
	scanFile(Source{Provider: ProviderCodex, Dir: "/Users/mariomakdis/.codex/sessions"}, path, info, cache, func(r Record) {
		count++
		if count == 1 {
			t.Logf("first record: model=%s session=%s totals=%+v", r.Model, r.SessionID, r.Totals)
		}
	})
	t.Logf("records=%d cache entry size=%d", count, cache.Files[path].Size)
}
