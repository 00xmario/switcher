package desktoprelay_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func echoCredential(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.Header.Get("Authorization")))}, nil
}

func TestConversationCancellationAfterMetadataRecheckCannotPublishBehindDurableWrite(t *testing.T) {
	cfg := fixtureConfig(t)
	prepared, releasePrep, checked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
	var prepOnce, writeOnce sync.Once
	defer prepOnce.Do(func() { close(releasePrep) })
	defer writeOnce.Do(func() { close(releaseWrite) })
	var holdWrite, afterPrep atomic.Bool
	cfg.SyncDirectory = func(f *os.File) error {
		if holdWrite.CompareAndSwap(true, false) {
			close(writeEntered)
			<-releaseWrite
		}
		return f.Sync()
	}
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		close(prepared)
		<-releasePrep
		afterPrep.Store(true)
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		for _, id := range ids {
			out[id] = conversationA
		}
		if afterPrep.Load() {
			close(checked)
		}
		return out, nil
	})
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	expected := memberRevisions(m, scope.ID, conversationA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := m.BindConversation(ctx, scope.ID, conversationA, "B", 0, expected); result <- err }()
	await(t, prepared)
	holdWrite.Store(true)
	wrote := make(chan error, 1)
	go func() { _, err := m.CreateScope("durable write contention"); wrote <- err }()
	await(t, writeEntered)
	prepOnce.Do(func() { close(releasePrep) })
	// Resolver completion while the write owns m.mu proves the second lookup
	// also runs outside the mutation lock.
	await(t, checked)
	select {
	case err := <-result:
		t.Fatalf("group escaped durable lock %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	writeOnce.Do(func() { close(releaseWrite) })
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled grouped publication %v", err)
	}
	if len(m.ConversationBindings()) != 0 || observed(t, m, scope.ID, sessionA).AccountID != "" {
		t.Fatal("cancelled group changed persistent selection")
	}
}

func TestConversationCommitUncertaintyKeepsAllMembersAndGroupInOneRecord(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA, sessionB: conversationA})
	cfg.Transport = transportFunc(echoCredential)
	var fail atomic.Bool
	cfg.SyncDirectory = func(f *os.File) error {
		if fail.CompareAndSwap(true, false) {
			return errors.New("fixture fsync failure")
		}
		return f.Sync()
	}
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	for _, id := range []string{sessionA, sessionB} {
		drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
	}
	fail.Store(true)
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); !errors.Is(err, desktoprelay.ErrUnavailable) {
		t.Fatalf("uncertain commit acknowledged %v", err)
	}
	if bindings := m.ConversationBindings(); len(bindings) != 1 || bindings[0].AccountID != "B" || bindings[0].Revision != 1 {
		t.Fatalf("committed group rolled back %+v", bindings)
	}
	for _, s := range m.Sessions() {
		if s.AccountID != "B" {
			t.Fatalf("partial committed member %+v", s)
		}
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close(context.Background()) })
	if err := other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, s := range other.Sessions() {
		if s.AccountID != "B" {
			t.Fatalf("disk lost atomic member update %+v", s)
		}
	}
	if bindings := other.ConversationBindings(); len(bindings) != 1 || bindings[0].AccountID != "B" {
		t.Fatalf("disk lost grouped selection %+v", bindings)
	}
}

func TestConversationRevisionCASFencesPreparedRequestsEvenWhenAccountDoesNotChange(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
	entered, release := make(chan struct{}), make(chan struct{})
	var block atomic.Bool
	cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
		if block.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
	}}
	var sends atomic.Int32
	cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) { sends.Add(1); return echoCredential(r) })
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	before := observed(t, m, scope.ID, sessionA)
	block.Store(true)
	result := make(chan int, 1)
	go func() { r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil); drain(t, r); result <- r.StatusCode }()
	await(t, entered)
	next, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", group.Revision, memberRevisions(m, scope.ID, conversationA))
	if err != nil || next.Revision != 2 || observed(t, m, scope.ID, sessionA).Revision != before.Revision {
		t.Fatalf("same-account group revision %+v %v", next, err)
	}
	close(release)
	if status := <-result; status != 409 || sends.Load() != 1 || observed(t, m, scope.ID, sessionA).LastResponse != nil {
		t.Fatalf("stale group request dispatched: HTTP %d sends %d", status, sends.Load())
	}
}

func TestTrustedResolverFailuresDoNotChangeVerifiedAssociationsOrLeakSourceErrors(t *testing.T) {
	for _, failure := range []string{"unavailable", "missing known alias", "changed known alias", "local prefix", "noncanonical UUID", "unsolicited alias"} {
		t.Run(failure, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var broken atomic.Bool
			var lookups, sends atomic.Int32
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				lookups.Add(1)
				out := make(map[string]string)
				for _, id := range ids {
					out[id] = conversationA
				}
				if broken.Load() {
					switch failure {
					case "unavailable":
						return nil, errors.New("Bearer fixture-source-secret")
					case "missing known alias":
						return map[string]string{}, nil
					case "changed known alias":
						out[sessionA] = conversationB
					case "local prefix":
						out[sessionA] = "local_" + conversationA
					case "noncanonical UUID":
						out[sessionA] = strings.ToUpper(conversationA)
					case "unsolicited alias":
						out[sessionE] = conversationA
					}
				}
				return out, nil
			})
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) { sends.Add(1); return echoCredential(r) })
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil))
			binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			before := observed(t, m, scope.ID, sessionA)
			broken.Store(true)
			lookupCount := lookups.Load()
			m.Sessions()
			m.Scopes()
			m.Status()
			m.ConversationBindings()
			if lookups.Load() != lookupCount {
				t.Fatal("GET performed metadata resolution")
			}
			_, err = m.BindConversation(context.Background(), scope.ID, conversationA, "A", binding.Revision, memberRevisions(m, scope.ID, conversationA))
			var association *desktoprelay.AssociationError
			if !errors.As(err, &association) {
				t.Fatalf("untyped resolver failure: %v", err)
			}
			for chain := err; chain != nil; chain = errors.Unwrap(chain) {
				if strings.Contains(chain.Error(), "fixture-source-secret") {
					t.Fatal("resolver error chain leaked source text")
				}
			}
			r := send(t, c, br, "/v1/messages", `{}`, sessionA, nil)
			response := drain(t, r)
			if r.StatusCode != 503 || !strings.Contains(response, association.Code) || strings.Contains(response, "fixture-source-secret") || sends.Load() != 1 {
				t.Fatalf("unsafe unresolved dispatch: HTTP %d %s sends %d", r.StatusCode, response, sends.Load())
			}
			after := observed(t, m, scope.ID, sessionA)
			if after.ConversationID != before.ConversationID || after.AccountID != before.AccountID || after.Revision != before.Revision || after.Requests != before.Requests {
				t.Fatalf("failed resolution changed verified mapping %+v", after)
			}
			view, _ := json.Marshal(m.Sessions())
			if strings.Contains(string(view), "Bearer") {
				t.Fatalf("unsafe view %s", view)
			}
		})
	}
}

func TestExplicitConversationConsentCanIncludeNewlyVerifiedObservedMember(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	var known atomic.Bool
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		out := make(map[string]string)
		for _, id := range ids {
			if id == sessionA || known.Load() {
				out[id] = conversationA
			}
		}
		return out, nil
	})
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	for _, id := range []string{sessionA, sessionB} {
		drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
	}
	known.Store(true)
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("newly verified hidden member ignored: %v", err)
	}
	expected := map[string]uint64{sessionA: observed(t, m, scope.ID, sessionA).Revision, sessionB: observed(t, m, scope.ID, sessionB).Revision}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, expected); err != nil {
		t.Fatal(err)
	}
	if got := observed(t, m, scope.ID, sessionB); got.ConversationID != conversationA || got.AccountID != "B" || got.Revision != expected[sessionB]+1 {
		t.Fatalf("explicit consent missed converging member %+v", got)
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationB, "B", 0, nil); !errors.Is(err, desktoprelay.ErrNotFound) {
		t.Fatalf("missing conversation was bound: %v", err)
	}
}

func TestConversationCASResetRestartAndScopeIsolation(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA, sessionB: conversationA, sessionC: conversationB, sessionD: conversationA, sessionE: conversationA})
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	for _, id := range []string{sessionA, sessionB, sessionC} {
		drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
	}
	for id, account := range map[string]string{sessionA: "A", sessionB: "B"} {
		if _, err := m.Bind(context.Background(), scope.ID, id, account, observed(t, m, scope.ID, id).Revision); err != nil {
			t.Fatal(err)
		}
	}
	before := memberRevisions(m, scope.ID, conversationA)
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, map[string]uint64{sessionA: before[sessionA]}); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("hidden member accepted: %v", err)
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationB, "B", 0, map[string]uint64{sessionA: before[sessionA]}); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("foreign membership accepted: %v", err)
	}
	stale := memberRevisions(m, scope.ID, conversationA)
	stale[sessionB]--
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, stale); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("stale member accepted: %v", err)
	}
	binding, err := m.BindConversation(context.Background(), strings.ToUpper(scope.ID), strings.ToUpper(conversationA), "B", 0, before)
	if err != nil || binding.ScopeID != scope.ID || binding.ConversationID != conversationA {
		t.Fatalf("case-normalized bind %+v %v", binding, err)
	}
	for _, id := range []string{sessionA, sessionB} {
		member := observed(t, m, scope.ID, id)
		if member.AccountID != "B" {
			t.Fatalf("partial group publication %+v", member)
		}
		if _, err := m.Bind(context.Background(), scope.ID, id, "A", member.Revision); !errors.Is(err, desktoprelay.ErrConflict) {
			t.Fatalf("exact bind overrode group: %v", err)
		}
		if _, err := m.Unbind(scope.ID, id, member.Revision); !errors.Is(err, desktoprelay.ErrConflict) {
			t.Fatalf("exact reset split group: %v", err)
		}
	}
	peer, err := m.CreateScope("CLI peer")
	if err != nil {
		t.Fatal(err)
	}
	pc, pbr := tunnel(t, peer)
	if got := drain(t, send(t, pc, pbr, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer caller-token" {
		t.Fatal("selection crossed scope")
	}
	// A newly observed verified alias invalidates an older member map.
	before = memberRevisions(m, scope.ID, conversationA)
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionD, nil)); got != "Bearer token-B" {
		t.Fatal("future member missed selection")
	}
	if _, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, binding.Revision, before); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("new member omitted by reset: %v", err)
	}
	if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, memberRevisions(m, scope.ID, conversationA)); !errors.Is(err, desktoprelay.ErrConflict) {
		t.Fatalf("stale aggregate CAS accepted: %v", err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	other, err := desktoprelay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close(context.Background()) })
	if err := other.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(scope.ProxyURL)
	u.Host = other.Status().Address
	scope.ProxyURL = u.String()
	c, br = tunnel(t, scope)
	if bindings := other.ConversationBindings(); len(bindings) != 1 || bindings[0] != binding {
		t.Fatalf("group not persistent: %+v", bindings)
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionE, nil)); got != "Bearer token-B" {
		t.Fatal("new alias after restart missed selection")
	}
	reset, err := other.UnbindConversation(context.Background(), scope.ID, conversationA, binding.Revision, memberRevisions(other, scope.ID, conversationA))
	if err != nil || reset.AccountID != "" || reset.Revision != 2 {
		t.Fatalf("reset %+v %v", reset, err)
	}
	for _, s := range other.Sessions() {
		if s.ScopeID == scope.ID && s.ConversationID == conversationA && s.AccountID != "" {
			t.Fatalf("reset left member override %+v", s)
		}
	}
	if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionB, nil)); got != "Bearer caller-token" {
		t.Fatal("reset inherited selection")
	}
	rebound, err := other.BindConversation(context.Background(), scope.ID, conversationA, "A", reset.Revision, memberRevisions(other, scope.ID, conversationA))
	if err != nil || rebound.Revision != 3 {
		t.Fatalf("reset lost revision history %+v %v", rebound, err)
	}
	if err := other.DeleteScope(scope.ID); err != nil {
		t.Fatal(err)
	}
	if len(other.ConversationBindings()) != 0 || len(other.Sessions()) != 1 || other.Sessions()[0].ScopeID != peer.ID {
		t.Fatal("revocation retained grouped state or removed peer")
	}
}

func TestConversationPreparationCannotPublishStaleMembershipOrLifecycle(t *testing.T) {
	for _, action := range []string{"new verified alias", "new unknown alias", "member revision", "group CAS", "metadata changed", "metadata missing", "cancel", "revoke", "restart"} {
		t.Run(action, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var metadata atomic.Int32
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				out := make(map[string]string)
				for _, id := range ids {
					if id == sessionD || metadata.Load() == 2 {
						continue
					}
					out[id] = conversationA
					if metadata.Load() == 1 {
						out[id] = conversationB
					}
				}
				return out, nil
			})
			entered, release := make(chan struct{}), make(chan struct{})
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				if id == "B" {
					close(entered)
					<-release
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionB, nil))
			expected := memberRevisions(m, scope.ID, conversationA)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := m.BindConversation(ctx, scope.ID, conversationA, "B", 0, expected); result <- err }()
			await(t, entered)
			// The public view must remain available during both external sources.
			if len(m.Sessions()) != 2 || len(m.ConversationBindings()) != 0 {
				t.Fatal("preparation published early")
			}
			want := desktoprelay.ErrConflict
			switch action {
			case "new verified alias":
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionC, nil))
			case "new unknown alias":
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionD, nil))
			case "member revision":
				_, err := m.Bind(context.Background(), scope.ID, sessionA, "A", expected[sessionA])
				if err != nil {
					t.Fatal(err)
				}
			case "group CAS":
				_, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, expected)
				if err != nil {
					t.Fatal(err)
				}
			case "metadata changed":
				metadata.Store(1)
			case "metadata missing":
				metadata.Store(2)
			case "cancel":
				cancel()
				want = context.Canceled
			case "revoke":
				if err := m.DeleteScope(scope.ID); err != nil {
					t.Fatal(err)
				}
				want = desktoprelay.ErrNotFound
			case "restart":
				if err := m.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := m.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				want = context.Canceled
			}
			close(release)
			if err := <-result; !errors.Is(err, want) {
				t.Fatalf("stale publication result %v, want %v", err, want)
			}
			for _, s := range m.Sessions() {
				if s.AccountID == "B" {
					t.Fatalf("stale selected account persisted %+v", s)
				}
			}
			for _, b := range m.ConversationBindings() {
				if b.AccountID == "B" {
					t.Fatalf("stale group persisted %+v", b)
				}
			}
		})
	}
}
