package chatgptlogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func managerCredentials(expires time.Time) credentials {
	return credentials{Email: "test@example.com", Issuer: issuer, Subject: "subject", ClientID: "client", IDToken: "id", AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "Bearer", ExpiresIn: 3600, Scopes: []string{"chatgpt.tokens.use.direct"}, SavedAt: expires.Add(-time.Hour)}
}

func writeManagerCredentials(t *testing.T, c credentials) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestManager(t *testing.T, path, endpoint string) *TokenManager {
	t.Helper()
	if endpoint == "" {
		endpoint = issuer + "/api/accounts/oauth/token"
	}
	m, err := newTokenManager(path, nil, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTokenManagerCachedAndRefresh(t *testing.T) {
	t.Run("cached", func(t *testing.T) {
		path := writeManagerCredentials(t, managerCredentials(time.Now().Add(time.Hour)))
		m := newTestManager(t, path, "")
		token, err := m.Token(context.Background())
		if err != nil || token != "old-access" {
			t.Fatalf("Token() = %q, %v", token, err)
		}
	})
	t.Run("refresh", func(t *testing.T) {
		var requested bool
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requested = true
			if r.Method != http.MethodPost {
				t.Errorf("method = %s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "client" || r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("resource") != resource || r.Form.Has("scope") {
				t.Errorf("unexpected refresh form: %v", r.Form)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated","token_type":"Bearer","expires_in":3600,"scope":"chatgpt.tokens.use.direct"}`))
		}))
		defer s.Close()
		path := writeManagerCredentials(t, managerCredentials(time.Now().Add(-time.Hour)))
		token, err := newTestManager(t, path, s.URL).Token(context.Background())
		if err != nil || token != "new-access" || !requested {
			t.Fatalf("Token() = %q, %v; requested=%v", token, err, requested)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var saved credentials
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.RefreshToken != "rotated" || saved.Subject != "subject" || saved.Email != "test@example.com" || len(saved.Scopes) != 1 || saved.Scopes[0] != "chatgpt.tokens.use.direct" {
			t.Fatalf("unexpected persisted credentials")
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("permissions = %o", info.Mode().Perm())
		}
	})
}

func TestTokenManagerRefreshErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{"http": {401, `{"error":"invalid_grant"}`}, "invalid JSON": {200, `{`}, "incomplete": {200, `{"access_token":"x"}`}} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer s.Close()
			path := writeManagerCredentials(t, managerCredentials(time.Now().Add(-time.Hour)))
			if token, err := newTestManager(t, path, s.URL).Token(context.Background()); err == nil || token != "" {
				t.Fatalf("Token() = %q, %v", token, err)
			}
		})
	}
	if _, err := NewTokenManager("", nil); err == nil {
		t.Fatal("accepted empty credentials path")
	}
}

func TestTokenManagerSerializesAcrossInstances(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"shared-access","refresh_token":"shared-refresh","token_type":"Bearer","expires_in":3600}`))
	}))
	defer s.Close()
	path := writeManagerCredentials(t, managerCredentials(time.Now().Add(-time.Hour)))
	managers := []*TokenManager{newTestManager(t, path, s.URL), newTestManager(t, path, s.URL)}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, m := range managers {
		wg.Add(1)
		go func(m *TokenManager) {
			defer wg.Done()
			token, err := m.Token(context.Background())
			if err == nil && !strings.EqualFold(token, "shared-access") {
				err = context.Canceled
			}
			errs <- err
		}(m)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}
