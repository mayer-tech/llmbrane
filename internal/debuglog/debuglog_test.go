package debuglog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func initTestLogger(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := Init(d, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Init("", 0); err != nil {
			t.Error(err)
		}
	})
	return d
}

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries []Entry
	for _, block := range strings.Split(strings.TrimSpace(string(data)), "\n\n") {
		_, body, ok := strings.Cut(block, "\n")
		if !ok {
			t.Fatalf("missing JSON block in %s", path)
		}
		var entry Entry
		if err := json.Unmarshal([]byte(body), &entry); err != nil {
			t.Fatalf("invalid entry in %s: %v", path, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestConversationGroupsAppendAcrossDirections(t *testing.T) {
	d := initTestLogger(t)
	var requestIDs []string
	var prefix string
	for range 2 {
		ctx := WithRequest(context.Background(), "conversation-123")
		entry := NewEntry(ctx, "inbound")
		requestIDs = append(requestIDs, entry.RequestID)
		if prefix == "" {
			prefix = entry.filePrefix
		}
		for _, direction := range []string{"inbound", "canonical", "outbound"} {
			e := NewEntry(ctx, direction)
			if e.filePrefix != prefix || e.RequestID != entry.RequestID {
				t.Fatalf("direction %s lost request correlation", direction)
			}
			Write(e)
		}
	}
	if requestIDs[0] == "" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("expected different request IDs, got %v", requestIDs)
	}
	stamp, id, ok := strings.Cut(prefix, "_")
	if !ok || id != "conversation-123" {
		t.Fatalf("unexpected filename prefix %q", prefix)
	}
	if _, err := time.Parse(filenameTimeFormat, stamp); err != nil {
		t.Fatalf("invalid filename timestamp: %v", err)
	}
	files, err := os.ReadDir(d)
	if err != nil || len(files) != 3 {
		t.Fatalf("expected exactly three files, got %d: %v", len(files), err)
	}
	for _, direction := range []string{"inbound", "canonical", "outbound"} {
		entries := readEntries(t, filepath.Join(d, prefix+"_"+direction+".jsonl"))
		if len(entries) != 2 {
			t.Fatalf("expected two entries for %s, got %d", direction, len(entries))
		}
		for i, entry := range entries {
			if entry.RequestID != requestIDs[i] || entry.ConversationID != "conversation-123" || entry.Direction != direction {
				t.Fatalf("unexpected entry: %+v", entry)
			}
		}
	}
}

func TestRequestsWithoutConversationIDAreUnique(t *testing.T) {
	d := initTestLogger(t)
	const count = 50
	prefixes := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithRequest(context.Background(), "")
			entry := NewEntry(ctx, "inbound")
			prefixes <- entry.filePrefix
			Write(entry)
		}()
	}
	wg.Wait()
	close(prefixes)
	seen := make(map[string]bool)
	for prefix := range prefixes {
		if seen[prefix] {
			t.Fatalf("duplicate filename prefix %q", prefix)
		}
		seen[prefix] = true
		entries := readEntries(t, filepath.Join(d, prefix+"_inbound.jsonl"))
		if len(entries) != 1 || entries[0].RequestID == "" || entries[0].ConversationID != "" {
			t.Fatalf("unexpected ungrouped entries: %+v", entries)
		}
		if !strings.HasSuffix(prefix, "_"+entries[0].RequestID) {
			t.Fatalf("generated request ID not used in filename %q", prefix)
		}
	}
	if len(seen) != count {
		t.Fatalf("expected %d groups, got %d", count, len(seen))
	}
}

func TestConcurrentConversationRequestsShareGroup(t *testing.T) {
	d := initTestLogger(t)
	const count = 20
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := WithRequest(context.Background(), "shared")
			Write(NewEntry(ctx, "canonical"))
		}()
	}
	wg.Wait()
	files, err := os.ReadDir(d)
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one shared file, got %d: %v", len(files), err)
	}
	entries := readEntries(t, filepath.Join(d, files[0].Name()))
	if len(entries) != count {
		t.Fatalf("expected %d entries, got %d", count, len(entries))
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.RequestID == "" || seen[entry.RequestID] {
			t.Fatalf("missing or repeated request ID %q", entry.RequestID)
		}
		seen[entry.RequestID] = true
	}
}

func TestRestoreGroupFromExistingDirection(t *testing.T) {
	d := initTestLogger(t)
	const prefix = "2026-10-18T16:30:21Z_conversation_with_underscores"
	path := filepath.Join(d, prefix+"_outbound.jsonl")
	if err := os.WriteFile(path, []byte("existing content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Init(d, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx := WithRequest(context.Background(), "conversation_with_underscores")
	entry := NewEntry(ctx, "inbound")
	if entry.filePrefix != prefix {
		t.Fatalf("expected original prefix %q, got %q", prefix, entry.filePrefix)
	}
	Write(entry)
	Write(NewEntry(ctx, "outbound"))
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "existing content\n") || !strings.Contains(string(data), entry.RequestID) {
		t.Fatalf("existing file was not preserved and appended: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d, prefix+"_inbound.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestFilenameIDsAreSafeAndDistinct(t *testing.T) {
	d := initTestLogger(t)
	ids := []string{"../escape", "a/b", "a?b", strings.Repeat("x", 300), "sha256-reserved", "safe-ID_123"}
	seen := make(map[string]bool)
	for _, id := range ids {
		ctx := WithRequest(context.Background(), id)
		entry := NewEntry(ctx, "inbound")
		if !validFilenameID(entry.fileID) || seen[entry.fileID] {
			t.Fatalf("unsafe or colliding filename ID %q", entry.fileID)
		}
		seen[entry.fileID] = true
		if id == "safe-ID_123" && entry.fileID != id {
			t.Fatalf("safe client ID was not preserved: %q", entry.fileID)
		}
		Write(entry)
		entries := readEntries(t, filepath.Join(d, entry.filePrefix+"_inbound.jsonl"))
		if entries[0].ConversationID != id {
			t.Fatalf("original conversation ID was not preserved: %+v", entries[0])
		}
		if got := NewEntry(WithRequest(context.Background(), id), "outbound"); got.filePrefix != entry.filePrefix {
			t.Fatalf("unsafe ID did not reuse its group: %q", id)
		}
	}
}

func TestCleanupUsesModificationTime(t *testing.T) {
	d := initTestLogger(t)
	// A historical timestamp in a filename must not expire a freshly used file.
	fresh := filepath.Join(d, "2020-01-01T00:00:00Z_active_inbound.jsonl")
	stale := filepath.Join(d, "2020-01-01T00:00:00Z_expired_inbound.jsonl")
	legacy := filepath.Join(d, "inbound_2020-01-01.jsonl")
	runtime := filepath.Join(d, "runtime.log")
	for _, path := range []string{fresh, stale, legacy, runtime} {
		if err := os.WriteFile(path, []byte("log\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{stale, legacy, runtime} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := Init(d, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	cleanup()
	for _, path := range []string{stale, legacy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected expired file to be removed: %s (%v)", path, err)
		}
	}
	for _, path := range []string{fresh, runtime} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected file to survive cleanup: %s (%v)", path, err)
		}
	}
	mu.Lock()
	_, expiredExists := groups["expired"]
	_, activeExists := groups["active"]
	mu.Unlock()
	if expiredExists || !activeExists {
		t.Fatalf("unexpected retained groups: expired=%v active=%v", expiredExists, activeExists)
	}
}

func TestDisabledLoggerDoesNotCreateRequestContext(t *testing.T) {
	if err := Init("", 0); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := WithRequest(ctx, "conversation"); got != ctx {
		t.Fatal("disabled logging must not allocate a request context")
	}
	Write(&Entry{Direction: "inbound"})
	if Dir() != "" || Enabled() {
		t.Fatal("logger should be disabled")
	}
}
