package chatgptlogin

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var uuidHostID = regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ServerHostID returns the stable host ID stored independently of credentials.
// The file is published atomically so concurrent processes use the same ID.
func ServerHostID(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("data directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "chatgpt-host-id")
	read := func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(string(data))
		if !uuidHostID.MatchString(id) {
			return "", fmt.Errorf("invalid host ID in %s", path)
		}
		return id, nil
	}
	id, err := read()
	if err == nil || !os.IsNotExist(err) {
		return id, err
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	f, err := os.CreateTemp(dir, ".chatgpt-host-id-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = fmt.Fprintln(f, id); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Link(f.Name(), path); err != nil {
		if os.IsExist(err) {
			return read()
		}
		return "", err
	}
	return id, nil
}
