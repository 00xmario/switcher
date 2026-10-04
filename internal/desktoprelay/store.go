package desktoprelay

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type storedScope struct {
	Scope
	Secret string `json:"secret"`
}

type diskState struct {
	Version              int                            `json:"version"`
	Enabled              bool                           `json:"enabled"`
	Certificate          string                         `json:"certificate"`
	PrivateKey           string                         `json:"private_key"`
	Scopes               map[string]storedScope         `json:"scopes"`
	Sessions             map[string]Session             `json:"sessions"`
	ConversationBindings map[string]ConversationBinding `json:"conversation_bindings,omitempty"`
	Setups               map[string]setupRecord         `json:"setups,omitempty"`
}

type privateStore struct {
	dir           *os.File
	syncDirectory func(*os.File) error
}

func (s *privateStore) Close() error             { return s.dir.Close() }
func (s *privateStore) Remove(name string) error { return unix.Unlinkat(int(s.dir.Fd()), name, 0) }
func (s *privateStore) Rename(old, new string) error {
	return unix.Renameat(int(s.dir.Fd()), old, int(s.dir.Fd()), new)
}
func (s *privateStore) OpenFile(name string, flags int, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(s.dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, uint32(mode))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Every component is opened relative to a pinned descriptor with O_NOFOLLOW.
// No pathname validation/open gap can substitute a symlink into the traversal.
func openStore(path string, create bool) (*privateStore, error) {
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return nil, errors.New("invalid store path")
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && create && i == len(parts)-1 {
			if err = unix.Mkdirat(fd, part, 0700); err == nil || err == unix.EEXIST {
				if err = unix.Fsync(fd); err == nil {
					next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				}
			}
		}
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Mode&0777 != 0700 || stat.Uid != uint32(os.Getuid()) {
		f.Close()
		return nil, errors.New("unsafe store ownership or mode")
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrBusy
	}
	return &privateStore{dir: f}, nil
}

func readPrivate(root *privateStore, name string) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 {
		return nil, errors.New("unsafe store file")
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		return nil, errors.New("unsafe store file ownership or links")
	}
	return io.ReadAll(f)
}

var errCommitUncertain = errors.New("relay state commit durability uncertain")

func atomicPrivate(root *privateStore, name string, data []byte) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(root.dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			return errors.New("unsafe destination file")
		}
	} else if err != unix.ENOENT {
		return err
	}
	token, err := randomHex(16)
	if err != nil {
		return err
	}
	tmp := ".write-" + token
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = root.Rename(tmp, name); err != nil {
		return err
	}
	if root.syncDirectory != nil {
		err = root.syncDirectory(root.dir)
	} else {
		err = root.dir.Sync()
	}
	if err != nil {
		return errCommitUncertain
	}
	return nil
}

func loadState(root *privateStore) (diskState, bool, error) {
	b, err := readPrivate(root, "state.json")
	if os.IsNotExist(err) {
		return diskState{}, false, nil
	}
	if err != nil {
		return diskState{}, false, err
	}
	var s diskState
	if err = json.Unmarshal(b, &s); err != nil || s.Version != 1 {
		return s, false, errors.New("unsupported relay state")
	}
	if !validSetupRecords(s, root.dir.Name()) {
		return s, false, errors.New("corrupt relay setup journal")
	}
	if s.Scopes == nil {
		s.Scopes = map[string]storedScope{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]Session{}
	}
	pruneSessions(&s, time.Now())
	// Drop records that no longer belong to a scope instead of refusing to start.
	for key, session := range s.Sessions {
		if _, ok := s.Scopes[session.ScopeID]; !ok || key != taskKey(session.ScopeID, session.SessionID) {
			delete(s.Sessions, key)
			continue
		}
		session.InFlight = 0
		s.Sessions[key] = session
	}
	for key, binding := range s.ConversationBindings {
		if _, ok := s.Scopes[binding.ScopeID]; !ok || key != taskKey(binding.ScopeID, binding.ConversationID) {
			delete(s.ConversationBindings, key)
		}
	}
	return s, true, nil
}

// Sessions are request telemetry. Idle ones are forgotten after two weeks and
// at most maxSessions are kept, so the state file stays small.
const (
	maxSessions = 2000
	sessionTTL  = 14 * 24 * time.Hour
)

func pruneSessions(s *diskState, now time.Time) {
	if len(s.Sessions) == 0 {
		return
	}
	keys := make([]string, 0, len(s.Sessions))
	for key, session := range s.Sessions {
		if session.InFlight == 0 && now.Sub(session.LastSeen) > sessionTTL {
			delete(s.Sessions, key)
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) <= maxSessions {
		return
	}
	sort.Slice(keys, func(i, j int) bool { return s.Sessions[keys[i]].LastSeen.After(s.Sessions[keys[j]].LastSeen) })
	for _, key := range keys[maxSessions:] {
		if s.Sessions[key].InFlight == 0 {
			delete(s.Sessions, key)
		}
	}
}

// saveLocked persists the state now. The caller holds m.mu.
func (m *Manager) saveLocked() error {
	if m.store == nil {
		return ErrUnavailable
	}
	b, seq, err := m.snapshotLocked()
	if err == nil {
		err = m.write(m.store, b, seq)
	}
	if err != nil {
		return fmt.Errorf("%w: could not persist state", ErrUnavailable)
	}
	return nil
}

func (m *Manager) snapshotLocked() ([]byte, uint64, error) {
	pruneSessions(&m.state, time.Now())
	for key := range m.threads {
		if _, ok := m.state.Sessions[key]; !ok {
			delete(m.threads, key)
		}
	}
	m.saveSeq++
	b, err := json.Marshal(m.state)
	return b, m.saveSeq, err
}

func (m *Manager) write(store *privateStore, b []byte, seq uint64) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	if seq < m.savedSeq {
		return nil
	}
	if err := atomicPrivate(store, "state.json", b); err != nil {
		return err
	}
	m.savedSeq = seq
	return nil
}

// saveSoonLocked batches persistence of request observations. The write runs
// outside m.mu, so requests never wait for, or fail because of, the disk.
func (m *Manager) saveSoonLocked() {
	if m.savePending {
		return
	}
	m.savePending = true
	time.AfterFunc(2*time.Second, func() {
		m.mu.Lock()
		m.savePending = false
		store := m.store
		if store == nil {
			m.mu.Unlock()
			return
		}
		b, seq, err := m.snapshotLocked()
		m.mu.Unlock()
		if err == nil {
			err = m.write(store, b, seq)
		}
		if err != nil {
			log.Printf("desktop relay: save state: %v", err)
		}
	})
}

func rollbackAllowed(err error) bool { return !errors.Is(err, errCommitUncertain) }

func taskKey(scope, session string) string { return scope + "/" + session }

func validUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	zero := true
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
		if c != '0' {
			zero = false
		}
	}
	return !zero
}
