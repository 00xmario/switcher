package googleauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"switcher/internal/googleauth"
	"switcher/internal/provider"
	"switcher/internal/provider/antigravity"
	"switcher/internal/provider/gemini"
	"switcher/internal/store"
)

type memoryTransport func(*http.Request) (*http.Response, error)

func (f memoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mockGoogleHTTP(t *testing.T, roundTrip memoryTransport) {
	t.Helper()
	old := provider.OAuthHTTPClient
	provider.OAuthHTTPClient = &http.Client{Transport: roundTrip}
	t.Cleanup(func() { provider.OAuthHTTPClient = old })
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func googleProviders() []provider.Provider {
	return []provider.Provider{gemini.New(), antigravity.New()}
}

func quotaSummary(fraction, reset string) string {
	return fmt.Sprintf(`{"groups":[{"displayName":"fixture","buckets":[{"remainingFraction":%s,"resetTime":%q}]}]}`, fraction, reset)
}

func TestGoogleRateLimitRequiresConfirmedDepletedQuota(t *testing.T) {
	reset := time.Now().UTC().Truncate(time.Second).Add(90 * time.Minute)
	for _, p := range googleProviders() {
		for _, tc := range []struct {
			name, errorBody, quota string
			status, usageStatus    int
			want, query            bool
		}{
			{"burst", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, quotaSummary("0.5", reset.Format(time.RFC3339)), 429, 200, false, true},
			{"malformed error", `{"error":`, quotaSummary("0.5", reset.Format(time.RFC3339)), 429, 200, false, true},
			{"quota outage", `{}`, `{"private":"do-not-log"}`, 429, 503, false, true},
			{"unknown quota", `{}`, `{}`, 429, 200, false, true},
			{"malformed quota", `{}`, `{"groups":`, 429, 200, false, true},
			{"invalid negative fraction", `{}`, quotaSummary("-0.5", reset.Format(time.RFC3339)), 429, 200, false, true},
			{"invalid large fraction", `{}`, quotaSummary("1.5", reset.Format(time.RFC3339)), 429, 200, false, true},
			{"tiny remaining fraction", `{}`, quotaSummary("1e-100", reset.Format(time.RFC3339)), 429, 200, false, true},
			{"oversized quota", `{}`, quotaSummary("0", reset.Format(time.RFC3339)) + strings.Repeat(" ", (1<<20)+1), 429, 200, false, true},
			{"confirmed quota", `{"error":"other"}`, quotaSummary("0", reset.Format(time.RFC3339)), 429, 200, true, true},
			{"confirmed multiple windows", `{}`, fmt.Sprintf(`{"groups":[{"buckets":[{"remainingFraction":0,"resetTime":%q},{"remainingFraction":0,"resetTime":%q},{"remainingFraction":0.5,"resetTime":%q}]}]}`, reset.Add(-time.Hour).Format(time.RFC3339), reset.Format(time.RFC3339), reset.Add(time.Hour).Format(time.RFC3339)), 429, 200, true, true},
			{"confirmed structured resource error", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, quotaSummary("0", reset.Format(time.RFC3339)), 503, 200, true, true},
			{"missing reset", `{}`, quotaSummary("0", ""), 429, 200, true, true},
			{"past reset", `{}`, quotaSummary("0", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)), 429, 200, true, true},
			{"message mention", `{"error":{"message":"RESOURCE_EXHAUSTED"}}`, quotaSummary("0", ""), 500, 200, false, false},
			{"unstructured resource error", `{"error":"RESOURCE_EXHAUSTED"}`, quotaSummary("0", ""), 500, 200, false, false},
			{"successful HTTP status", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, quotaSummary("0", ""), 200, 200, false, false},
			{"oversized error", `{"error":{"status":"RESOURCE_EXHAUSTED"}}` + strings.Repeat(" ", 8<<10), quotaSummary("0", ""), 429, 200, false, false},
		} {
			t.Run(p.ID()+"/"+tc.name, func(t *testing.T) {
				calls := 0
				mockGoogleHTTP(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Header.Get("Authorization") != "Bearer fixture-access" {
						t.Fatal("wrong quota credential")
					}
					switch r.URL.Path {
					case "/v1internal:retrieveUserQuotaSummary":
						return response(tc.usageStatus, tc.quota), nil
					case "/v1internal:fetchAvailableModels":
						return response(http.StatusServiceUnavailable, `{"private":"do-not-log"}`), nil
					default:
						t.Fatalf("unexpected Google request: %s", r.URL.Path)
						return nil, errors.New("unexpected request")
					}
				})
				a := store.Account{Token: store.Token{AccessToken: "fixture-access", Extra: map[string]any{"project_id": "fixture-project"}}}
				until, exhausted := p.ParseRateLimit(context.Background(), a, tc.status, []byte(tc.errorBody))
				if exhausted != tc.want || (!exhausted && !until.IsZero()) || (exhausted && !until.After(time.Now())) {
					t.Fatalf("classification = %v, %v", until, exhausted)
				}
				if (calls > 0) != tc.query {
					t.Fatalf("quota calls = %d, query allowed = %v", calls, tc.query)
				}
				if exhausted && strings.Contains(tc.name, "confirmed") && !until.Equal(reset) {
					t.Fatalf("reset = %v, want reported %v", until, reset)
				}
			})
		}
	}
}

func TestAntigravityModelsFallbackRequiresActualZeroQuota(t *testing.T) {
	for _, fraction := range []string{"-0.5", "1.5", "1e-100", "null", "0.5", "0"} {
		t.Run(fraction, func(t *testing.T) {
			mockGoogleHTTP(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/v1internal:retrieveUserQuotaSummary":
					return response(503, `{}`), nil
				case "/v1internal:fetchAvailableModels":
					return response(200, fmt.Sprintf(`{"models":{"fixture-model":{"quotaInfo":{"remaining_fraction":%s}}}}`, fraction)), nil
				default:
					t.Fatal("unexpected models fallback request")
					return nil, errors.New("unexpected request")
				}
			})
			if _, exhausted := antigravity.New().ParseRateLimit(context.Background(), store.Account{}, 429, nil); exhausted != (fraction == "0") {
				t.Fatalf("remaining fraction %s classified as exhausted=%v", fraction, exhausted)
			}
		})
	}
}

func TestGoogleQuotaCancellationAndStatusContracts(t *testing.T) {
	for _, p := range googleProviders() {
		for _, status := range []int{401, 403, 429, 503} {
			t.Run(fmt.Sprintf("%s/status-%d", p.ID(), status), func(t *testing.T) {
				mockGoogleHTTP(t, func(*http.Request) (*http.Response, error) {
					return response(status, `{"private":"do-not-log"}`), nil
				})
				a := store.Account{Token: store.Token{AccessToken: "secret-access", Extra: map[string]any{"project_id": "fixture-project"}}}
				_, err := p.Usage(context.Background(), a)
				if !errors.Is(err, provider.ErrUsageUnavailable) || errors.Is(err, provider.ErrUsageAuthRequired) != (status == 401) ||
					errors.Is(err, provider.ErrUsageRateLimited) != (status == 429) || errors.Is(err, provider.ErrReloginRequired) ||
					strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "secret-access") {
					t.Fatalf("wrong quota error: %v", err)
				}
			})
		}
		t.Run(p.ID()+"/cancellation", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mockGoogleHTTP(t, func(*http.Request) (*http.Response, error) {
				cancel()
				return nil, context.Canceled
			})
			a := store.Account{Token: store.Token{Extra: map[string]any{"project_id": "fixture-project"}}}
			if _, exhausted := p.ParseRateLimit(ctx, a, 429, nil); exhausted {
				t.Fatal("cancelled quota request caused exhaustion")
			}
			if _, err := p.Usage(ctx, a); !errors.Is(err, context.Canceled) || !errors.Is(err, provider.ErrUsageUnavailable) {
				t.Fatalf("usage cancellation was lost: %v", err)
			}
		})
	}
}

func TestGoogleProjectDiscoveryProvenanceAndCallerCancellation(t *testing.T) {
	for _, p := range googleProviders() {
		for _, outcome := range []string{"discovered", "outage", "service timeout", "invalid project", "caller cancelled"} {
			t.Run(p.ID()+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				mockGoogleHTTP(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/token":
						return response(200, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":3600}`), nil
					case "/oauth2/v2/userinfo":
						return response(200, `{"email":"fixture@example.test"}`), nil
					case "/v1internal:loadCodeAssist":
						switch outcome {
						case "outage":
							return response(503, `{"private":"do-not-log"}`), nil
						case "service timeout":
							return nil, context.DeadlineExceeded
						case "invalid project":
							return response(200, `{}`), nil
						case "caller cancelled":
							cancel()
							return nil, context.Canceled
						}
						return response(200, `{"cloudaicompanionProject":"projects/fixture-project"}`), nil
					case "/v1internal:onboardUser":
						return response(200, `{"done":true,"response":{"cloudaicompanionProject":"projects/"}}`), nil
					default:
						t.Fatalf("unexpected Google request: %s", r.URL.Path)
						return nil, errors.New("unexpected request")
					}
				})
				info, err := p.LoginStart(ctx)
				if err != nil {
					t.Fatal(err)
				}
				a, err := p.LoginExchange(ctx, info.State, "fixture-code")
				if outcome == "caller cancelled" {
					if !errors.Is(err, context.Canceled) || a.ID != "" {
						t.Fatalf("cancelled login returned an account: %q, %v", a.ID, err)
					}
					return
				}
				if err != nil || a.Token.AccessToken != "fixture-access" || a.Token.RefreshToken != "fixture-refresh" {
					t.Fatalf("valid login credentials were lost: %v", err)
				}
				wantStatus, wantProject := googleauth.ProjectDiscoveryFailed, ""
				if outcome == "discovered" {
					wantStatus, wantProject = googleauth.ProjectDiscoveryDiscovered, "fixture-project"
				}
				raw, err := json.Marshal(a)
				if err != nil {
					t.Fatal(err)
				}
				var saved store.Account
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}
				if saved.Token.Extra[googleauth.ProjectDiscoveryStatusKey] != wantStatus || saved.Token.Extra["project_id"] != wantProject || strings.Contains(string(raw), "do-not-log") {
					t.Fatal("discovery provenance did not survive persistence or retained an upstream error")
				}
				if err := p.Refresh(context.Background(), &saved); err != nil || saved.Token.Extra[googleauth.ProjectDiscoveryStatusKey] != wantStatus {
					t.Fatalf("token refresh lost discovery provenance: %v", err)
				}
			})
		}
	}
}

func TestGoogleApplyAuthRemovesAlternateKeys(t *testing.T) {
	for _, p := range googleProviders() {
		t.Run(p.ID(), func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodPost, "https://example.test/v1internal:generateContent", nil)
			r.Header.Set("Authorization", "Bearer old-token")
			r.Header.Set("X-Api-Key", "old-api-key")
			r.Header.Set("X-Goog-Api-Key", "old-google-key")
			r.Header.Set("X-Client-Feature", "preserve")
			if err := p.ApplyAuth(r, store.Account{Token: store.Token{AccessToken: "chosen-token"}}); err != nil {
				t.Fatal(err)
			}
			if r.Header.Get("Authorization") != "Bearer chosen-token" || r.Header.Get("X-Api-Key") != "" ||
				r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("X-Client-Feature") != "preserve" {
				t.Fatal("outgoing authentication or feature headers are wrong")
			}
		})
	}
}

func TestGoogleAuthRejectsOversizedJSONPrefixes(t *testing.T) {
	for _, operation := range []string{"exchange", "refresh", "userinfo", "onboard"} {
		t.Run(operation, func(t *testing.T) {
			mockGoogleHTTP(t, func(*http.Request) (*http.Response, error) {
				body := `{"access_token":"do-not-log","email":"fixture@example.test","done":true,"response":{"cloudaicompanionProject":"fixture-project"}}`
				return response(200, body+strings.Repeat(" ", (1<<20)+1)), nil
			})
			var err error
			switch operation {
			case "exchange":
				_, err = googleauth.TokenExchange(context.Background(), "secret", "code", "client", "http://example.test/callback")
			case "refresh":
				_, err = googleauth.RefreshToken(context.Background(), "secret", "refresh", "client")
			case "userinfo":
				_, err = googleauth.Userinfo(context.Background(), "access")
			case "onboard":
				_, err = googleauth.OnboardUser(context.Background(), "access", "https://example.test", "agent", "client", []byte(`{}`))
			}
			if err == nil || errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
				t.Fatalf("oversized response was accepted, classified, or leaked: %v", err)
			}
		})
	}
}
