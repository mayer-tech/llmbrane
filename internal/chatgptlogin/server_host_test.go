package chatgptlogin

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestServerHostIDConcurrentAndPersistent(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = ServerHostID(dir)
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] || !uuidHostID.MatchString(ids[i]) {
			t.Fatalf("host %d: id=%q err=%v", i, ids[i], errs[i])
		}
	}
	info, err := os.Stat(filepath.Join(dir, "chatgpt-host-id"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("host ID permissions: info=%v err=%v", info, err)
	}
}

func TestServerHostIDRejectsInvalidExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chatgpt-host-id")
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ServerHostID(dir); err == nil {
		t.Fatal("expected invalid host ID error")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "invalid" {
		t.Fatal("invalid file was changed")
	}
}
