package desktoprelay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"switcher/internal/desktoprelay"
)

type cachedDiscoveryFixture struct {
	lookup func(context.Context, []string) (map[string]string, error)
	cached func([]string, map[string]string) map[string]string
}

func (f cachedDiscoveryFixture) Resolve(ctx context.Context, ids []string) (map[string]string, error) {
	return f.lookup(ctx, ids)
}
func (f cachedDiscoveryFixture) ResolveCached(ids []string, known map[string]string) map[string]string {
	return f.cached(ids, known)
}

func TestCallerAndExactInferenceHaveNoMetadataIODependencyWithoutActiveGroups(t *testing.T) {
	for _, mode := range []string{"cold", "missing", "changed", "invalid cache", "base only"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var calls atomic.Int32
			var failed atomic.Bool
			resolver := cachedDiscoveryFixture{
				lookup: func(_ context.Context, ids []string) (map[string]string, error) {
					calls.Add(1)
					if failed.Load() {
						return nil, errors.New("fixture metadata timeout")
					}
					return map[string]string{sessionA: conversationA}, nil
				},
				cached: func(ids []string, known map[string]string) map[string]string {
					if mode == "cold" {
						return nil
					}
					if !failed.Load() {
						return map[string]string{sessionA: conversationA}
					}
					if mode == "changed" {
						return map[string]string{sessionA: conversationB}
					}
					if mode == "invalid cache" {
						return map[string]string{sessionA: "local_" + conversationA}
					}
					return nil
				},
			}
			cfg.Conversations = resolver
			if mode == "base only" {
				cfg.Conversations = struct {
					desktoprelay.ConversationResolver
				}{resolverFunc(resolver.lookup)}
			}
			body := ` {"model":"native-model","messages":[{"role":"user","content":"unchanged"}]} `
			wantToken := "Bearer caller-token"
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				got, _ := io.ReadAll(r.Body)
				if string(got) != body || r.Header.Get("Authorization") != wantToken || r.URL.RawQuery != "beta=true" {
					t.Errorf("request changed %q %s %s", got, r.Header.Get("Authorization"), r.URL)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			request := func() {
				t.Helper()
				r := send(t, c, br, "/v1/messages?beta=true", body, sessionA, nil)
				drain(t, r)
				if r.StatusCode != 200 {
					t.Fatalf("metadata blocked normal request: %d", r.StatusCode)
				}
			}
			request()
			if calls.Load() != 0 {
				t.Fatal("cold caller discovery performed metadata I/O")
			}
			if _, err := m.Bind(context.Background(), scope.ID, sessionA, "B", observed(t, m, scope.ID, sessionA).Revision); err != nil {
				t.Fatal(err)
			}
			failed.Store(true)
			baseline := calls.Load()
			wantToken = "Bearer token-B"
			request()
			member := observed(t, m, scope.ID, sessionA)
			if _, err := m.Unbind(scope.ID, sessionA, member.Revision); err != nil {
				t.Fatal(err)
			}
			wantToken = "Bearer caller-token"
			request()
			if calls.Load() != baseline {
				t.Fatalf("inference performed metadata I/O: %d calls", calls.Load()-baseline)
			}
		})
	}
}

func TestUnknownCallerSurvivesActiveGroupDiscoveryFailureButSelectedGroupFailsClosed(t *testing.T) {
	for _, failure := range []string{"error", "missing", "changed"} {
		t.Run(failure, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.Source = fixtureSource()
			var fail atomic.Bool
			var sends atomic.Int32
			cfg.Conversations = cachedDiscoveryFixture{
				cached: func(ids []string, known map[string]string) map[string]string {
					return map[string]string{sessionA: conversationA}
				},
				lookup: func(_ context.Context, ids []string) (map[string]string, error) {
					out := make(map[string]string)
					if fail.Load() && failure == "error" {
						return nil, errors.New("fixture timed out reading metadata")
					}
					for _, id := range ids {
						if id == sessionA {
							if !fail.Load() {
								out[id] = conversationA
							} else if failure == "changed" {
								out[id] = conversationB
							}
						}
					}
					return out, nil
				},
			}
			cfg.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				sends.Add(1)
				if r.Header.Get("Authorization") != "Bearer caller-token" {
					t.Errorf("unexpected selected dispatch %s", r.Header.Get("Authorization"))
				}
				return echoCredential(r)
			})
			m, scope := startFixture(t, cfg)
			c, br := tunnel(t, scope)
			drain(t, send(t, c, br, "/v1/messages/count_tokens", `{}`, sessionA, nil))
			if _, err := m.BindConversation(context.Background(), scope.ID, conversationA, "B", 0, memberRevisions(m, scope.ID, conversationA)); err != nil {
				t.Fatal(err)
			}
			fail.Store(true)
			r := send(t, c, br, "/v1/messages", `{"model":"normal-caller"}`, sessionB, nil)
			if body := drain(t, r); r.StatusCode != 200 || body != "Bearer caller-token" {
				t.Fatalf("unknown caller blocked: HTTP %d %s", r.StatusCode, body)
			}
			if got := observed(t, m, scope.ID, sessionB); got.AccountID != "" || got.ConversationID != "" {
				t.Fatalf("unknown caller acquired group selection %+v", got)
			}
			r = send(t, c, br, "/v1/messages", `{"model":"selected-group"}`, sessionA, nil)
			drain(t, r)
			if r.StatusCode != 503 || sends.Load() != 2 {
				t.Fatalf("unverified selected group fell back: HTTP %d sends %d", r.StatusCode, sends.Load())
			}
		})
	}
}
