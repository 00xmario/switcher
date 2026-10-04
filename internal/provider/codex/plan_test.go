package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"switcher/internal/store"
)

func TestUsageReportsCurrentFreePlanInsteadOfSavedPro(t *testing.T) {
	old := oauthHTTPClient
	t.Cleanup(func() { oauthHTTPClient = old })
	oauthHTTPClient = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/backend-api/codex/usage" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"plan_type":"free","rate_limit":{"primary_window":{"limit_window_seconds":2592000,"used_percent":0,"reset_at":2000000000}}}`))}, nil
	})}
	u, err := New().Usage(context.Background(), store.Account{Plan: "pro", Token: store.Token{AccessToken: "fixture-access"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(u)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result["plan"] != "free" {
		t.Fatalf("fresh free plan was discarded: %s", raw)
	}
	if len(u.Windows) != 1 || u.Windows[0].Label != "Monthly" {
		t.Fatal("free-tier quota was lost")
	}
}

func TestUsagePlanPresenceAndNormalization(t *testing.T) {
	old := oauthHTTPClient
	t.Cleanup(func() { oauthHTTPClient = old })
	for _, tc := range []struct{ body, plan string }{
		{`{"plan_type":" Free "}`, "free"},
		{`{"plan_type":"prolite"}`, "prolite"},
		{`{"plan_type":"go"}`, "go"},
		{`{"plan_type":""}`, ""},
		{`{}`, ""},
	} {
		oauthHTTPClient = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
		})}
		u, err := New().Usage(context.Background(), store.Account{Plan: "pro"})
		if err != nil || u.Plan != tc.plan || len(u.Windows) != 0 {
			t.Fatalf("body=%s plan=%q err=%v", tc.body, u.Plan, err)
		}
	}
}
