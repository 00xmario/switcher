package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/store"
)

type fixtureKeychain struct{ values map[string][]byte }

func (k *fixtureKeychain) Read(_ context.Context, service string) ([]byte, bool, error) {
	value, ok := k.values[service]
	return append([]byte(nil), value...), ok, nil
}

func TestBurst429DoesNotExhaustSubscription(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name        string
		body        string
		usageStatus int
		usageBody   string
		want        bool
	}{
		{"burst-with-quota", `{"error":{"type":"rate_limit_error","message":"Request rate limit exceeded"}}`, 200, `{"five_hour":{"utilization":0},"seven_day":{"utilization":5}}`, false},
		{"burst-with-usage-outage", `{"error":{"type":"rate_limit_error"}}`, 503, `{}`, false},
		{"explicit-exhaustion", `{"error":{"type":"usage_limit_reached"}}`, 503, `{}`, true},
		{"confirmed-quota", `{"error":{"type":"rate_limit_error"}}`, 200, fmt.Sprintf(`{"five_hour":{"utilization":100,"resets_at":%q}}`, reset.Format(time.RFC3339)), true},
		{"already-reset", `{"error":{"type":"rate_limit_error"}}`, 200, `{"five_hour":{"utilization":100,"resets_at":"2000-01-01T00:00:00Z"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/oauth/usage" || req.Method != http.MethodGet {
					t.Fatal("unexpected fixture request")
				}
				return &http.Response{StatusCode: tc.usageStatus, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.usageBody))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = old })
			until, exhausted := New().ParseRateLimit(context.Background(), store.Account{Token: store.Token{AccessToken: "fixture-access"}}, 429, []byte(tc.body))
			if exhausted != tc.want || (!tc.want && !until.IsZero()) {
				t.Fatalf("429 classification: exhausted=%v until=%v", exhausted, until)
			}
			if tc.name == "confirmed-quota" && !until.Equal(reset) {
				t.Fatal("confirmed exhaustion lost the provider's reset time")
			}
		})
	}
}

func TestGeneric429IgnoresModelScopedQuota(t *testing.T) {
	sessionReset := time.Now().Add(time.Hour).Truncate(time.Second)
	weeklyReset := sessionReset.Add(time.Hour)
	modelReset := weeklyReset.Add(2 * time.Hour)
	scoped := fmt.Sprintf(`"limits":[{"kind":"weekly_scoped","percent":100,"resets_at":%q,"scope":{"model":{"display_name":"Sonnet"}}}]`, modelReset.Format(time.RFC3339))
	generic := `{"error":{"type":"rate_limit_error","message":"Request rate limit exceeded"}}`
	for _, tc := range []struct {
		name      string
		body      string
		usageBody string
		want      bool
		wantReset time.Time
	}{
		{"overall-quota-available", generic, `{"five_hour":{"utilization":20},"seven_day":{"utilization":30},` + scoped + `}`, false, time.Time{}},
		{"overall-quota-unavailable", generic, `{"five_hour":null,"seven_day":null,` + scoped + `}`, false, time.Time{}},
		{"session-exhausted", generic, fmt.Sprintf(`{"five_hour":{"utilization":100,"resets_at":%q},"seven_day":{"utilization":30},%s}`, sessionReset.Format(time.RFC3339), scoped), true, sessionReset},
		{"weekly-exhausted", generic, fmt.Sprintf(`{"five_hour":{"utilization":20},"seven_day":{"utilization":100,"resets_at":%q},%s}`, weeklyReset.Format(time.RFC3339), scoped), true, weeklyReset},
		{"explicit-exhaustion", `{"error":{"type":"usage_limit_reached"}}`, `{` + scoped + `}`, true, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Path != "/api/oauth/usage" {
					t.Fatal("unexpected fixture request")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.usageBody))}, nil
			})}
			t.Cleanup(func() { http.DefaultClient = old })
			until, exhausted := New().ParseRateLimit(context.Background(), store.Account{Token: store.Token{AccessToken: "fixture-access"}}, 429, []byte(tc.body))
			if exhausted != tc.want || (!tc.want && !until.IsZero()) {
				t.Fatalf("unrelated scoped quota changed account exhaustion: exhausted=%v until=%v", exhausted, until)
			}
			if !tc.wantReset.IsZero() && !until.Equal(tc.wantReset) {
				t.Fatalf("model-scoped reset extended account parking: got=%v want=%v", until, tc.wantReset)
			}
			if tc.name == "explicit-exhaustion" && !until.Before(modelReset) {
				t.Fatal("explicit exhaustion borrowed an unrelated model's reset")
			}
		})
	}
}

func (k *fixtureKeychain) Write(_ context.Context, service string, value []byte) error {
	k.values[service] = append([]byte(nil), value...)
	return nil
}
func (k *fixtureKeychain) Delete(_ context.Context, service string) error {
	delete(k.values, service)
	return nil
}

func TestRefreshAfter401ForcesOnlyRejectedNativeGeneration(t *testing.T) {
	for _, keychain := range []bool{false, true} {
		for _, advanced := range []bool{false, true} {
			name := "file"
			if keychain {
				name = "keychain"
			}
			if advanced {
				name += "/advanced"
			} else {
				name += "/rejected"
			}
			t.Run(name, func(t *testing.T) {
				home, data := t.TempDir(), t.TempDir()
				paths := claudecode.Paths{Home: home, ConfigHome: filepath.Join(home, ".claude"), ConfigFile: filepath.Join(home, ".claude.json"), CredentialsFile: filepath.Join(home, ".claude", ".credentials.json"), Service: "Claude Code-credentials", Keychain: keychain}
				identity := claudecode.Identity{UUID: "fixture-user", Email: "fixture@example.test", OrganizationUUID: "fixture-org"}
				identityJSON, _ := json.Marshal(identity)
				credential := func(access, refresh string) []byte {
					value, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "mcpOAuth": map[string]string{"fixture": "preserve"}})
					return value
				}
				a := store.Account{ID: "claude-fixture", Provider: "claude", Email: identity.Email, Token: store.Token{AccountID: identity.UUID, AccessToken: "at-original", RefreshToken: "rt-original", ExpiresAt: time.Now().Add(time.Hour).Unix()}, ClaudeCode: &store.ClaudeCodeLogin{Credentials: credential("at-original", "rt-original"), OAuthAccount: identityJSON}}
				st := store.New(data)
				if err := st.Save(a); err != nil {
					t.Fatal(err)
				}
				live := a.ClaudeCode.Credentials
				if advanced {
					live = credential("at-code-new", "rt-code-new")
				}
				if err := os.MkdirAll(paths.ConfigHome, 0700); err != nil {
					t.Fatal(err)
				}
				config, _ := json.Marshal(map[string]any{"oauthAccount": identity})
				if err := os.WriteFile(paths.ConfigFile, config, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(paths.CredentialsFile, live, 0600); err != nil {
					t.Fatal(err)
				}
				keys := &fixtureKeychain{values: map[string][]byte{}}
				if keychain {
					keys.values[paths.Service] = live
				}
				p := New()
				p.Native = claudecode.New(paths, data, st, keys, func(context.Context, string) (claudecode.Identity, error) { return identity, nil }, p.refreshGrant)
				calls := 0
				old := oauthHTTPClient
				oauthHTTPClient = &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					var body struct {
						Refresh string `json:"refresh_token"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if req.Method != http.MethodPost || body.Refresh != "rt-original" {
						t.Fatal("grant used another generation")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"at-issued","refresh_token":"rt-issued","expires_in":3600}`)), Header: http.Header{}}, nil
				})}
				t.Cleanup(func() { oauthHTTPClient = old })
				forced, ok := any(p).(interface {
					RefreshAfter401(context.Context, *store.Account, string) error
				})
				if !ok {
					t.Fatal("provider lacks generation-aware 401 refresh")
				}
				if err := forced.RefreshAfter401(context.Background(), &a, "at-original"); err != nil {
					t.Fatal(err)
				}
				wantAccess, wantRefresh, wantCalls := "at-issued", "rt-issued", 1
				if advanced {
					wantAccess, wantRefresh, wantCalls = "at-code-new", "rt-code-new", 0
				}
				if a.Token.AccessToken != wantAccess || a.Token.RefreshToken != wantRefresh || calls != wantCalls {
					t.Fatal("401 refresh repeated a rejected token or consumed a newer native grant")
				}
				if err := forced.RefreshAfter401(context.Background(), &a, "at-original"); err != nil || calls != wantCalls {
					t.Fatal("a repeated rejected generation consumed another grant")
				}
				written, err := os.ReadFile(paths.CredentialsFile)
				if err != nil || !strings.Contains(string(written), wantRefresh) || !strings.Contains(string(written), "preserve") {
					t.Fatal("native successor or shared MCP state was not retained")
				}
			})
		}
	}
}
