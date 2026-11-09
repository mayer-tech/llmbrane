package chatgptlogin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	want := credentials{Subject: "user", AccessToken: "secret", ClientID: "client"}
	if err := saveCredentials(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions = %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got credentials
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Subject != want.Subject || got.AccessToken != want.AccessToken {
		t.Fatal("credentials do not match")
	}
	if err := saveCredentials(path, credentials{Subject: "other"}); err == nil {
		t.Fatal("existing credentials overwritten")
	}
	data2, err := os.ReadFile(path)
	if err != nil || string(data2) != string(data) {
		t.Fatal("existing file changed")
	}
}

func TestLoginRequiresOutput(t *testing.T) {
	if err := Login(context.Background(), "", strings.NewReader(""), os.Stdout); err == nil {
		t.Fatal("expected missing output error")
	}
}

func TestHostIDStable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	a, err := hostID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := hostID()
	if err != nil {
		t.Fatal(err)
	}
	if a != b || !strings.HasPrefix(a, "urn:uuid:") {
		t.Fatalf("host ID was not stable: %q %q", a, b)
	}
}
