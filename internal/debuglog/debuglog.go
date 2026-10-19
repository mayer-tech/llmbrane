// Package debuglog provides file-based debug logging for inbound, canonical,
// and outbound traffic. Pretty-printed JSON entries are grouped by conversation
// (or request) with separate files for each direction.
package debuglog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// sensitiveHeaders are header names whose values are redacted in debug logs.
var sensitiveHeaders = map[string]bool{
	"authorization": true,
	"x-api-key":     true,
	"api-key":       true,
	"x-auth-token":  true,
	"cookie":        true,
}

var (
	mu          sync.Mutex
	dir         string
	retention   time.Duration
	enabled     bool
	groups      = make(map[string]logGroup)
	cleanupOnce sync.Once
)

// Init configures the debug logger. If debugDir is empty, file logging is disabled.
// retentionDur specifies how long log files are kept (e.g. 24*time.Hour for "1d").
// Returns an error if the directory cannot be created or is not writable.
func Init(debugDir string, retentionDur time.Duration) error {
	mu.Lock()
	defer mu.Unlock()

	enabled = false
	dir = ""
	groups = make(map[string]logGroup)
	if debugDir == "" {
		return nil
	}

	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		return fmt.Errorf("debuglog: failed to create directory %s: %w", debugDir, err)
	}

	// Write test to verify the directory is writable
	testFile := filepath.Join(debugDir, ".write_test")
	if err := os.WriteFile(testFile, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("debuglog: directory %s is not writable: %w", debugDir, err)
	}
	os.Remove(testFile)

	dir = debugDir
	retention = retentionDur
	if err := restoreGroupsLocked(); err != nil {
		dir = ""
		return fmt.Errorf("debuglog: failed to restore log groups: %w", err)
	}
	enabled = true

	cleanupOnce.Do(func() { go cleanupLoop() })
	return nil
}

// Dir returns the configured debug log directory, or empty string if disabled.
func Dir() string {
	mu.Lock()
	defer mu.Unlock()
	return dir
}

// LogRuntime appends a timestamped message to the runtime log file.
// This is a no-op if debug logging is disabled.
func LogRuntime(msg string) {
	if !Enabled() {
		return
	}

	mu.Lock()
	d := dir
	mu.Unlock()

	f, err := os.OpenFile(filepath.Join(d, "runtime.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(f, "%s %s\n", ts, msg)
}

// Enabled reports whether debug file logging is active.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// Entry is a single debug log record.
type Entry struct {
	Timestamp       string            `json:"ts"`
	Direction       string            `json:"dir"`
	RequestID       string            `json:"request_id,omitempty"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	Method          string            `json:"method,omitempty"`
	Path            string            `json:"path,omitempty"`
	URL             string            `json:"url,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            json.RawMessage   `json:"body,omitempty"`
	Status          int               `json:"status,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
	ResponseBody    json.RawMessage   `json:"response_body,omitempty"`
	DurationMs      int64             `json:"duration_ms,omitempty"`
	StreamEvents    []json.RawMessage `json:"stream_events,omitempty"`
	Error           string            `json:"error,omitempty"`
	fileID          string
	filePrefix      string
}

// Write appends an entry to <first-request-timestamp>_<id>_<direction>.jsonl.
// Callers should use NewEntry to share a request's correlation and log group.
func Write(e *Entry) {
	if !Enabled() || e == nil {
		return
	}
	if e.Direction != "inbound" && e.Direction != "canonical" && e.Direction != "outbound" {
		return
	}
	if e.filePrefix == "" {
		ctx := WithRequest(context.Background(), e.ConversationID)
		correlated := NewEntry(ctx, e.Direction)
		e.fileID = correlated.fileID
		e.filePrefix = correlated.filePrefix
		if e.RequestID == "" {
			e.RequestID = correlated.RequestID
		}
	}

	now := time.Now().UTC()
	e.Timestamp = now.Format(time.RFC3339)

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return
	}

	header := fmt.Sprintf("// ===== %s %s %s %s =====\n",
		e.Timestamp, e.Direction, e.Method, firstNonEmpty(e.Path, e.URL))
	filename := e.filePrefix + "_" + e.Direction + ".jsonl"

	mu.Lock()
	defer mu.Unlock()
	if !enabled {
		return
	}

	f, err := os.OpenFile(filepath.Join(dir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	groups[e.fileID] = logGroup{prefix: e.filePrefix, lastUsed: now}
	f.WriteString(header)
	f.Write(data)
	f.WriteString("\n\n")
}

// RedactHeaders returns a copy of the given headers with sensitive values masked.
func RedactHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = "[REDACTED]"
		} else {
			out[k] = strings.Join(v, ", ")
		}
	}
	return out
}

// cleanupLoop periodically removes log files older than the retention period.
func cleanupLoop() {
	cleanup()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cleanup()
	}
}

// cleanup removes old log files and forgets inactive groups. Holding mu keeps
// cleanup from racing with appends, initialization, or group restoration.
func cleanup() {
	mu.Lock()
	defer mu.Unlock()

	if !enabled || dir == "" || retention <= 0 {
		return
	}

	cutoff := time.Now().Add(-retention)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
	for id, group := range groups {
		if group.lastUsed.Before(cutoff) {
			delete(groups, id)
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
