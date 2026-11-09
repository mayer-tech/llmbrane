package chatgptlogin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerHostIDLocalFallback(t *testing.T) {
	const localID = "urn:uuid:12345678-1234-4234-8234-123456789abc"
	const serverID = "urn:uuid:87654321-4321-4321-8321-cba987654321"
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	localDir := filepath.Join(configDir, "llmbrane")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(localDir, "chatgpt-host-id")
	if err := os.WriteFile(localFile, []byte(localID+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	id, err := ServerHostID(dataDir)
	if err != nil || id != localID {
		t.Fatalf("fallback: %q %v", id, err)
	}
	dataPath := filepath.Join(dataDir, "chatgpt-host-id")
	data, err := os.ReadFile(dataPath)
	if err != nil || string(data) != localID+"\n" {
		t.Fatalf("persisted fallback: %q %v", data, err)
	}
	if err := os.WriteFile(dataPath, []byte(serverID), 0600); err != nil {
		t.Fatal(err)
	}
	id, err = ServerHostID(dataDir)
	if err != nil || id != serverID {
		t.Fatalf("data precedence: %q %v", id, err)
	}
}

func TestServerHostIDInvalidLocalFallback(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	localDir := filepath.Join(configDir, "llmbrane")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "chatgpt-host-id"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if _, err := ServerHostID(dataDir); err == nil {
		t.Fatal("expected invalid host ID error")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "chatgpt-host-id")); !os.IsNotExist(err) {
		t.Fatalf("unexpected data host ID: %v", err)
	}
}
