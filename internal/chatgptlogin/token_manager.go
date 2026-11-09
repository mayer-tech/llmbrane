package chatgptlogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const refreshWindow = time.Minute
const maxTokenResponse = 1 << 20

var tokenLocks sync.Map // absolute credential path -> *sync.Mutex

// TokenManager reads and refreshes a saved ChatGPT credential file. Refreshes are
// serialized between manager instances in this process; separate processes must
// not use the same credential file concurrently.
type TokenManager struct {
	path     string
	client   *http.Client
	endpoint string
}

// NewTokenManager constructs a manager for credentials saved by Login.
func NewTokenManager(path string, client *http.Client) (*TokenManager, error) {
	return newTokenManager(path, client, issuer+"/api/accounts/oauth/token")
}

func newTokenManager(path string, client *http.Client, endpoint string) (*TokenManager, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("credentials path is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Never forward credentials if an endpoint responds with a redirect.
	cloned := *client
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &TokenManager{path: abs, client: &cloned, endpoint: endpoint}, nil
}

// Token returns a usable bearer access token, refreshing credentials within one
// minute of expiry and atomically persisting rotated credentials with mode 0600.
func (m *TokenManager) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value, _ := tokenLocks.LoadOrStore(m.path, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return "", fmt.Errorf("read credentials: %w", err)
	}
	var creds credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", errors.New("invalid credentials file")
	}
	if err := validateCredentials(creds); err != nil {
		return "", err
	}
	if !hasPlanScope(creds.Scopes) {
		return "", errors.New("ChatGPT plan usage permission was not granted")
	}
	if time.Now().Before(creds.SavedAt.Add(time.Duration(creds.ExpiresIn)*time.Second - refreshWindow)) {
		return creds.AccessToken, nil
	}
	updated, err := m.refresh(ctx, creds)
	if err != nil {
		return "", err
	}
	if err := saveRefreshedCredentials(m.path, updated); err != nil {
		return "", fmt.Errorf("save refreshed credentials: %w", err)
	}
	if !hasPlanScope(updated.Scopes) {
		return "", errors.New("ChatGPT plan usage permission was not granted")
	}
	return updated.AccessToken, nil
}

func hasPlanScope(scopes []string) bool {
	for _, scope := range scopes {
		if scope == "chatgpt.tokens.use.direct" {
			return true
		}
	}
	return false
}

func validateCredentials(c credentials) error {
	if c.Issuer != issuer || c.Subject == "" || c.ClientID == "" || c.AccessToken == "" || c.RefreshToken == "" || !strings.EqualFold(c.TokenType, "Bearer") || c.ExpiresIn <= 0 || c.ExpiresIn > int64((365*24*time.Hour)/time.Second) || c.SavedAt.IsZero() || len(c.Scopes) == 0 {
		return errors.New("credentials are missing required fields or have invalid identity")
	}
	return nil
}

func (m *TokenManager) refresh(ctx context.Context, c credentials) (credentials, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {c.ClientID}, "refresh_token": {c.RefreshToken}, "resource": {resource}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return credentials{}, errors.New("could not create token refresh request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return credentials{}, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return credentials{}, fmt.Errorf("token refresh failed (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse+1))
	if err != nil || len(body) > maxTokenResponse {
		return credentials{}, errors.New("invalid or oversized token refresh response")
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return credentials{}, errors.New("invalid token refresh response")
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 {
		return credentials{}, errors.New("incomplete token refresh response")
	}
	c.AccessToken, c.TokenType, c.ExpiresIn, c.SavedAt = token.AccessToken, token.TokenType, token.ExpiresIn, time.Now().UTC()
	if token.RefreshToken != "" {
		c.RefreshToken = token.RefreshToken
	}
	// Keep the previously verified ID token; refresh responses are not identity-validated here.
	if token.Scope != "" {
		c.Scopes = strings.Fields(token.Scope)
	}
	return c, nil
}

func saveRefreshedCredentials(path string, c credentials) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".llmbrane-credentials-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
