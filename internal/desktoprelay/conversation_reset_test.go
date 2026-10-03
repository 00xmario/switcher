package desktoprelay_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/desktoprelay"
)

func TestDefaultConversationResetFencesDelayedExactBindWithoutChangingMemberRevision(t *testing.T) {
	for _, repeated := range []bool{false, true} {
		name := "first reset"
		if repeated {
			name = "repeated default reset"
		}
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Conversations = metadataFixture(map[string]string{sessionA: conversationA})
			cfg.Transport = transportFunc(echoCredential)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var prepares atomic.Int32
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				prepares.Add(1)
				close(entered)
				<-release
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			before := observed(t, m, scope.ID, sessionA)
			expectedGroupRevision := uint64(0)
			if repeated {
				reset, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, 0, memberRevisions(m, scope.ID, conversationA))
				if err != nil {
					t.Fatal(err)
				}
				expectedGroupRevision = reset.Revision
			}
			result := make(chan error, 1)
			go func() {
				_, err := m.Bind(context.Background(), scope.ID, sessionA, "B", before.Revision)
				result <- err
			}()
			await(t, entered)
			reset, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, expectedGroupRevision, memberRevisions(m, scope.ID, conversationA))
			if err != nil || reset.Revision != expectedGroupRevision+1 || reset.AccountID != "" {
				t.Fatalf("default reset %+v %v", reset, err)
			}
			if got := observed(t, m, scope.ID, sessionA); got.Revision != before.Revision {
				t.Fatalf("test must fence by aggregate revision, not member revision: %+v", got)
			}
			unblock()
			if err := <-result; !errors.Is(err, desktoprelay.ErrConflict) {
				t.Fatalf("old exact bind published after reset: %v", err)
			}
			if got := observed(t, m, scope.ID, sessionA); got.AccountID != "" || got.Revision != before.Revision {
				t.Fatalf("delayed preparation restored authority %+v", got)
			}
			if prepares.Load() != 1 {
				t.Fatal("reset acquired credentials")
			}
			if err := m.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := m.Resume(context.Background()); err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(scope.ProxyURL)
			u.Host = m.Status().Address
			scope.ProxyURL = u.String()
			c, br = tunnel(t, scope)
			if got := drain(t, send(t, c, br, "/v1/messages", `{}`, sessionA, nil)); got != "Bearer caller-token" {
				t.Fatalf("durable reset lost to old exact bind %q", got)
			}
		})
	}
}

func TestRecordedConversationResetRejectsIncompleteOwnershipCASAndUnknownGroups(t *testing.T) {
	for _, refusal := range []string{"stale group", "missing member", "stale member", "foreign member", "extra unknown member", "unknown group", "missing newly verified offline member", "cancelled"} {
		t.Run(refusal, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var unavailable atomic.Bool
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				if unavailable.Load() && refusal != "missing newly verified offline member" {
					return nil, errors.New("fixture metadata failed")
				}
				out := make(map[string]string)
				for _, id := range ids {
					if id != sessionC || unavailable.Load() {
						out[id] = conversationA
					}
				}
				return out, nil
			})
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			for _, id := range []string{sessionA, sessionB, sessionC} {
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
			}
			if _, err := m.Bind(context.Background(), scope.ID, sessionC, "A", observed(t, m, scope.ID, sessionC).Revision); err != nil {
				t.Fatal(err)
			}
			binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			unavailable.Store(true)
			beforeSessions, beforeBindings := m.Sessions(), m.ConversationBindings()
			path := filepath.Join(cfg.DataRoot, "state.json")
			beforeDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			expected := memberRevisions(m, scope.ID, conversationA)
			conversation, revision, want := conversationA, binding.Revision, desktoprelay.ErrConflict
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch refusal {
			case "stale group":
				revision = 0
			case "missing member":
				delete(expected, sessionB)
			case "stale member":
				expected[sessionA]--
			case "foreign member":
				delete(expected, sessionB)
				expected[sessionC] = observed(t, m, scope.ID, sessionC).Revision
			case "extra unknown member":
				expected[sessionC] = observed(t, m, scope.ID, sessionC).Revision
			case "unknown group":
				conversation, revision, want = conversationB, 0, desktoprelay.ErrNotFound
			case "cancelled":
				cancel()
				want = context.Canceled
			}
			if _, err := m.UnbindConversation(ctx, scope.ID, conversation, revision, expected); !errors.Is(err, want) {
				t.Fatalf("unsafe recorded reset %s: %v, want %v", refusal, err, want)
			}
			if !reflect.DeepEqual(m.Sessions(), beforeSessions) || !reflect.DeepEqual(m.ConversationBindings(), beforeBindings) {
				t.Fatal("rejected reset changed ownership, binding or evidence")
			}
			afterDisk, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(afterDisk) != string(beforeDisk) || m.Status().Listening {
				t.Fatal("rejected reset published state or started a listener")
			}
		})
	}
}

func TestActiveConversationResetSurvivesStopDuringMetadataLookupAndPreventsFutureInheritance(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var pause, unavailable atomic.Bool
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		if pause.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
		if unavailable.Load() {
			return nil, errors.New("fixture metadata unavailable after stop")
		}
		out := make(map[string]string)
		for _, id := range ids {
			out[id] = conversationA
		}
		return out, nil
	})
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages", `{"model":"old-response"}`, sessionA, nil))
	group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	proof := observed(t, m, scope.ID, sessionA).LastResponse
	expected := memberRevisions(m, scope.ID, conversationA)
	pause.Store(true)
	result := make(chan error, 1)
	go func() {
		_, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, group.Revision, expected)
		result <- err
	}()
	await(t, entered)
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	unavailable.Store(true)
	unblock()
	if err := <-result; err != nil {
		t.Fatalf("stop cancelled safe reset %v", err)
	}
	if got := observed(t, m, scope.ID, sessionA); got.AccountID != "" || !reflect.DeepEqual(got.LastResponse, proof) {
		t.Fatalf("reset changed actual response evidence %+v", got)
	}
	if status := m.Status(); status.Enabled || status.Listening {
		t.Fatalf("reset reversed stop %+v", status)
	}
	unavailable.Store(false)
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(scope.ProxyURL)
	u.Host = m.Status().Address
	scope.ProxyURL = u.String()
	c, br = tunnel(t, scope)
	if got := drain(t, send(t, c, br, "/v1/messages", `{"model":"future-alias"}`, sessionB, nil)); got != "Bearer caller-token" {
		t.Fatalf("future verified alias inherited reset authority %q", got)
	}
	if bindings := m.ConversationBindings(); len(bindings) != 1 || bindings[0].AccountID != "" || bindings[0].Revision != 2 {
		t.Fatalf("reset epoch not durable %+v", bindings)
	}
}

func TestRecordedConversationResetCancellationDuringFinalLockCannotPublish(t *testing.T) {
	cfg := fixtureConfig(t)
	cfg.Source = fixtureSource()
	entered, releaseLookup, checked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	writeEntered, releaseWrite := make(chan struct{}), make(chan struct{})
	var lookupOnce, writeOnce sync.Once
	defer lookupOnce.Do(func() { close(releaseLookup) })
	defer writeOnce.Do(func() { close(releaseWrite) })
	var pause, afterLookup, holdWrite atomic.Bool
	cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
		if pause.CompareAndSwap(true, false) {
			close(entered)
			<-releaseLookup
			afterLookup.Store(true)
		} else if afterLookup.Load() {
			close(checked)
		}
		out := make(map[string]string)
		for _, id := range ids {
			out[id] = conversationA
		}
		return out, nil
	})
	cfg.SyncDirectory = func(f *os.File) error {
		if holdWrite.CompareAndSwap(true, false) {
			close(writeEntered)
			<-releaseWrite
		}
		return f.Sync()
	}
	cfg.Transport = transportFunc(echoCredential)
	m, scope := startFixture(t, cfg)
	c, br := tunnel(t, scope)
	drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
	group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
	if err != nil {
		t.Fatal(err)
	}
	expected := memberRevisions(m, scope.ID, conversationA)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pause.Store(true)
	result := make(chan error, 1)
	go func() {
		_, err := m.UnbindConversation(ctx, scope.ID, conversationA, group.Revision, expected)
		result <- err
	}()
	await(t, entered)
	holdWrite.Store(true)
	wrote := make(chan error, 1)
	go func() { _, err := m.CreateScope("reset durable write contention"); wrote <- err }()
	await(t, writeEntered)
	lookupOnce.Do(func() { close(releaseLookup) })
	await(t, checked)
	select {
	case err := <-result:
		t.Fatalf("reset escaped final mutation lock %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	writeOnce.Do(func() { close(releaseWrite) })
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reset published %v", err)
	}
	if got := observed(t, m, scope.ID, sessionA); got.AccountID != "B" || got.Revision != expected[sessionA] {
		t.Fatalf("cancelled reset changed member %+v", got)
	}
	if bindings := m.ConversationBindings(); len(bindings) != 1 || bindings[0] != group {
		t.Fatalf("cancelled reset changed group epoch %+v", bindings)
	}
}

func TestConversationResetRechecksKnownMembersAggregateAndScopeAfterMetadataLookup(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		for _, action := range []string{"member added", "new selection", "revoke", "cancel lookup"} {
			name := action + " available"
			if unavailable {
				name = action + " unavailable"
			}
			t.Run(name, func(t *testing.T) {
				cfg := fixtureConfig(t)
				cfg.Source = fixtureSource()
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				var pause, failed atomic.Bool
				cfg.Conversations = resolverFunc(func(ctx context.Context, ids []string) (map[string]string, error) {
					if pause.CompareAndSwap(true, false) {
						close(entered)
						<-release
					}
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if failed.Load() {
						return nil, errors.New("fixture metadata unavailable")
					}
					out := make(map[string]string)
					for _, id := range ids {
						out[id] = conversationA
					}
					return out, nil
				})
				cfg.Transport = transportFunc(echoCredential)
				m, scope := startFixture(t, cfg)
				c, br := tunnel(t, scope)
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
				group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
				if err != nil {
					t.Fatal(err)
				}
				expected := memberRevisions(m, scope.ID, conversationA)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pause.Store(true)
				result := make(chan error, 1)
				go func() {
					_, err := m.UnbindConversation(ctx, scope.ID, conversationA, group.Revision, expected)
					result <- err
				}()
				await(t, entered)
				want := desktoprelay.ErrConflict
				switch action {
				case "member added":
					drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionB, nil))
				case "new selection":
					group, err = m.BindConversation(context.Background(), scope.ID, conversationA, "C", group.Revision, expected)
					if err != nil {
						t.Fatal(err)
					}
				case "revoke":
					if err := m.DeleteScope(scope.ID); err != nil {
						t.Fatal(err)
					}
					want = desktoprelay.ErrNotFound
				case "cancel lookup":
					cancel()
					want = context.Canceled
				}
				failed.Store(unavailable)
				unblock()
				if err := <-result; !errors.Is(err, want) {
					t.Fatalf("stale reset %s: %v, want %v", name, err, want)
				}
				if action == "revoke" {
					if len(m.ConversationBindings()) != 0 {
						t.Fatal("reset recreated revoked ownership")
					}
				} else {
					if bindings := m.ConversationBindings(); len(bindings) != 1 || bindings[0] != group {
						t.Fatalf("stale reset changed aggregate %+v", bindings)
					}
				}
			})
		}
	}
}

func TestInitialConversationResetRequiresRunningVerifiedMembership(t *testing.T) {
	for _, mode := range []string{"running verified", "running unavailable", "stopped verified", "stopped unavailable"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var failed atomic.Bool
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				if failed.Load() {
					return nil, errors.New("fixture metadata unavailable")
				}
				out := make(map[string]string)
				for _, id := range ids {
					out[id] = conversationA
				}
				return out, nil
			})
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			member, err := m.Bind(context.Background(), scope.ID, sessionA, "B", observed(t, m, scope.ID, sessionA).Revision)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "stopped verified" || mode == "stopped unavailable" {
				if err := m.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			failed.Store(mode == "running unavailable" || mode == "stopped unavailable")
			reset, err := m.UnbindConversation(context.Background(), scope.ID, conversationA, 0, map[string]uint64{sessionA: member.Revision})
			if mode == "running verified" {
				if err != nil || reset.Revision != 1 || reset.AccountID != "" || observed(t, m, scope.ID, sessionA).AccountID != "" {
					t.Fatalf("verified legacy reset %+v %v", reset, err)
				}
			} else {
				want := desktoprelay.ErrNotFound
				if mode == "running unavailable" {
					want = desktoprelay.ErrUnavailable
				}
				if !errors.Is(err, want) || len(m.ConversationBindings()) != 0 || observed(t, m, scope.ID, sessionA).AccountID != "B" {
					t.Fatalf("unverified reset invented ownership %+v %v", reset, err)
				}
			}
		})
	}
}

func TestGroupedMutationIgnoresUnrelatedAssociationChangesAndIncludesVerifiedTargetAliases(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "bind"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var phase atomic.Int32
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				out := make(map[string]string)
				for _, id := range ids {
					if id == sessionB && phase.Load() == 0 {
						continue
					}
					conversation := conversationA
					if id == sessionC {
						conversation = conversationB
						if phase.Load() == 1 {
							conversation = sessionC
						}
						if phase.Load() == 2 {
							continue
						}
					}
					out[id] = conversation
				}
				return out, nil
			})
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				if id == "B" {
					phase.Store(2)
				}
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			for _, id := range []string{sessionA, sessionB, sessionC} {
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
			}
			group, err := m.BindConversation(context.Background(), scope.ID, conversationA, "A", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			phase.Store(1)
			expected := map[string]uint64{sessionA: observed(t, m, scope.ID, sessionA).Revision, sessionB: observed(t, m, scope.ID, sessionB).Revision}
			account := "B"
			if reset {
				group, err = m.UnbindConversation(context.Background(), scope.ID, conversationA, group.Revision, expected)
				account = ""
			} else {
				group, err = m.BindConversation(context.Background(), scope.ID, conversationA, "B", group.Revision, expected)
			}
			if err != nil || group.AccountID != account || group.Revision != 2 {
				t.Fatalf("unrelated stale association blocked target %s: %+v %v", name, group, err)
			}
			for _, id := range []string{sessionA, sessionB} {
				if s := observed(t, m, scope.ID, id); s.ConversationID != conversationA || s.AccountID != account {
					t.Fatalf("target member missed mutation %+v", s)
				}
			}
			if unrelated := observed(t, m, scope.ID, sessionC); unrelated.ConversationID != conversationB || unrelated.Revision != 1 || unrelated.AccountID != "" {
				t.Fatalf("mutation reassociated unrelated chat %+v", unrelated)
			}
		})
	}
}

func TestConversationResetWorksStoppedWithUnavailableMetadataAndNoCredentialAcquisition(t *testing.T) {
	for _, mode := range []string{"stopped available", "closed available", "fresh unavailable", "fresh nil resolver", "missing target", "changed target", "unrelated changed"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t)
			var unavailable atomic.Bool
			var prepares atomic.Int32
			cfg.Source = sourceFunc{prepare: func(_ context.Context, id string) (desktoprelay.Credential, error) {
				prepares.Add(1)
				return desktoprelay.Credential{AccountID: id, AccessToken: "token-" + id}, nil
			}}
			cfg.Conversations = resolverFunc(func(_ context.Context, ids []string) (map[string]string, error) {
				if unavailable.Load() && mode == "fresh unavailable" {
					return nil, errors.New("fixture metadata unavailable")
				}
				out := make(map[string]string)
				for _, id := range ids {
					conversation := conversationA
					if id == sessionC {
						conversation = conversationB
					}
					if unavailable.Load() {
						if mode == "missing target" && id != sessionC {
							continue
						}
						if mode == "changed target" && id != sessionC {
							conversation = conversationB
						}
						if mode == "unrelated changed" && id == sessionC {
							continue
						}
					}
					out[id] = conversation
				}
				return out, nil
			})
			cfg.Transport = transportFunc(echoCredential)
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			for _, id := range []string{sessionA, sessionB, sessionC} {
				drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, id, nil))
			}
			binding, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA))
			if err != nil {
				t.Fatal(err)
			}
			expected := memberRevisions(m, scope.ID, conversationA)
			if mode == "closed available" {
				err = m.Close(context.Background())
			} else {
				err = m.Stop(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			enabled := m.Status().Enabled
			unavailable.Store(true)
			target := m
			if mode == "fresh unavailable" || mode == "fresh nil resolver" {
				cfg.Source = nil
				if mode == "fresh nil resolver" {
					cfg.Conversations = nil
				}
				target, err = desktoprelay.New(cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { target.Close(context.Background()) })
			}
			reset, err := target.UnbindConversation(context.Background(), scope.ID, conversationA, binding.Revision, expected)
			if err != nil || reset.AccountID != "" || reset.Revision != binding.Revision+1 {
				t.Fatalf("stopped reset %+v %v", reset, err)
			}
			if status := target.Status(); status.Listening || status.Enabled != enabled {
				t.Fatalf("reset activated listener or changed enablement %+v", status)
			}
			for _, id := range []string{sessionA, sessionB} {
				member := observed(t, target, scope.ID, id)
				if member.AccountID != "" || member.ConversationID != conversationA || member.Revision != expected[id]+1 {
					t.Fatalf("recorded member not reset %+v", member)
				}
			}
			if other := observed(t, target, scope.ID, sessionC); other.ConversationID != conversationB || other.Revision != 1 {
				t.Fatalf("reset changed unrelated ownership %+v", other)
			}
			if prepares.Load() != 1 {
				t.Fatalf("reset acquired credentials: %d preparations", prepares.Load())
			}
			reset, err = target.UnbindConversation(context.Background(), scope.ID, conversationA, reset.Revision, memberRevisions(target, scope.ID, conversationA))
			if err != nil || reset.Revision != 3 {
				t.Fatalf("repeated stopped reset lost epoch %+v %v", reset, err)
			}
			if err := target.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			other, err := desktoprelay.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(context.Background())
			if _, err := other.UnbindConversation(context.Background(), scope.ID, conversationA, reset.Revision, expected); !errors.Is(err, desktoprelay.ErrConflict) {
				t.Fatalf("stopped reload accepted stale members: %v", err)
			}
			if bindings := other.ConversationBindings(); len(bindings) != 1 || bindings[0] != reset {
				t.Fatalf("durable reset epoch changed %+v", bindings)
			}
		})
	}
}
