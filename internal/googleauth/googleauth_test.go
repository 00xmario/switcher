package googleauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"switcher/internal/provider"
)

func TestTokenExchangePostsSecretAndParses(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
	}))
	defer srv.Close()

	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	tok, err := TokenExchange(context.Background(), "secret", "code", "client-id", "http://localhost/cb")
	if err != nil {
		t.Fatal(err)
	}
	if gotForm.Get("client_secret") != "secret" || gotForm.Get("code") != "code" ||
		gotForm.Get("client_id") != "client-id" || gotForm.Get("redirect_uri") != "http://localhost/cb" ||
		gotForm.Get("grant_type") != "authorization_code" {
		t.Fatalf("unexpected form: %v", gotForm)
	}
	if tok.AccessToken != "at" || tok.RefreshToken != "rt" || tok.ExpiresIn != 3600 {
		t.Fatalf("unexpected token: %+v", tok)
	}
	if tok.ExpiresAt() <= 0 {
		t.Fatalf("ExpiresAt not derived from expires_in")
	}
}

func TestRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("refresh_token") != "rt" || r.PostForm.Get("grant_type") != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "expires_in": 60})
	}))
	defer srv.Close()

	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	tok, err := RefreshToken(context.Background(), "secret", "rt", "client-id")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at2" || tok.ExpiresIn != 60 {
		t.Fatalf("unexpected token: %+v", tok)
	}
}

func TestRefreshTokenRejectionOnlyOnRefresh(t *testing.T) {
	status, body := http.StatusBadRequest, `{"error":"invalid_grant","secret":"do-not-log"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	old := TokenURL
	TokenURL = srv.URL
	defer func() { TokenURL = old }()

	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusUnauthorized, ``, true},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{http.StatusForbidden, `{"error":"invalid_grant"}`, false},
		{http.StatusServiceUnavailable, `{"error":"invalid_grant"}`, false},
		{http.StatusOK, `{not json`, false},
	} {
		status, body = tc.status, tc.body
		_, err := RefreshToken(context.Background(), "secret", "rt", "client")
		if err == nil || errors.Is(err, provider.ErrReloginRequired) != tc.want {
			t.Fatalf("status %d body %q: err = %v, relogin = %t", status, body, err, tc.want)
		}
	}
	status, body = http.StatusBadRequest, `{"error":"invalid_grant","secret":"do-not-log"}`
	_, err := RefreshToken(context.Background(), "secret", "rt", "client")
	if !errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("refresh error leaked or lost classification: %v", err)
	}
	_, err = TokenExchange(context.Background(), "secret", "code", "client", "callback")
	if errors.Is(err, provider.ErrReloginRequired) || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("login exchange leaked or classified refresh failure: %v", err)
	}
}

func TestUserinfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at" {
			t.Fatalf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"email":"me@example.com"}`))
	}))
	defer srv.Close()

	old := UserinfoURL
	UserinfoURL = srv.URL
	defer func() { UserinfoURL = old }()

	email, err := Userinfo(context.Background(), "at")
	if err != nil {
		t.Fatal(err)
	}
	if email != "me@example.com" {
		t.Fatalf("email = %q", email)
	}
}
