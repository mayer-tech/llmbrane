// Package debuglog provides file-based debug logging for inbound and outbound
// HTTP traffic. Entries are written as pretty-printed JSON blocks separated by
// a comment header line, one file per direction and day.
package debuglog

import (
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
	mu        sync.Mutex
	dir       string
	retention time.Duration
	enabled   bool
)

// Init configures the debug logger. If debugDir is empty, file logging is disabled.
// retentionDur specifies how long log files are kept (e.g. 24*time.Hour for "1d").
func Init(debugDir string, retentionDur time.Duration) {
	mu.Lock()
	defer mu.Unlock()

	if debugDir == "" {
		enabled = false
		return
	}

	if err := os.MkdirAll(debugDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "debuglog: failed to create directory %s: %v\n", debugDir, err)
		enabled = false
		return
	}

	dir = debugDir
	retention = retentionDur
	enabled = true

	go cleanupLoop()
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
}

// Write appends a pretty-printed entry to the appropriate daily log file.
func Write(e *Entry) {
	if !Enabled() {
		return
	}

	e.Timestamp = time.Now().UTC().Format(time.RFC3339)

	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return
	}

	header := fmt.Sprintf("// ===== %s %s %s %s =====\n",
		e.Timestamp, e.Direction, e.Method, firstNonEmpty(e.Path, e.URL))

	filename := fmt.Sprintf("%s_%s.jsonl", e.Direction, time.Now().UTC().Format("2006-01-02"))

	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(filepath.Join(dir, filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

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

// cleanup removes log files older than the retention period.
func cleanup() {
	mu.Lock()
	d := dir
	r := retention
	mu.Unlock()

	if d == "" || r <= 0 {
		return
	}

	cutoff := time.Now().Add(-r)
	entries, err := os.ReadDir(d)
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
			os.Remove(filepath.Join(d, entry.Name()))
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
