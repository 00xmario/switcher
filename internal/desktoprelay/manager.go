package desktoprelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Manager struct {
	cfg           Config
	lifecycle     sync.Mutex
	mu            sync.Mutex
	store         *privateStore
	state         diskState
	run           *runtime
	condition     string
	threads       map[string]string
	remote        atomic.Pointer[RemoteInference]
	hostOnce      sync.Once
	hostTransport http.RoundTripper
	savePending   bool
	// saveMu orders state.json writes; saveSeq numbers marshalled snapshots
	// so a slower background write never replaces a newer one.
	saveMu      sync.Mutex
	saveSeq     uint64
	savedSeq    uint64
	associating atomic.Bool
	associated  time.Time
}

type runtime struct {
	listener     net.Listener
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	server       *http.Server
	transport    http.RoundTripper
	plain        http.RoundTripper // plain proxy requests to other destinations
	mu           sync.Mutex
	closed       bool
	conns        map[net.Conn]struct{}
	wg           sync.WaitGroup
	shutdownOnce sync.Once
	finished     chan struct{}
	inFlight     atomic.Int64
}

// track registers a hijacked connection so shutdown can close it.
func (r *runtime) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.conns[c] = struct{}{}
	r.wg.Add(1)
	return true
}

func (r *runtime) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
	r.wg.Done()
}

func New(cfg Config) (*Manager, error) {
	if cfg.DataRoot == "" || !filepath.IsAbs(cfg.DataRoot) || cfg.Port < 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("%w: invalid configuration", ErrUnavailable)
	}
	cfg.DataRoot = filepath.Clean(cfg.DataRoot)
	if cfg.FixtureTLSRoots != nil {
		cfg.FixtureTLSRoots = cfg.FixtureTLSRoots.Clone()
	}
	return &Manager{cfg: cfg, condition: "disabled"}, nil
}

func (m *Manager) initializeLocked(create bool) error {
	if m.store != nil {
		return nil
	}
	root, err := openStore(m.cfg.DataRoot, create)
	if os.IsNotExist(err) && !create {
		return nil
	}
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return ErrBusy
		}
		return fmt.Errorf("%w: unsafe or inaccessible store", ErrUnavailable)
	}
	root.syncDirectory = m.cfg.SyncDirectory
	s, exists, err := loadState(root)
	if err != nil {
		root.Close()
		return fmt.Errorf("%w: invalid persisted state", ErrUnavailable)
	}
	if !exists {
		if !create {
			root.Close()
			return nil
		}
		cert, key, err := newCA()
		if err != nil {
			root.Close()
			return ErrUnavailable
		}
		s = diskState{Version: 1, Certificate: cert, PrivateKey: key, Scopes: map[string]storedScope{}, Sessions: map[string]Session{}}
	}
	if _, err := leafCertificate(s.Certificate, s.PrivateKey); err != nil {
		root.Close()
		return ErrUnavailable
	}
	// A missing CA export can be repaired from the single atomic state record.
	if b, err := readPrivate(root, "ca.pem"); err == nil && string(b) != s.Certificate {
		root.Close()
		return ErrUnavailable
	} else if err != nil && !os.IsNotExist(err) {
		root.Close()
		return ErrUnavailable
	}
	m.store, m.state = root, s
	if !exists {
		if err := m.saveLocked(); err != nil {
			root.Close()
			m.store = nil
			return err
		}
	}
	return nil
}

// SetRemote sends Claude inference to a connected Switcher host; nil restores
// local accounts.
func (m *Manager) SetRemote(fn RemoteInference) {
	if fn == nil {
		m.remote.Store(nil)
		return
	}
	m.remote.Store(&fn)
}

func (m *Manager) remoteInference() RemoteInference {
	if fn := m.remote.Load(); fn != nil {
		return *fn
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) error  { return m.start(ctx, false) }
func (m *Manager) Resume(ctx context.Context) error { return m.start(ctx, true) }

func (m *Manager) start(ctx context.Context, resume bool) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	return m.startLifecycleLocked(ctx, resume)
}

func (m *Manager) startLifecycleLocked(ctx context.Context, resume bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.run != nil {
		if m.run.ctx.Err() != nil {
			select {
			case <-m.run.finished:
				if err := m.finishShutdownLocked(m.run); err != nil {
					return err
				}
			default:
				return ErrBusy
			}
		} else {
			return nil
		}
	}
	if err := m.initializeLocked(!resume); err != nil {
		m.condition = "store_error"
		return err
	}
	if resume && (m.store == nil || !m.state.Enabled) {
		return nil
	}
	l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(m.cfg.Port))
	if err != nil {
		m.condition = "listen_error"
		return fmt.Errorf("%w: could not listen", ErrUnavailable)
	}
	old := m.state.Enabled
	m.state.Enabled = true
	if err = m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			m.state.Enabled = old
		}
		l.Close()
		return err
	}
	if err = atomicPrivate(m.store, "ca.pem", []byte(m.state.Certificate)); err != nil {
		l.Close()
		m.condition = "store_error"
		return ErrUnavailable
	}
	runCtx, cancel := context.WithCancel(context.Background())
	r := &runtime{listener: l, ctx: runCtx, cancel: cancel, done: make(chan struct{}), finished: make(chan struct{}), conns: make(map[net.Conn]struct{})}
	r.transport = m.transport()
	r.plain = &http.Transport{Proxy: nil, DialContext: m.dial, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second}
	r.server = ingressServer(m, r)
	m.run, m.condition = r, "listening"
	go func() {
		defer close(r.done)
		r.server.Serve(l)
	}()
	return nil
}

func (m *Manager) Stop(ctx context.Context) error  { return m.stop(ctx, true) }
func (m *Manager) Close(ctx context.Context) error { return m.stop(ctx, false) }

func (m *Manager) stop(ctx context.Context, disable bool) error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	var persistErr error
	if disable {
		if err := m.initializeLocked(false); err != nil {
			m.mu.Unlock()
			return err
		}
		if hasManagedSetup(m.state) {
			m.mu.Unlock()
			return setupError("setup_owned", ErrConflict)
		}
		if m.store != nil {
			old := m.state.Enabled
			m.state.Enabled = false
			persistErr = m.saveLocked()
			if persistErr != nil && rollbackAllowed(persistErr) {
				m.state.Enabled = old
			}
		}
	}
	r := m.run
	if r != nil {
		m.condition = "stopping"
	}
	m.mu.Unlock()
	if r != nil {
		r.shutdownOnce.Do(func() {
			r.cancel()
			r.mu.Lock()
			r.closed = true
			for c := range r.conns {
				c.Close()
			}
			r.mu.Unlock()
			r.server.Close()
			go func() { r.wg.Wait(); <-r.done; close(r.finished) }()
		})
		select {
		case <-r.finished:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return ErrBusy
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.finishShutdownLocked(r); persistErr == nil {
		persistErr = err
	}
	if persistErr != nil {
		m.condition = "store_error"
	}
	return persistErr
}

// The lifecycle and state locks serialize reclamation with new startup and
// binding mutations. Only a completed runtime can release its store ownership.
func (m *Manager) finishShutdownLocked(r *runtime) error {
	if r != nil {
		for _, transport := range []http.RoundTripper{r.transport, r.plain} {
			if t, ok := transport.(interface{ CloseIdleConnections() }); ok {
				t.CloseIdleConnections()
			}
		}
	}
	var persistErr error
	m.run = nil
	if m.store != nil {
		if err := m.saveLocked(); err != nil && persistErr == nil {
			persistErr = err
		}
		m.store.Close()
		m.store = nil
	}
	if m.state.Enabled {
		m.condition = "stopped"
	} else {
		m.condition = "disabled"
	}
	if persistErr != nil {
		m.condition = "store_error"
	}
	return persistErr
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Enabled: m.state.Enabled, Condition: m.condition, Validation: "fixture_tested"}
	if m.state.Certificate != "" {
		s.CAPath = filepath.Join(m.cfg.DataRoot, "ca.pem")
	}
	if m.run != nil {
		s.InFlight = uint64(m.run.inFlight.Load())
		if m.run.ctx.Err() == nil {
			s.Listening = true
			s.Address = m.run.listener.Addr().String()
		}
	}
	return s
}

func (m *Manager) CreateScope(label string) (ScopeSetup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run == nil || m.run.ctx.Err() != nil {
		return ScopeSetup{}, ErrUnavailable
	}
	if len(label) > 256 || len(m.state.Scopes) >= 128 {
		return ScopeSetup{}, ErrBusy
	}
	id, err := randomID()
	if err != nil {
		return ScopeSetup{}, ErrUnavailable
	}
	secret, err := randomHex(32)
	if err != nil {
		return ScopeSetup{}, ErrUnavailable
	}
	s := storedScope{Scope: Scope{ID: id, Label: label}, Secret: secret}
	m.state.Scopes[id] = s
	if err := m.saveLocked(); err != nil {
		if rollbackAllowed(err) {
			delete(m.state.Scopes, id)
		}
		return ScopeSetup{}, err
	}
	u := &url.URL{Scheme: "http", Host: m.run.listener.Addr().String(), User: url.UserPassword(id, secret)}
	ca := filepath.Join(m.cfg.DataRoot, "ca.pem")
	return ScopeSetup{Scope: s.Scope, ProxyURL: u.String(), CAPath: ca, Env: map[string]string{"HTTPS_PROXY": u.String(), "NODE_EXTRA_CA_CERTS": ca}}, nil
}

func (m *Manager) Scopes() []Scope {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Scope, 0, len(m.state.Scopes))
	for _, s := range m.state.Scopes {
		out = append(out, s.Scope)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Sessions() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.state.Sessions))
	for _, s := range m.state.Sessions {
		out = append(out, publicSession(s))
	}
	sort.Slice(out, func(i, j int) bool {
		return taskKey(out[i].ScopeID, out[i].SessionID) < taskKey(out[j].ScopeID, out[j].SessionID)
	})
	return out
}
