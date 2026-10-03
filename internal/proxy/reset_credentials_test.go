package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"switcher/internal/provider"
	"switcher/internal/provider/codex"
	"switcher/internal/store"
)

type resetCredentialProvider struct {
	*codex.Provider
	refreshes atomic.Int32
	rejected  string
}

func (p *resetCredentialProvider) Refresh(_ context.Context, a *store.Account) error {
	a.Token.AccessToken = fmt.Sprintf("fixture-fresh-%d", p.refreshes.Add(1))
	a.Token.ExpiresAt = time.Now().Add(time.Hour).Unix()
	return nil
}

func (p *resetCredentialProvider) RefreshAfter401(ctx context.Context, a *store.Account, rejected string) error {
	if rejected != a.Token.AccessToken {
		return errors.New("reset recovery supplied the wrong rejected generation")
	}
	p.rejected = rejected
	return p.Refresh(ctx, a)
}

func (*resetCredentialProvider) Usage(context.Context, store.Account) (provider.Usage, error) {
	return provider.Usage{Available: true, Windows: []provider.UsageWindow{{Label: "Session", ResetsAt: time.Now().Add(time.Hour).Unix()}}}, nil
}

func resetCredentialFixture(t *testing.T, access string, expiresAt int64) (*Manager, *resetCredentialProvider) {
	t.Helper()
	st := store.New(t.TempDir())
	if err := st.Save(store.Account{ID: "codex-fixture", Provider: "codex", Email: "fixture@example.test",
		Token: store.Token{AccessToken: access, RefreshToken: "fixture-refresh", AccountID: "fixture-account", ExpiresAt: expiresAt}}); err != nil {
		t.Fatal(err)
	}
	p := &resetCredentialProvider{Provider: codex.New()}
	m, err := New(st, map[string]provider.Provider{"codex": p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func resetCreditDocument(ids ...string) string {
	credits := make([]map[string]string, 0, len(ids))
	for i, id := range ids {
		credits = append(credits, map[string]string{"id": id, "status": "available", "reset_type": "codex_rate_limits",
			"expires_at": time.Now().Add(time.Duration(i+1) * time.Hour).UTC().Format(time.RFC3339)})
	}
	raw, _ := json.Marshal(map[string]any{"credits": credits})
	return string(raw)
}

func TestManualResetPreparesExpiredCodexCredentialsAndPinsCredit(t *testing.T) {
	m, p := resetCredentialFixture(t, "fixture-expired", time.Now().Add(-time.Hour).Unix())
	var lists, consumes int
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "chatgpt.com" || r.URL.Scheme != "https" {
			t.Fatalf("unexpected fixture destination: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-fresh-1" {
			t.Errorf("manual reset used an expired credential: %q", r.Header.Get("Authorization"))
			return regressionResponse(401, `{}`), nil
		}
		switch r.URL.Path {
		case "/backend-api/wham/rate-limit-reset-credits":
			lists++
			return regressionResponse(200, resetCreditDocument("earlier-credit", "chosen-credit")), nil
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consumes++
			var body struct {
				CreditID string `json:"credit_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CreditID != "chosen-credit" {
				t.Errorf("redemption changed the displayed credit: id=%q error=%v", body.CreditID, err)
			}
			return regressionResponse(200, `{"code":"reset"}`), nil
		default:
			return nil, fmt.Errorf("unexpected fixture route: %s", r.URL.Path)
		}
	})
	outcome, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit")
	awaitResetWorker(t, m, "codex-fixture")
	if err != nil || outcome != "reset" || p.refreshes.Load() != 1 || lists != 1 || consumes != 1 {
		t.Fatalf("manual reset did not prepare credentials: outcome=%s error=%v refreshes=%d lists=%d consumes=%d", outcome, err, p.refreshes.Load(), lists, consumes)
	}
	saved, err := m.store.Get("codex-fixture")
	if err != nil || saved.Token.AccessToken != "fixture-fresh-1" {
		t.Fatalf("refreshed credentials were not persisted: token=%q error=%v", saved.Token.AccessToken, err)
	}
	if _, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit"); err != nil || consumes != 1 || p.refreshes.Load() != 1 {
		t.Fatalf("pinned retry performed extra work: error=%v refreshes=%d consumes=%d", err, p.refreshes.Load(), consumes)
	}
}

func TestManualReset401RecoveryKeepsPinnedCredit(t *testing.T) {
	for _, rejectedOperation := range []string{"list", "consume"} {
		t.Run(rejectedOperation, func(t *testing.T) {
			m, p := resetCredentialFixture(t, "fixture-rejected", time.Now().Add(time.Hour).Unix())
			var lists, consumes int
			var redemptionID string
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				rejected := r.Header.Get("Authorization") == "Bearer fixture-rejected"
				if !rejected && r.Header.Get("Authorization") != "Bearer fixture-fresh-1" {
					t.Errorf("unexpected credential generation: %q", r.Header.Get("Authorization"))
				}
				switch r.URL.Path {
				case "/backend-api/wham/rate-limit-reset-credits":
					lists++
					if rejectedOperation == "list" && rejected {
						return regressionResponse(401, `{}`), nil
					}
					return regressionResponse(200, resetCreditDocument("earlier-credit", "chosen-credit")), nil
				case "/backend-api/wham/rate-limit-reset-credits/consume":
					consumes++
					var body struct {
						CreditID  string `json:"credit_id"`
						RequestID string `json:"redeem_request_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CreditID != "chosen-credit" || body.RequestID == "" {
						t.Errorf("redemption lost its pinned identity: body=%+v error=%v", body, err)
					}
					if redemptionID != "" && redemptionID != body.RequestID {
						t.Error("401 retry changed the redemption idempotency key")
					}
					redemptionID = body.RequestID
					if rejectedOperation == "consume" && rejected {
						return regressionResponse(401, `{}`), nil
					}
					return regressionResponse(200, `{"code":"reset"}`), nil
				default:
					return nil, fmt.Errorf("unexpected fixture route: %s", r.URL.Path)
				}
			})
			outcome, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit")
			awaitResetWorker(t, m, "codex-fixture")
			wantLists, wantConsumes := 2, 1
			if rejectedOperation == "consume" {
				wantLists, wantConsumes = 1, 2
			}
			if err != nil || outcome != "reset" || p.refreshes.Load() != 1 || p.rejected != "fixture-rejected" || lists != wantLists || consumes != wantConsumes {
				t.Fatalf("401 recovery failed: outcome=%s error=%v refreshes=%d rejected=%q lists=%d consumes=%d", outcome, err, p.refreshes.Load(), p.rejected, lists, consumes)
			}
		})
	}
}

func TestManualReset401RecoveryHasOneSharedRefreshBudget(t *testing.T) {
	for _, rejectedOperation := range []string{"list", "consume", "both"} {
		t.Run(rejectedOperation, func(t *testing.T) {
			m, p := resetCredentialFixture(t, "fixture-rejected", time.Now().Add(time.Hour).Unix())
			var lists, consumes int
			mockForwarding(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/backend-api/wham/rate-limit-reset-credits":
					lists++
					if rejectedOperation == "list" || rejectedOperation == "both" && r.Header.Get("Authorization") == "Bearer fixture-rejected" {
						return regressionResponse(401, `{}`), nil
					}
					return regressionResponse(200, resetCreditDocument("chosen-credit", "next-credit")), nil
				case "/backend-api/wham/rate-limit-reset-credits/consume":
					consumes++
					var body struct {
						CreditID string `json:"credit_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CreditID != "chosen-credit" {
						t.Errorf("retry advanced the credit: id=%q error=%v", body.CreditID, err)
					}
					return regressionResponse(401, `{}`), nil
				default:
					return nil, fmt.Errorf("unexpected fixture route: %s", r.URL.Path)
				}
			})
			_, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit")
			wantLists, wantConsumes := 2, 0
			if rejectedOperation == "consume" {
				wantLists, wantConsumes = 1, 2
			}
			if rejectedOperation == "both" {
				wantLists, wantConsumes = 2, 1
			}
			if err == nil || p.refreshes.Load() != 1 || lists != wantLists || consumes != wantConsumes || m.LastReset("codex-fixture") != nil {
				t.Fatalf("401 retries exceeded one recovery: error=%v refreshes=%d lists=%d consumes=%d", err, p.refreshes.Load(), lists, consumes)
			}
		})
	}
}

func TestManualResetDoesNotAdvanceCreditAfter401Relist(t *testing.T) {
	m, p := resetCredentialFixture(t, "fixture-rejected", time.Now().Add(time.Hour).Unix())
	var consumes int
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume" {
			consumes++
			return regressionResponse(200, `{"code":"reset"}`), nil
		}
		if r.Header.Get("Authorization") == "Bearer fixture-rejected" {
			return regressionResponse(401, `{}`), nil
		}
		return regressionResponse(200, resetCreditDocument("next-credit")), nil
	})
	_, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit")
	if !errors.Is(err, ErrNoResetCredits) || p.refreshes.Load() != 1 || consumes != 0 {
		t.Fatalf("missing pinned credit advanced redemption: error=%v refreshes=%d consumes=%d", err, p.refreshes.Load(), consumes)
	}
}

func TestManualResetDoesNotRetryUncertainConsumeFailure(t *testing.T) {
	m, p := resetCredentialFixture(t, "fixture-valid", time.Now().Add(time.Hour).Unix())
	var consumes int
	mockForwarding(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume" {
			consumes++
			return nil, errors.New("uncertain transport failure with http 401 text")
		}
		return regressionResponse(200, resetCreditDocument("chosen-credit", "next-credit")), nil
	})
	_, err := m.UseBankedReset(context.Background(), "codex-fixture", "chosen-credit")
	if err == nil || p.refreshes.Load() != 0 || consumes != 1 {
		t.Fatalf("uncertain outcome was replayed: error=%v refreshes=%d consumes=%d", err, p.refreshes.Load(), consumes)
	}
}
