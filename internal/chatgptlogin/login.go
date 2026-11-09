// Package chatgptlogin implements the local ChatGPT plan authorization flow.
package chatgptlogin

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	issuer       = "https://auth.openai.com"
	resource     = "https://api.openai.com/v1"
	callbackPath = "/auth/callback"
)

type credentials struct {
	Email        string    `json:"email,omitempty"`
	Issuer       string    `json:"issuer"`
	Subject      string    `json:"subject"`
	ClientID     string    `json:"client_id"`
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int64     `json:"expires_in"`
	Scopes       []string  `json:"scopes"`
	SavedAt      time.Time `json:"saved_at"`
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

func randomValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hostID persists the local host identifier separately from transferable credentials.
func hostID() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "llmbrane")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "chatgpt-host-id")
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id = strings.TrimSpace(string(data))
		if !strings.HasPrefix(id, "urn:uuid:") {
			return "", fmt.Errorf("invalid saved host ID")
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	_, err = io.WriteString(f, id+"\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

type callback struct {
	code, clientID string
	err            error
}

// Login performs a new registration and writes the resulting credentials.
func Login(ctx context.Context, output string, in io.Reader, out io.Writer) error {
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("-o is required")
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("output file already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	host, err := hostID()
	if err != nil {
		return fmt.Errorf("host ID: %w", err)
	}
	state, err := randomValue(32)
	if err != nil {
		return err
	}
	nonce, err := randomValue(32)
	if err != nil {
		return err
	}
	verifier, err := randomValue(32)
	if err != nil {
		return err
	}
	challenge := sha256.Sum256([]byte(verifier))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + callbackPath
	result := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		if len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid state", http.StatusBadRequest)
			return
		}
		res := callback{}
		if q.Get("error") != "" {
			res.err = fmt.Errorf("authorization was denied or failed")
		} else if len(q["code"]) != 1 || len(q["client_id"]) != 1 || q.Get("code") == "" || q.Get("client_id") == "" || q.Get("client_id") == "dynamic_agent_client" {
			res.err = fmt.Errorf("incomplete authorization callback")
		} else {
			res.code, res.clientID = q.Get("code"), q.Get("client_id")
		}
		select {
		case result <- res:
			fmt.Fprint(w, "Authorization received. Return to the terminal.")
		default:
			http.Error(w, "Callback already received", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	values := url.Values{"client_id": {"dynamic_agent_client"}, "agent_name_hint": {"LLMbrane"}, "ext_agent_host_id": {host}, "response_type": {"code"}, "redirect_uri": {redirect}, "scope": {"openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"}, "resource": {resource}, "state": {state}, "nonce": {nonce}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}}
	fmt.Fprintf(out, "Open this URL in a browser on this computer:\n%s/api/accounts/authorize?%s\n", issuer, values.Encode())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var cb callback
	select {
	case cb = <-result:
	case <-ctx.Done():
		return fmt.Errorf("login cancelled or timed out: %w", ctx.Err())
	}
	if cb.err != nil {
		return cb.err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {cb.clientID}, "code": {cb.code}, "code_verifier": {verifier}, "redirect_uri": {redirect}, "resource": {resource}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/api/accounts/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("token exchange failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token exchange failed (HTTP %d)", resp.StatusCode)
	}
	var token tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return fmt.Errorf("invalid token response")
	}
	savedAt := time.Now().UTC()
	if token.IDToken == "" || token.AccessToken == "" || token.RefreshToken == "" || !strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 {
		return fmt.Errorf("incomplete token response")
	}
	scopes := strings.Fields(token.Scope)
	allowed := false
	for _, s := range scopes {
		if s == "chatgpt.tokens.use.direct" {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("ChatGPT plan usage permission was not granted")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return fmt.Errorf("OpenID discovery failed: %w", err)
	}
	id, err := provider.Verifier(&oidc.Config{ClientID: cb.clientID}).Verify(ctx, token.IDToken)
	if err != nil {
		return fmt.Errorf("ID token verification failed: %w", err)
	}
	var claims struct {
		Nonce    string `json:"nonce"`
		Email    string `json:"email"`
		IssuedAt *int64 `json:"iat"`
	}
	if err := id.Claims(&claims); err != nil || claims.Nonce != nonce || id.Subject == "" || claims.IssuedAt == nil {
		return fmt.Errorf("invalid ID token identity or nonce")
	}
	fmt.Fprintf(out, "Verified account: %s (subject: %s)\nSave credentials? [y/N] ", claims.Email, id.Subject)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if strings.TrimSpace(strings.ToLower(line)) != "y" {
		return fmt.Errorf("login cancelled")
	}
	creds := credentials{Email: claims.Email, Issuer: issuer, Subject: id.Subject, ClientID: cb.clientID, IDToken: token.IDToken, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, ExpiresIn: token.ExpiresIn, Scopes: scopes, SavedAt: savedAt}
	return saveCredentials(output, creds)
}

func saveCredentials(path string, creds credentials) error {
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".llmbrane-credentials-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Linking atomically publishes the file without overwriting existing credentials.
	if err := os.Link(name, path); err != nil {
		return fmt.Errorf("cannot save credentials: %w", err)
	}
	return nil
}
