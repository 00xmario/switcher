package sessionmeta

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const cachedAlias = "11111111-1111-4111-8111-111111111111"
const cachedGroup = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const cachedMissing = "22222222-2222-4222-8222-222222222222"

func TestResolveCachedNeverWaitsScansOrMutatesSnapshot(t *testing.T) {
	i := New(Config{DesktopRoot: filepath.Join(t.TempDir(), "missing")})
	if got := i.ResolveCached([]string{cachedAlias}, nil); len(got) != 0 || !i.expires.IsZero() {
		t.Fatal("cold cached lookup scanned or returned authority")
	}
	i.cache = snapshot{conversations: map[string]string{cachedAlias: cachedGroup}, validGroups: map[string]bool{cachedGroup: true}, historicalComplete: true}
	i.expires = time.Now().Add(time.Minute)
	known := map[string]string{cachedMissing: cachedGroup}
	want := map[string]string{cachedAlias: cachedGroup, cachedMissing: cachedGroup}
	if got := i.ResolveCached([]string{cachedAlias, cachedMissing, "invalid"}, known); !reflect.DeepEqual(got, want) {
		t.Fatalf("cached proof = %#v", got)
	} else {
		got[cachedAlias] = "caller mutation"
	}
	i.gate <- struct{}{} // A full scan owns this same gate.
	done := make(chan map[string]string, 1)
	go func() { done <- i.ResolveCached([]string{cachedAlias}, known) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Error("busy cache returned authority")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("cached lookup waited behind full scan")
	}
	<-i.gate
	i.expires = time.Now().Add(-time.Second)
	if got := i.ResolveCached([]string{cachedAlias, cachedMissing}, known); len(got) != 0 {
		t.Fatal("expired cached snapshot granted authority")
	}
	if i.cache.conversations[cachedAlias] != cachedGroup || !reflect.DeepEqual(known, map[string]string{cachedMissing: cachedGroup}) {
		t.Fatal("cached lookup mutated shared state")
	}
}

func TestResolveNegativeCachePreventsRepeatedScansUntilExpiry(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "a", "workspace")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	i := New(Config{DesktopRoot: root})
	if got, err := i.Resolve(context.Background(), []string{cachedMissing}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	path := filepath.Join(dir, "local_"+cachedGroup+".json")
	data := `{"sessionId":"local_` + cachedGroup + `","cliSessionId":"` + cachedMissing + `","createdAt":1788432998079,"cwd":"/fixture/project"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 100; j++ {
		if got, err := i.Resolve(context.Background(), []string{cachedMissing}); err != nil || len(got) != 0 {
			t.Fatalf("negative hit reread updated fixture on request %d: %#v, %v", j, got, err)
		}
	}
	i.expires = time.Now().Add(-time.Second)
	if got, err := i.Resolve(context.Background(), []string{cachedMissing}); err != nil || got[cachedMissing] != cachedGroup {
		t.Fatalf("TTL refresh did not discover source: %#v, %v", got, err)
	}
}

func TestResolveCachedPreservesHistoricalProofChecks(t *testing.T) {
	for _, reason := range []string{"valid", "claimed", "incomplete", "vetoed", "disputed", "current wins"} {
		t.Run(reason, func(t *testing.T) {
			i := New(Config{})
			i.expires = time.Now().Add(time.Minute)
			i.cache = snapshot{conversations: map[string]string{cachedAlias: cachedGroup}, validGroups: map[string]bool{cachedGroup: true}, claimedAliases: make(map[string]bool), historicalComplete: true}
			known := map[string]string{cachedMissing: cachedGroup}
			want := map[string]string{cachedAlias: cachedGroup}
			switch reason {
			case "valid":
				want[cachedMissing] = cachedGroup
			case "claimed":
				i.cache.claimedAliases[cachedMissing] = true
			case "incomplete":
				i.cache.historicalComplete = false
			case "vetoed":
				delete(i.cache.validGroups, cachedGroup)
			case "disputed":
				known[cachedMissing] = "local_" + cachedGroup
			case "current wins":
				i.cache.conversations[cachedMissing] = cachedAlias
				want[cachedMissing] = cachedAlias
			}
			if got := i.ResolveCached([]string{cachedAlias, cachedMissing, "00000000-0000-0000-0000-000000000000", "../bad"}, known); !reflect.DeepEqual(got, want) {
				t.Fatalf("cached proof = %#v, want %#v", got, want)
			}
		})
	}
}

func TestResolveUnseenMissForcesOnceAndBoundedMissesSuppressFurtherScans(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "a", "workspace")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	i := New(Config{DesktopRoot: root})
	i.Lookup(context.Background(), []string{cachedAlias})
	path := filepath.Join(dir, "local_"+cachedGroup+".json")
	data := `{"sessionId":"local_` + cachedGroup + `","cliSessionId":"` + cachedMissing + `","createdAt":1788432998079,"cwd":"/fixture/project"}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if got := i.ResolveCached([]string{cachedMissing}, nil); len(got) != 0 {
		t.Fatal("cached-only lookup read the newly added file")
	}
	if got, err := i.Resolve(context.Background(), []string{cachedMissing}); err != nil || got[cachedMissing] != cachedGroup {
		t.Fatal("unseen alias did not force a current scan", got, err)
	}
	expires := i.expires
	i.misses = make(map[string]bool)
	for j := 0; j < maxEntries; j++ {
		i.misses[fmt.Sprintf("%08x-1111-4111-8111-111111111111", j)] = true
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(data, cachedMissing, cachedAlias)), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := i.Resolve(context.Background(), []string{cachedAlias}); err != nil || len(got) != 0 || len(i.misses) != maxEntries || !i.expires.Equal(expires) {
		t.Fatal("full negative cache caused scan, grew, or extended TTL", got, err)
	}
}
