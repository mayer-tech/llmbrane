package debuglog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"
)

const filenameTimeFormat = "2006-01-02T15:04:05Z"

type requestKey struct{}

type requestInfo struct {
	requestID      string
	conversationID string
	fileID         string
	prefix         string
}

type logGroup struct {
	prefix   string
	lastUsed time.Time
}

// WithRequest establishes the log group and a unique ID for one inbound request.
// Reusing the returned context preserves the group across adapters and retries.
// A conversation ID reuses an existing group; otherwise the request gets its own.
func WithRequest(ctx context.Context, conversationID string) context.Context {
	if _, ok := ctx.Value(requestKey{}).(requestInfo); ok {
		return ctx
	}

	mu.Lock()
	defer mu.Unlock()
	if !enabled {
		return ctx
	}

	now := time.Now().UTC()
	info := requestInfo{
		requestID:      rand.Text(),
		conversationID: strings.TrimSpace(conversationID),
	}
	info.fileID = filenameID(firstNonEmpty(info.conversationID, info.requestID))
	group, ok := groups[info.fileID]
	if !ok {
		group.prefix = now.Format(filenameTimeFormat) + "_" + info.fileID
	}
	group.lastUsed = now
	groups[info.fileID] = group
	info.prefix = group.prefix
	return context.WithValue(ctx, requestKey{}, info)
}

// NewEntry creates an entry correlated with the request's log group.
// Without a request context, it creates a separate group for this entry.
func NewEntry(ctx context.Context, direction string) *Entry {
	ctx = WithRequest(ctx, "")
	info, _ := ctx.Value(requestKey{}).(requestInfo)
	return &Entry{
		Direction:      direction,
		RequestID:      info.requestID,
		ConversationID: info.conversationID,
		fileID:         info.fileID,
		filePrefix:     info.prefix,
	}
}

// filenameID preserves common client IDs and hashes anything unsafe or too long.
// The hash namespace is reserved to avoid collisions with literal client IDs.
func filenameID(id string) string {
	if validFilenameID(id) && !strings.HasPrefix(id, "sha256-") {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("sha256-%x", sum)
}

func validFilenameID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// restoreGroupsLocked recovers original timestamps from surviving log files.
// Legacy daily files and unrelated files are ignored. The caller must hold mu.
func restoreGroupsLocked() error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		stamp, rest, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			continue
		}
		if _, err := time.Parse(filenameTimeFormat, stamp); err != nil {
			continue
		}
		for _, direction := range []string{"inbound", "canonical", "outbound"} {
			id, ok := strings.CutSuffix(rest, "_"+direction+".jsonl")
			if !ok || !validFilenameID(id) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				break
			}
			prefix := stamp + "_" + id
			group, exists := groups[id]
			if !exists || prefix < group.prefix {
				group.prefix = prefix
			}
			if info.ModTime().After(group.lastUsed) {
				group.lastUsed = info.ModTime()
			}
			groups[id] = group
			break
		}
	}
	return nil
}
