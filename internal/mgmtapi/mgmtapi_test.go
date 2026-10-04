package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switcher/internal/claudecode"
	"switcher/internal/provider"
	"switcher/internal/proxy"
	"switcher/internal/store"
)

type managementTransport func(*http.Request) (*http.Response, error)

func (f managementTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := f(r)
	if response != nil {
		response.Request = r
	}
	return response, err
}

type managementProvider struct{ provider.Provider }

func (*managementProvider) IsExpired(a store.Account) bool { return a.Token.AccessToken == "expired" }
func (*managementProvider) Refresh(_ context.Context, a *store.Account) error {
	a.Token.AccessToken, a.Token.ExpiresAt = "fresh", time.Now().Add(time.Hour).Unix()
	return nil
}

type managementNativeProvider struct{ *managementProvider }

func (*managementNativeProvider) NativeEnabled() bool { return true }
func (*managementNativeProvider) NativeStatus() claudecode.Status {
	return claudecode.Status{Available: true}
}
func (*managementNativeProvider) SwitchNative(context.Context, string, func(string) error) (claudecode.SwitchResult, error) {
	return claudecode.SwitchResult{}, provider.ErrUnsupported
}
func (*managementNativeProvider) SyncNative(_ context.Context, a *store.Account) (bool, error) {
	if a.Token.AccessToken != "saved-predecessor" {
		return false, nil
	}
	a.Token.AccessToken = "native-current"
	return true, nil
}
func (*managementNativeProvider) RefreshAfter401(_ context.Context, a *store.Account, rejected string) error {
	if rejected != "native-current" || a.Token.AccessToken != rejected {
		return errors.New("wrong rejected generation")
	}
	a.Token.AccessToken = "native-successor"
	return nil
}

func managementFixture(t *testing.T, prov provider.Provider, access string) *API {
	t.Helper()
	st := store.New(t.TempDir())
	for _, id := range []string{"a", "b"} {
		if err := st.Save(store.Account{ID: id, Provider: "codex", Token: store.Token{AccessToken: access}}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := proxy.New(st, map[string]provider.Provider{"codex": prov}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Activate("b"); err != nil {
		t.Fatal(err)
	}
	return &API{Store: st, Proxy: m, ManagementKey: "fixture-key"}
}

func mockManagementNetwork(t *testing.T, transport managementTransport) {
	t.Helper()
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func managementResponse(status int, location string) *http.Response {
	response := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}
	if location != "" {
		response.Header.Set("Location", location)
	}
	return response
}

func managementCall(api *API, target string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(apiCallRequest{AuthIndex: "a", URL: target, Header: map[string]string{"Authorization": "Bearer $TOKEN$", "X-Token": "$TOKEN$"}})
	request := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer fixture-key")
	response := httptest.NewRecorder()
	mux := http.NewServeMux()
	api.Register(mux)
	mux.ServeHTTP(response, request)
	return response
}

func TestAPICallRejectsPlaintextAndRedirectDowngrades(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial HTTP", true: "redirect HTTP"}[redirect], func(t *testing.T) {
			api := managementFixture(t, &managementProvider{}, "fresh")
			calls := 0
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme == "http" {
					t.Error("account token sent over plaintext HTTP")
					return managementResponse(200, ""), nil
				}
				return managementResponse(302, "http://chatgpt.com/quota"), nil
			})
			target, wantCode, wantCalls := "http://chatgpt.com/quota", 400, 0
			if redirect {
				target, wantCode, wantCalls = "https://chatgpt.com/quota", 502, 1
			}
			response := managementCall(api, target)
			if response.Code != wantCode || calls != wantCalls {
				t.Fatalf("unsafe destination accepted: status=%d calls=%d", response.Code, calls)
			}
		})
	}
}

func TestAPICallPreparesExpiredAndNativeCredentialsWithoutSwitching(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired", true: "native rejected generation"}[native], func(t *testing.T) {
			var prov provider.Provider = &managementProvider{}
			access, want := "expired", "fresh"
			if native {
				// Claude Code rotated the login; Switcher's stored copy is rejected
				// and the retry adopts Claude Code's token without a new grant.
				prov, access, want = &managementNativeProvider{managementProvider: &managementProvider{}}, "saved-predecessor", "native-current"
			}
			api := managementFixture(t, prov, access)
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				if native && r.Header.Get("Authorization") == "Bearer saved-predecessor" {
					return managementResponse(401, ""), nil
				}
				if r.Header.Get("Authorization") != "Bearer "+want {
					t.Errorf("management used stale credentials: %q", r.Header.Get("Authorization"))
				}
				return managementResponse(200, ""), nil
			})
			response := managementCall(api, "https://chatgpt.com/quota")
			var result struct {
				StatusCode int `json:"status_code"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || result.StatusCode != 200 || api.Proxy.ActiveID("codex") != "b" {
				t.Fatalf("management credential preparation changed route or failed: status=%d body=%s active=%s", response.Code, response.Body.String(), api.Proxy.ActiveID("codex"))
			}
		})
	}
}

func TestAPICallRedirectCredentialsStayOnOriginalOrigin(t *testing.T) {
	for _, crossOrigin := range []bool{false, true} {
		t.Run(map[bool]string{false: "same origin", true: "cross origin"}[crossOrigin], func(t *testing.T) {
			api := managementFixture(t, &managementProvider{}, "fresh")
			calls := 0
			mockManagementNetwork(t, func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					location := "https://chatgpt.com/quota-next"
					if crossOrigin {
						location = "https://api.anthropic.com/quota-next"
					}
					return managementResponse(302, location), nil
				}
				for _, key := range []string{"Authorization", "X-Token"} {
					if crossOrigin && r.Header.Get(key) != "" || !crossOrigin && r.Header.Get(key) == "" {
						t.Errorf("redirect credential policy violated for %s", key)
					}
				}
				return managementResponse(200, ""), nil
			})
			if response := managementCall(api, "https://chatgpt.com/quota"); response.Code != 200 || calls != 2 {
				t.Fatalf("redirect failed: status=%d calls=%d", response.Code, calls)
			}
		})
	}
}
