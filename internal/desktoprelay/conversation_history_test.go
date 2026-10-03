package desktoprelay_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

type historicalResolverFuncs struct {
	base       resolverFunc
	historical func(context.Context, []string, map[string]string) (map[string]string, error)
}

func (f historicalResolverFuncs) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	return f.base(ctx, ids)
}
func (f historicalResolverFuncs) ResolveHistorical(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
	return f.historical(ctx, ids, known)
}

func (f historicalResolverFuncs) ResolveCached(ids []string, known map[string]string) map[string]string {
	result, _ := f.historical(context.Background(), ids, known)
	return result
}

var _ desktoprelay.HistoricalConversationResolver = historicalResolverFuncs{}

func TestHistoricalProofKeepsPreviouslyVerifiedAliasesSelectableAfterRotation(t *testing.T) {
	cfg := fixtureConfig(t)
	var rotated, historicalCalls, preparations atomic.Int32
	base := resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		for _, id := range ids {
			if id == sessionA && rotated.Load() == 0 || id == sessionB && rotated.Load() == 1 {
				out[id] = conversationA
			}
			if id == sessionD {
				out[id] = conversationB
			}
		}
		return out, nil
	})
	cfg.Conversations = historicalResolverFuncs{base: base, historical: func(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
		historicalCalls.Add(1)
		for id, g := range known {
			if id == sessionC || id == sessionE || id == sessionA && g != conversationA || id == sessionB && g != conversationA || id == sessionD && g != conversationB {
				t.Errorf("history received unproved or foreign association %s -> %s", id, g)
			}
		}
		out, err := base(ctx, ids)
		for _, id := range ids {
			// The immutable native G remains valid. Only previously persisted
			// proof can retain A after the current metadata rotates to B.
			if id == sessionA && rotated.Load() == 1 && known[id] == conversationA {
				out[id] = conversationA
			}
		}
		return out, err
	}}
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		preparations.Add(1)
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	tokens := make(chan string, 16)
	cfg.Transport = physicalProvider(t, func(w http.ResponseWriter, r *http.Request) {
		tokens <- r.Header.Get("Authorization")
		w.WriteHeader(200)
	})
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	request := func(id, want string) {
		t.Helper()
		if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatal(err)
		}
		r := send(t, c, br, "/v1/messages", `{"model":"same-model","title":"same title","conversation_id":"`+conversationA+`"}`, id, nil)
		drain(t, r)
		if got := <-tokens; r.StatusCode != 200 || got != want {
			t.Fatalf("rotated alias %s: HTTP %d credential %q", id, r.StatusCode, got)
		}
	}
	request(sessionA, "Bearer caller-token")
	request(sessionC, "Bearer caller-token") // Retired legacy alias never proved G.
	request(sessionD, "Bearer caller-token") // Equal model/title, separate native G.
	// An empty response can reach the client before the handler's deferred
	// release. Wait for runtime quiescence before testing read-only state equality.
	quiescent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for m.Status().InFlight != 0 {
		select {
		case <-tick.C:
		case <-quiescent.Done():
			t.Fatal("fixture requests did not release before the verification baseline")
		}
	}
	group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	rotated.Store(1)
	before, bindings := m.Sessions(), m.ConversationBindings()
	statePath := filepath.Join(cfg.DataRoot, "state.json")
	beforeDisk, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparations.Load()
	proofs, err := m.VerifiedConversationAssociations(context.Background())
	if err != nil || !reflect.DeepEqual(proofs, map[string]string{scope.ID + "/" + sessionA: conversationA, scope.ID + "/" + sessionD: conversationB}) {
		t.Fatalf("read-only historical view %v %v", proofs, err)
	}
	afterDisk, err := os.ReadFile(statePath)
	if err != nil || string(afterDisk) != string(beforeDisk) || !reflect.DeepEqual(m.Sessions(), before) || !reflect.DeepEqual(m.ConversationBindings(), bindings) || preparations.Load() != prepared {
		t.Fatal("verification read mutated state or acquired credentials")
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", group.Revision, memberRevisions(m, scope.ID, conversationA)); err != nil {
		t.Fatalf("retired proved alias froze grouped controls: %v", err)
	}
	request(sessionA, "Bearer token-B")
	request(sessionB, "Bearer token-B") // Fresh Y inherits the active group.
	request(sessionC, "Bearer caller-token")
	request(sessionD, "Bearer caller-token")
	if historicalCalls.Load() == 0 {
		t.Fatal("optional historical resolver was not used")
	}
	if retired := observed(t, m, scope.ID, sessionC); retired.ConversationID != "" || retired.AccountID != "" {
		t.Fatalf("history retroactively promoted an unproved retired alias %+v", retired)
	}
	if evidence := observed(t, m, scope.ID, sessionA).LastResponse; evidence == nil || evidence.Route != "selected" || evidence.AccountID != "B" || evidence.Status != 200 {
		t.Fatalf("retired selected route lost actual response proof %+v", evidence)
	}
}

func TestHistoricalHintsRespectScopeOwnershipAndOmitConflictingSavedAliases(t *testing.T) {
	for _, peerKind := range []string{"unproved peer", "conflicting peer", "matching proved peer"} {
		t.Run(peerKind, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var phase atomic.Int32
			base := resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				out := make(map[string]string)
				if phase.Load() == 2 || phase.Load() == 1 && peerKind == "unproved peer" {
					return out, nil
				}
				conversation := conversationA
				if phase.Load() == 1 && peerKind == "conflicting peer" {
					conversation = conversationB
				}
				for _, id := range ids {
					out[id] = conversation
				}
				return out, nil
			})
			cfg.Conversations = historicalResolverFuncs{base: base, historical: func(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
				out, err := base(ctx, ids)
				if phase.Load() == 2 {
					for _, id := range ids {
						if known[id] == conversationA {
							out[id] = conversationA
						}
					}
				}
				if phase.Load() == 2 && peerKind == "conflicting peer" && len(known) != 0 {
					t.Errorf("conflicting scope claims supplied a historical hint: %v", known)
				}
				return out, err
			}}
			cfg.Transport = transportFunc(echoCredential)
			m, own := startFixture(t, cfg)
			c, br := tunnel(t, own)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			peer, err := m.CreateScope("same alias, independent scope")
			if err != nil {
				t.Fatal(err)
			}
			pc, pbr := tunnel(t, peer)
			phase.Store(1)
			drain(t, send(t, pc, pbr, "/v1/messages/count_tokens", `{"title":"same title","conversation_id":"`+conversationA+`"}`, sessionA, nil))
			phase.Store(2)
			proofs, err := m.VerifiedConversationAssociations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			if peerKind != "conflicting peer" {
				want[own.ID+"/"+sessionA] = conversationA
			}
			if peerKind == "matching proved peer" {
				want[peer.ID+"/"+sessionA] = conversationA
			}
			if !reflect.DeepEqual(proofs, want) {
				t.Fatalf("history crossed scope ownership: %v, want %v", proofs, want)
			}
			if peerKind == "unproved peer" {
				r := send(t, pc, pbr, "/v1/messages/count_tokens", `{}`, sessionA, nil)
				drain(t, r)
				if r.StatusCode != 200 || observed(t, m, peer.ID, sessionA).ConversationID != "" {
					t.Fatal("retired unproved peer alias was promoted by another scope's history")
				}
			}
		})
	}
}

func TestHistoricalSourceVetoAndBaseOnlyResolversKeepRetiredAliasesFailClosed(t *testing.T) {
	for _, reason := range []string{"native removed", "veto", "truncated", "immutable identity changed", "bridge only", "current different conversation", "source failure", "invalid output", "base only"} {
		t.Run(reason, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var retired atomic.Bool
			var sends atomic.Int32
			base := resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				out := make(map[string]string)
				for _, id := range ids {
					if !retired.Load() {
						out[id] = conversationA
					} else if reason == "current different conversation" {
						out[id] = conversationB
					}
				}
				return out, nil
			})
			cfg.Conversations = historicalResolverFuncs{base: base, historical: func(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
				if retired.Load() {
					if known[sessionA] != conversationA {
						t.Errorf("missing core-persisted historical hint %v", known)
					}
					if reason == "source failure" {
						return nil, errors.New("Bearer fixture-private-history-secret")
					}
					if reason == "invalid output" {
						return map[string]string{sessionA: "local_" + conversationA}, nil
					}
				}
				// Negative native evidence or a current conflicting claim permits
				// no historical fill, even when the core offers prior proof.
				return base(ctx, ids)
			}}
			if reason == "base only" {
				cfg.Conversations = base
			}
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) { sends.Add(1); return echoCredential(r) })
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages", `{"model":"original-model"}`, sessionA, nil))
			group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			before := observed(t, m, scope.ID, sessionA)
			retired.Store(true)
			proofs, err := m.VerifiedConversationAssociations(context.Background())
			if reason == "source failure" || reason == "invalid output" {
				var association *desktoprelay.AssociationError
				if !errors.As(err, &association) || !errors.Is(err, desktoprelay.ErrUnavailable) {
					t.Fatalf("unsanitized historical source failure %v", err)
				}
				for chain := err; chain != nil; chain = errors.Unwrap(chain) {
					if strings.Contains(chain.Error(), "fixture-private-history-secret") {
						t.Fatal("historical source error chain leaked")
					}
				}
			} else if err != nil || len(proofs) != 0 {
				t.Fatalf("vetoed or absent proof returned an association %v %v", proofs, err)
			}
			r := send(t, c, br, "/v1/messages", `{"model":"blocked-model"}`, sessionA, nil)
			body := drain(t, r)
			if r.StatusCode != 503 || sends.Load() != 1 || strings.Contains(body, "fixture-private-history-secret") {
				t.Fatalf("retired unproved alias dispatched: HTTP %d sends %d", r.StatusCode, sends.Load())
			}
			if !reflect.DeepEqual(observed(t, m, scope.ID, sessionA), before) {
				t.Fatal("unproved history changed saved selection or response evidence")
			}
			if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", group.Revision, memberRevisions(m, scope.ID, conversationA)); err == nil {
				t.Fatal("historical veto widened grouped selection")
			}
		})
	}
}

func TestHistoricalVerificationReadIsLockFreeAndCancellationPreservesState(t *testing.T) {
	cfg := fixtureConfig(t)
	var pause atomic.Bool
	entered := make(chan struct{})
	base := metadataFixture(map[string]string{sessionA: conversationA})
	cfg.Conversations = historicalResolverFuncs{base: resolverFunc(base.Resolve), historical: func(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
		if pause.Load() {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return base.Resolve(ctx, ids)
	}}
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	before := m.Sessions()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pause.Store(true)
	result := make(chan error, 1)
	go func() { _, err := m.VerifiedConversationAssociations(ctx); result <- err }()
	await(t, entered)
	read := make(chan struct{})
	go func() { m.Sessions(); m.Scopes(); m.Status(); close(read) }()
	await(t, read)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("historical read ignored cancellation %v", err)
	}
	if !reflect.DeepEqual(m.Sessions(), before) {
		t.Fatal("cancelled verification read mutated observations")
	}
}

func TestVerificationReadDoesNotAdoptFreshProofAndOmitsRevokedScopes(t *testing.T) {
	cfg := fixtureConfig(t)
	var fresh, pause atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	base := resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		if fresh.Load() {
			for _, id := range ids {
				out[id] = conversationA
			}
		}
		return out, nil
	})
	cfg.Conversations = historicalResolverFuncs{base: base, historical: func(ctx context.Context, ids []string, known map[string]string) (map[string]string, error) {
		if len(known) != 0 {
			t.Errorf("GET supplied or persisted unproved history %v", known)
		}
		if pause.Load() {
			close(entered)
			<-release
		}
		return base(ctx, ids)
	}}
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{"conversation_id":"`+conversationA+`"}`, sessionA, nil))
	before := m.Sessions()
	fresh.Store(true)
	proofs, err := m.VerifiedConversationAssociations(context.Background())
	if err != nil || proofs[scope.ID+"/"+sessionA] != conversationA || !reflect.DeepEqual(m.Sessions(), before) {
		t.Fatalf("fresh GET proof mutated observations: %v %v", proofs, err)
	}
	pause.Store(true)
	result := make(chan map[string]string, 1)
	go func() {
		proofs, err := m.VerifiedConversationAssociations(context.Background())
		if err != nil {
			t.Errorf("revoked proof read %v", err)
		}
		result <- proofs
	}()
	await(t, entered)
	if err := m.DeleteScope(scope.ID); err != nil {
		t.Fatal(err)
	}
	unblock()
	if proofs := <-result; len(proofs) != 0 {
		t.Fatalf("read returned revoked scope aliases %v", proofs)
	}
	proofs, err = m.VerifiedConversationAssociations(context.Background())
	if err != nil || len(proofs) != 0 {
		t.Fatalf("revocation retained verification hints %v %v", proofs, err)
	}
}
