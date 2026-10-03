// Package sessionmeta reads saved session metadata for local response views.
// It never discovers HOME, creates directories, or opens transcript files.
package sessionmeta

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type Config struct {
	DesktopRoot  string
	ProjectsRoot string
}

// Info contains display metadata and an internal verified identity, never a
// saved record or an absolute cwd.
type Info struct {
	Title       string `json:"title,omitempty"`
	Project     string `json:"project,omitempty"`
	TitleSource string `json:"title_source,omitempty"`
	ClientKind  string `json:"client_kind,omitempty"`
	// ConversationID is trusted membership, serialized only by explicit callers.
	ConversationID string `json:"-"`
}

const (
	cacheTTL   = 3 * time.Second
	lookupTime = 750 * time.Millisecond
)

type Index struct {
	cfg     Config
	gate    chan struct{}
	cache   snapshot
	expires time.Time
	misses  map[string]bool
}

// New is lazy, including when a configured root is missing or unsafe.
func New(cfg Config) *Index {
	return &Index{cfg: cfg, gate: make(chan struct{}, 1)}
}

// Lookup returns only exact, nonzero UUID matches, keyed by the requested ID.
// Missing, unsafe and malformed metadata is ordinary absence, not an API error.
func (i *Index) Lookup(ctx context.Context, ids []string) map[string]Info {
	info, _, _ := i.lookup(ctx, ids, false, nil)
	return info
}

type snapshot struct {
	info               map[string]Info
	conversations      map[string]string
	validGroups        map[string]bool
	claimedAliases     map[string]bool
	historicalComplete bool
}

func (i *Index) lookup(ctx context.Context, ids []string, resolve bool, known map[string]string) (map[string]Info, map[string]string, error) {
	info, conversations := make(map[string]Info), make(map[string]string)
	if err := ctx.Err(); err != nil {
		return info, conversations, err
	}
	if i == nil || len(ids) == 0 {
		return info, conversations, nil
	}
	wanted := make(map[string]string)
	for _, id := range ids {
		if canonical := uuid(id); canonical != "" {
			wanted[id] = canonical
		}
	}
	if len(wanted) == 0 {
		return info, conversations, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTime)
	defer cancel()
	select {
	case i.gate <- struct{}{}:
		defer func() { <-i.gate }()
	case <-ctx.Done():
		return info, conversations, ctx.Err()
	}
	if ctx.Err() != nil {
		return info, conversations, ctx.Err()
	}
	expired := !time.Now().Before(i.expires)
	refresh := expired
	if resolve && !refresh && len(i.misses) < maxEntries {
		for _, canonical := range wanted {
			if i.cache.conversations[canonical] == "" && !i.misses[canonical] {
				// At most one forced scan per missing UUID within this TTL.
				refresh = true
				break
			}
		}
	}
	if refresh {
		cache := i.scan(ctx)
		if ctx.Err() != nil {
			return info, conversations, ctx.Err() // Do not publish canceled scans.
		}
		i.cache = cache
		if expired {
			i.expires = time.Now().Add(cacheTTL)
			i.misses = make(map[string]bool)
		}
	}
	if resolve {
		for _, canonical := range wanted {
			if i.cache.conversations[canonical] == "" && len(i.misses) < maxEntries {
				if i.misses == nil {
					i.misses = make(map[string]bool)
				}
				i.misses[canonical] = true
			}
		}
	}
	prior, disputed := canonicalKnown(ctx, known)
	for original, canonical := range wanted {
		if saved, ok := i.cache.info[canonical]; ok {
			info[original] = saved
		}
		if conversation := i.cache.association(canonical, prior, disputed); conversation != "" {
			conversations[original] = conversation
		}
	}
	if err := ctx.Err(); err != nil {
		return make(map[string]Info), make(map[string]string), err
	}
	return info, conversations, nil
}

func (s snapshot) association(alias string, prior map[string]string, disputed map[string]bool) string {
	if current := s.conversations[alias]; current != "" {
		return current
	}
	if group := prior[alias]; group != "" && s.historicalComplete && !disputed[alias] && !s.claimedAliases[alias] && s.validGroups[group] {
		return group
	}
	return ""
}

func uuid(s string) string {
	if len(s) != 36 || s == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	for j, c := range s {
		if j == 8 || j == 13 || j == 18 || j == 23 {
			if c != '-' {
				return ""
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return ""
		}
	}
	return strings.ToLower(s)
}

func (i *Index) scan(ctx context.Context) snapshot {
	out := make(map[string]Info)
	b := newBudget(ctx)
	native := newMembership(ctx)
	desktop := make(map[string][]match)
	nativeComplete := walkMetadata(i.cfg.DesktopRoot, 2, b, func(name string) bool {
		return strings.HasPrefix(name, "local_") && strings.HasSuffix(name, ".json")
	}, func(name string, data []byte) {
		native.trackClaims(data)
		var record struct {
			ID       string          `json:"cliSessionId"`
			Title    string          `json:"title"`
			Cwd      string          `json:"cwd"`
			Activity json.RawMessage `json:"lastActivityAt"`
		}
		if !decodeMetadata(ctx, data, &record) {
			native.reject(name)
			return
		}
		native.add(name, data, uuid(record.ID))
		if id := uuid(record.ID); id != "" {
			desktop[id] = append(desktop[id], match{record.Title, record.Cwd, activity(record.Activity)})
		}
	}, func(name string) {
		native.reject(name)
		native.historyReadable = false
	})
	for id, records := range desktop {
		if record, ok := authoritative(records); ok {
			out[id] = display(record, "desktop")
		}
	}
	cli := make(map[string][]match)
	walkMetadata(i.cfg.ProjectsRoot, 1, b, func(name string) bool {
		return name == "sessions-index.json"
	}, func(_ string, data []byte) {
		var index struct {
			Entries []json.RawMessage `json:"entries"`
		}
		if !decodeMetadata(ctx, data, &index) {
			return
		}
		for _, raw := range index.Entries {
			if !b.active() {
				break
			}
			b.entries--
			var record struct {
				ID          string `json:"sessionId"`
				CustomTitle string `json:"customTitle"`
				Summary     string `json:"summary"`
				ProjectPath string `json:"projectPath"`
			}
			if json.Unmarshal(raw, &record) != nil {
				continue
			}
			id := uuid(record.ID)
			if id == "" {
				continue
			}
			title := record.CustomTitle
			if cleanText(title) == "" {
				title = record.Summary
			}
			cli[id] = append(cli[id], match{title: title, cwd: record.ProjectPath})
		}
	}, nil)
	if !b.active() {
		// A truncated scan cannot prove that an unseen copy would not conflict.
		return snapshot{}
	}
	for id, records := range cli {
		record, ok := authoritative(records)
		if !ok {
			continue
		}
		fallback := display(record, "cli")
		info, exists := out[id]
		if !exists {
			out[id] = fallback
			continue
		}
		if info.Title == "" {
			info.Title, info.TitleSource = fallback.Title, fallback.TitleSource
		}
		if info.Project == "" {
			info.Project = fallback.Project
		}
		out[id] = info
	}
	conversations := make(map[string]string)
	validGroups, claimed := make(map[string]bool), make(map[string]bool)
	if nativeComplete {
		conversations = native.resolve()
		for group := range native.groups {
			if !native.invalidGroups[group] {
				validGroups[group] = true
			}
		}
		for alias := range native.aliases {
			claimed[alias] = true
		}
		for alias := range native.invalidAlias {
			claimed[alias] = true
		}
		for alias := range native.claimed {
			claimed[alias] = true
		}
	}
	// Membership is independent of display-title/project selection. A verified
	// member with disagreeing titles still belongs in grouped revision checks.
	for id, conversation := range conversations {
		info := out[id]
		info.ClientKind, info.ConversationID = "desktop", conversation
		out[id] = info
	}
	return snapshot{info: out, conversations: conversations, validGroups: validGroups, claimedAliases: claimed,
		historicalComplete: nativeComplete && native.historyReadable}
}

type match struct {
	title, cwd string
	active     time.Time
}

func display(record match, source string) Info {
	info := Info{Title: cleanText(record.title), Project: project(record.cwd), ClientKind: source}
	if info.Title != "" {
		info.TitleSource = source
	}
	return info
}

// Identical title/cwd copies are harmless. Conflicting copies require valid
// activity times on every candidate and one newest title/cwd identity. Missing
// or tied activity is ambiguous, including CLI copies with no activity field.
// File names, mtimes, createdAt, focusedAt and traversal order are not authority.
func authoritative(records []match) (match, bool) {
	first := records[0]
	same := func(a, b match) bool { return a.title == b.title && a.cwd == b.cwd }
	identical := true
	for _, record := range records[1:] {
		if !same(first, record) {
			identical = false
		}
	}
	if identical {
		return first, true
	}
	var newest match
	ambiguous := false
	for _, record := range records {
		if record.active.IsZero() {
			return match{}, false
		}
		if record.active.After(newest.active) {
			newest, ambiguous = record, false
		} else if record.active.Equal(newest.active) && !same(newest, record) {
			ambiguous = true
		}
	}
	return newest, !ambiguous
}

func activity(raw json.RawMessage) time.Time {
	var millis int64
	if json.Unmarshal(raw, &millis) == nil && millis > 0 && millis <= 253402300799999 {
		return time.UnixMilli(millis)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if stamp, err := time.Parse(time.RFC3339Nano, text); err == nil && stamp.After(time.Unix(0, 0)) {
			return stamp
		}
	}
	return time.Time{}
}

func project(cwd string) string {
	if !filepath.IsAbs(cwd) || strings.ContainsAny(cwd, "\x00\r\n") {
		return ""
	}
	name := filepath.Base(filepath.Clean(cwd))
	if name == "/" || name == "." || name == ".." {
		return ""
	}
	return cleanText(name)
}

// Treat saved titles as plain text. Markup and embedded URLs stay literal and
// are escaped by the JSON encoder and UI; neither is interpreted or fetched.
func cleanText(text string) string {
	runes := make([]rune, 0, 160)
	space := false
	for _, r := range text {
		if unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			space = len(runes) > 0
			continue
		}
		if space {
			runes = append(runes, ' ')
			space = false
		}
		runes = append(runes, r)
		if len(runes) >= 160 {
			return strings.TrimSpace(string(runes[:160]))
		}
	}
	return string(runes)
}
