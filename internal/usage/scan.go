package usage

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source describes one provider transcript directory.
type Source struct {
	Provider string
	Dir      string // e.g. ~/.codex/sessions
	FileName string // when set, only this basename is scanned (Grok)
}

// ResolveSources mirrors T3 Code's directory layout: $CODEX_HOME|~/.codex +
// /sessions, $CLAUDE_CONFIG_DIR|~/.claude + /projects, $GROK_HOME|~/.grok +
// /sessions (updates.jsonl only).
func ResolveSources() []Source {
	home := os.UserHomeDir
	h, _ := home()
	sources := []Source{}
	if codex := envOr("CODEX_HOME", filepath.Join(h, ".codex")); codex != "" {
		sources = append(sources, Source{Provider: ProviderCodex, Dir: filepath.Join(codex, "sessions")})
	}
	if claude := envOr("CLAUDE_CONFIG_DIR", filepath.Join(h, ".claude")); claude != "" {
		sources = append(sources, Source{Provider: ProviderClaude, Dir: filepath.Join(claude, "projects")})
	}
	if grok := envOr("GROK_HOME", filepath.Join(h, ".grok")); grok != "" {
		sources = append(sources, Source{Provider: ProviderGrok, Dir: filepath.Join(grok, "sessions"), FileName: "updates.jsonl"})
	}
	return sources
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// cachedFile stores what a previous scan learned about one file so an
// unchanged file costs no I/O and a grown file is resumed at the old offset.
type cachedFile struct {
	Size         int64             `json:"size"`
	MtimeNs      int64             `json:"mtime_ns"`
	Provider     string            `json:"provider"`
	Records      []persistedRecord `json:"records"`
	ResumeOffset int64             `json:"resume_offset"`
	Guard        string            `json:"guard,omitempty"`
	Codex        *codexScanState   `json:"codex_state,omitempty"`
}

// persistedRecord is a Record with the redundant fields dropped.
type persistedRecord struct {
	TS    int64    `json:"ts"`
	Model string   `json:"model"`
	SID   string   `json:"sid,omitempty"`
	T     Totals   `json:"t"`
	Rep   *float64 `json:"rep,omitempty"`
	Ded   string   `json:"ded,omitempty"`
}

// ScanCache is the on-disk cache (~/.switcher/usage-scan-cache.json).
type ScanCache struct {
	Version int                   `json:"version"`
	Files   map[string]cachedFile `json:"files"`
	mu      sync.Mutex
	dirty   bool
	path    string
}

const scanCacheVersion = 1

// LoadScanCache reads the cache file, returning an empty cache on any error.
func LoadScanCache(path string) *ScanCache {
	c := &ScanCache{Version: scanCacheVersion, Files: map[string]cachedFile{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var loaded ScanCache
	if json.Unmarshal(data, &loaded) != nil || loaded.Version != scanCacheVersion {
		return c
	}
	if loaded.Files != nil {
		c.Files = loaded.Files
	}
	return c
}

// Save writes the cache when something changed.
func (c *ScanCache) Save() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	// Retention: entries whose source file has not been touched in 90 days
	// are dropped so the cache cannot grow without bound.
	horizon := time.Now().AddDate(0, 0, -90)
	for key, entry := range c.Files {
		if time.Unix(0, entry.MtimeNs).Before(horizon) {
			delete(c.Files, key)
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, c.path)
	}
	c.dirty = false
}

// prune drops entries older than the retention horizon.
func (c *ScanCache) prune(olderThan time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.Files {
		if time.Unix(0, v.MtimeNs).Before(olderThan) {
			delete(c.Files, k)
			c.dirty = true
		}
	}
}

// scanOptions bounds a scan to a time window.
type scanOptions struct {
	WindowStart time.Time // records older than this (day granularity) are ignored
	Source      Source
}

// recordSink receives parsed records.
type recordSink func(Record)

// scanSource walks one provider directory and feeds deduplicated records to
// sink. Files whose mtime predates the window plus slack are skipped; files
// that grew since the last run are resumed at the cached byte offset.
func scanSource(src Source, opt scanOptions, cache *ScanCache, sink recordSink) {
	if _, err := os.Stat(src.Dir); err != nil {
		return
	}
	// mtime prefilter: window start minus 36h of slack, like T3 Code.
	minMtime := opt.WindowStart.Add(-36 * time.Hour).UnixNano()

	var files []string
	_ = filepath.WalkDir(src.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if src.FileName != "" {
			if d.Name() == src.FileName {
				files = append(files, path)
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)

	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.ModTime().UnixNano() < minMtime {
			continue
		}
		scanFile(src, path, info, cache, sink)
	}
}

// guardBytes is how many bytes before the resume offset are hashed to detect
// files that changed underneath us.
const guardBytes = 64

func scanFile(src Source, path string, info os.FileInfo, cache *ScanCache, sink recordSink) {
	key := path
	cache.mu.Lock()
	cached, haveCached := cache.Files[key]
	cache.mu.Unlock()

	var (
		records    []persistedRecord
		st         codexScanState
		resumeFrom int64
		guard      string
	)

	if haveCached && cached.Provider == src.Provider {
		switch {
		case cached.Size == info.Size() && cached.MtimeNs == info.ModTime().UnixNano():
			// Unchanged: replay cached records, no file I/O.
			for _, r := range cached.Records {
				sink(cachedToRecord(src.Provider, r, src.Dir))
			}
			return
		case info.Size() > cached.Size && cached.ResumeOffset > guardBytes:
			// Grew since last scan: resume, keeping earlier records.
			records = cached.Records
			resumeFrom = cached.ResumeOffset
			guard = cached.Guard
			st = derefState(cached.Codex)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// Verify the guard so a rewritten file restarts from byte zero.
	if resumeFrom > 0 && guard != "" {
		buf := make([]byte, guardBytes)
		if _, err := f.ReadAt(buf, resumeFrom-guardBytes); err != nil || sha256.Sum256(buf) != mustHash(guard) {
			records, resumeFrom, guard, st = nil, 0, "", codexScanState{}
		}
	}
	if _, err := f.Seek(resumeFrom, io.SeekStart); err != nil {
		return
	}

	handle := func(raw []byte, complete bool) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 || !mightCarryUsage(src.Provider, line) {
			return
		}
		var recs []*Record
		switch src.Provider {
		case ProviderCodex:
			if r := parseCodexLine(line, &st); r != nil {
				recs = []*Record{r}
			}
		case ProviderClaude:
			if r := parseClaudeLine(line); r != nil {
				recs = []*Record{r}
			}
		case ProviderGrok:
			recs = parseGrokLine(line)
		}
		for _, r := range recs {
			r.SourceDir = src.Dir
			sink(*r)
			if complete {
				records = append(records, recordToPersisted(r))
			}
		}
	}

	// Stream lines with ReadBytes so arbitrarily long lines work. Only
	// newline-terminated lines advance the resume offset: an unterminated
	// tail is parsed and delivered but never cached, so a half-written
	// record is not double counted.
	consumed := resumeFrom
	reader := bufio.NewReaderSize(f, 256*1024)
	for {
		lineBytes, readErr := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			complete := lineBytes[len(lineBytes)-1] == '\n'
			if complete {
				consumed += int64(len(lineBytes))
			}
			handle(bytes.TrimSuffix(lineBytes, []byte("\n")), complete)
		}
		if readErr != nil {
			break
		}
	}

	newSize := info.Size()
	var newGuard string
	if newSize > guardBytes {
		buf := make([]byte, guardBytes)
		if _, err := f.ReadAt(buf, newSize-guardBytes); err == nil {
			newGuard = hex.EncodeToString(hashBytes(buf))
		}
	}

	cache.mu.Lock()
	cache.Files[key] = cachedFile{
		Size:         newSize,
		MtimeNs:      info.ModTime().UnixNano(),
		Provider:     src.Provider,
		Records:      records,
		ResumeOffset: consumed,
		Guard:        newGuard,
		Codex:        codexStatePtr(st),
	}
	cache.dirty = true
	cache.mu.Unlock()
}

func recordToPersisted(r *Record) persistedRecord {
	return persistedRecord{TS: r.Timestamp.UnixMilli(), Model: r.Model, SID: r.SessionID, T: r.Totals, Rep: r.Reported, Ded: r.DedupeKey}
}

func cachedToRecord(provider string, p persistedRecord, sourceDir string) Record {
	return Record{Provider: provider, Model: p.Model, SessionID: p.SID,
		Timestamp: time.UnixMilli(p.TS).UTC(), Totals: p.T, Reported: p.Rep, DedupeKey: p.Ded, SourceDir: sourceDir}
}

func codexStatePtr(st codexScanState) *codexScanState {
	if st == (codexScanState{}) {
		return nil
	}
	return &st
}

func derefState(st *codexScanState) codexScanState {
	if st == nil {
		return codexScanState{}
	}
	return *st
}

func hashBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func mustHash(hexStr string) [32]byte {
	raw, _ := hex.DecodeString(hexStr)
	var out [32]byte
	copy(out[:], raw)
	return out
}
