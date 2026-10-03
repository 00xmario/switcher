package desktoprelay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	b, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 4<<20 {
		return nil, errors.New("store file too large")
	}
	return b, nil
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
	if !uniqueJSON(b) {
		return s, false, errors.New("ambiguous persisted state")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&s); err != nil {
		return s, false, errors.New("corrupt relay state")
	}
	if d.Decode(new(any)) != io.EOF || s.Version != 1 || s.Scopes == nil || s.Sessions == nil {
		return s, false, errors.New("unsupported relay state")
	}
	if len(s.Scopes) > 128 || len(s.Sessions)+len(s.ConversationBindings) > 4096 {
		return s, false, errors.New("relay state exceeds limits")
	}
	if !validSetupRecords(s, root.dir.Name()) {
		return s, false, errors.New("corrupt relay setup journal")
	}
	for id, scope := range s.Scopes {
		secret, err := hex.DecodeString(scope.Secret)
		if !validUUID(id) || scope.ID != id || err != nil || len(secret) != 32 || len(scope.Label) > 256 {
			return s, false, errors.New("corrupt relay scope")
		}
	}
	conversationMembers := make(map[string]int)
	for key, session := range s.Sessions {
		if key != taskKey(session.ScopeID, session.SessionID) || !validUUID(session.SessionID) || session.Revision == 0 || len(session.AccountID) > 256 || len(session.AgentID) > 128 || len(session.Model) > 256 || (session.ParentSessionID != "" && !validUUID(session.ParentSessionID)) {
			return s, false, errors.New("corrupt relay session")
		}
		if _, ok := s.Scopes[session.ScopeID]; !ok {
			return s, false, errors.New("orphan relay session")
		}
		if session.ConversationID != "" && !validUUID(session.ConversationID) {
			return s, false, errors.New("corrupt relay conversation identity")
		}
		if !validEvidence(session.LastResponse) {
			return s, false, errors.New("corrupt relay response evidence")
		}
		if session.ConversationID != "" {
			groupKey := taskKey(session.ScopeID, session.ConversationID)
			conversationMembers[groupKey]++
			if binding := s.ConversationBindings[groupKey]; binding.AccountID != "" && session.AccountID != binding.AccountID {
				return s, false, errors.New("inconsistent relay conversation selection")
			}
		}
		session.InFlight = 0
		s.Sessions[key] = session
	}
	for key, binding := range s.ConversationBindings {
		if key != taskKey(binding.ScopeID, binding.ConversationID) || !validUUID(binding.ConversationID) || binding.Revision == 0 || len(binding.AccountID) > 256 {
			return s, false, errors.New("corrupt relay conversation binding")
		}
		if _, ok := s.Scopes[binding.ScopeID]; !ok {
			return s, false, errors.New("orphan relay conversation binding")
		}
		if conversationMembers[key] == 0 {
			return s, false, errors.New("relay conversation binding has no observed members")
		}
	}
	return s, true, nil
}

func (m *Manager) saveLocked() error {
	if m.store == nil || m.failed {
		return ErrUnavailable
	}
	b, err := json.Marshal(m.state)
	if len(b) > 4<<20 {
		return ErrBusy
	}
	if err == nil {
		err = atomicPrivate(m.store, "state.json", b)
	}
	if err != nil {
		if errors.Is(err, errCommitUncertain) {
			m.failed = true
			m.condition = "store_error"
			if m.run != nil {
				m.run.cancel()
			}
			return errors.Join(ErrUnavailable, errCommitUncertain)
		}
		return fmt.Errorf("%w: could not persist state", ErrUnavailable)
	}
	return nil
}

func uniqueJSON(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		t, err := d.Token()
		if err != nil {
			return false
		}
		switch t {
		case json.Delim('{'):
			seen := make(map[string]bool)
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return false
				}
				seen[k] = true
				if !walk(depth + 1) {
					return false
				}
			}
			t, err = d.Token()
			return err == nil && t == json.Delim('}')
		case json.Delim('['):
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			t, err = d.Token()
			return err == nil && t == json.Delim(']')
		default:
			_, delim := t.(json.Delim)
			return !delim
		}
	}
	return walk(0) && d.Decode(new(any)) == io.EOF
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
