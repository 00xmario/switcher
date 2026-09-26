// Package store persists Switcher's accounts and routing state as plain JSON
// files under the data directory. Every write is atomic (temp file + rename)
// and every token file is created with 0600 permissions.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Token set returned by an OAuth login or a refresh. Extra carries
// provider-specific fields (project ids, token expiry timestamps) that do
// not fit the fixed columns; provider packages go through typed helpers,
// never raw map pokes at call sites.
type Token struct {
	IDToken      string         `json:"id_token"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token"`
	AccountID    string         `json:"account_id,omitempty"`
	ExpiresAt    int64          `json:"expires_at,omitempty"` // unix seconds
	Extra        map[string]any `json:"extra,omitempty"`
}

// Account is one stored login for one provider.
type Account struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Email       string `json:"email"`
	Plan        string `json:"plan,omitempty"`
	Token       Token  `json:"token"`
	CreatedAt   int64  `json:"created_at"`
	LastRefresh int64  `json:"last_refresh,omitempty"`
}

// State is the persisted routing state: the active account per provider,
// which accounts are exhausted (until a unix timestamp), plus the user's
// provider display order and hidden providers.
type State struct {
	Active          map[string]string `json:"active,omitempty"`
	Exhausted       map[string]int64  `json:"exhausted,omitempty"`
	ProviderOrder   []string          `json:"provider_order,omitempty"`
	HiddenProviders []string          `json:"hidden_providers,omitempty"`
	ManagementKey   string            `json:"management_key,omitempty"`
}

// ErrNotFound is returned when an account ID does not exist.
var ErrNotFound = errors.New("account not found")

// Store owns the accounts directory and the state file.
type Store struct {
	accountsDir string
	statePath   string

	listMu          sync.Mutex
	listCache       []Account
	listMtime       int64
	listFilesNewest int64
}

// New creates a Store rooted at the given directory: accounts live in
// <root>/accounts and routing state in <root>/state.json.
func New(root string) *Store {
	accounts := filepath.Join(root, "accounts")
	_ = os.MkdirAll(accounts, 0o700)
	return &Store{accountsDir: accounts, statePath: filepath.Join(root, "state.json")}
}

// List returns every stored account sorted by email.
func (s *Store) List() ([]Account, error) {
	info, err := os.Stat(s.accountsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	// The proxy hot path and the state poll call List on every request;
	// re-decoding every account file each time serialises traffic behind
	// disk reads, so the decoded list is cached until the directory or any
	// file in it changes.
	mtime := info.ModTime().UnixNano()
	entries, err := os.ReadDir(s.accountsDir)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	var newest int64
	for _, e := range entries {
		if fi, err := e.Info(); err == nil {
			if t := fi.ModTime().UnixNano(); t > newest {
				newest = t
			}
		}
	}
	s.listMu.Lock()
	if s.listCache != nil && s.listMtime == mtime && s.listFilesNewest == newest {
		out := make([]Account, len(s.listCache))
		copy(out, s.listCache)
		s.listMu.Unlock()
		return out, nil
	}
	s.listMu.Unlock()

	var out []Account
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var a Account
		// A single corrupt file must not brick the whole tool; skip it and
		// let the log show what happened.
		if err := readJSON(filepath.Join(s.accountsDir, e.Name()), &a); err != nil {
			log.Printf("store: skipping unreadable account file %s: %v", e.Name(), err)
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	s.listMu.Lock()
	s.listCache = out
	s.listMtime = mtime
	s.listFilesNewest = newest
	s.listMu.Unlock()
	result := make([]Account, len(out))
	copy(result, out)
	return result, nil
}

// validIDCharacter rejects path separators and anything unexpected so
// account ids straight from any API cannot traverse the accounts dir.
func validIDCharacter(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Get returns one account by ID.
func (s *Store) Get(id string) (Account, error) {
	if !validIDCharacter(id) {
		return Account{}, fmt.Errorf("invalid account id %q", id)
	}
	var a Account
	if err := readJSON(s.accountPath(id), &a); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Account{}, ErrNotFound
		}
		return Account{}, err
	}
	return a, nil
}

// Save writes an account atomically.
func (s *Store) Save(a Account) error {
	defer s.invalidateList()
	if a.ID == "" {
		return errors.New("save account: empty id")
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account: %w", err)
	}
	return writeAtomic(s.accountPath(a.ID), raw, 0o600)
}

// Delete removes an account file. Removing a non-existent account is not an
// error so callers can treat delete as idempotent.
func (s *Store) Delete(id string) error {
	defer s.invalidateList()
	if err := os.Remove(s.accountPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete account: %w", err)
	}
	return nil
}

// LoadState reads the routing state; a missing file yields the zero state.
func (s *Store) LoadState() (State, error) {
	raw, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return State{Active: map[string]string{}, Exhausted: map[string]int64{}}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if json.Unmarshal(raw, &state) != nil {
		// Legacy shape: active was a single string that belonged to codex.
		state = State{Active: map[string]string{}, Exhausted: map[string]int64{}}
		var legacy struct {
			Active    json.RawMessage  `json:"active"`
			Exhausted map[string]int64 `json:"exhausted"`
		}
		if json.Unmarshal(raw, &legacy) == nil {
			state.Exhausted = legacy.Exhausted
			var old string
			if json.Unmarshal(legacy.Active, &old) == nil && old != "" {
				state.Active["codex"] = old
			}
		}
	}
	if state.Exhausted == nil {
		state.Exhausted = map[string]int64{}
	}
	if state.Active == nil {
		state.Active = map[string]string{}
	}
	return state, nil
}

// SaveState writes the routing state atomically.
func (s *Store) SaveState(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := writeAtomic(s.statePath, raw, 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

func (s *Store) accountPath(id string) string {
	return filepath.Join(s.accountsDir, id+".json")
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	// Sync before rename so a crash never leaves a truncated file behind.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", filepath.Base(path), err)
	}
	return nil
}

// invalidateList drops the decoded account cache so the next List re-reads.
func (s *Store) invalidateList() {
	s.listMu.Lock()
	s.listMtime = 0
	s.listFilesNewest = 0
	s.listMu.Unlock()
}
