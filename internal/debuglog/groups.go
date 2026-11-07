package debuglog

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	identity       string
}

type logGroup struct {
	prefix   string
	lastUsed time.Time
	identity string
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
	id := firstNonEmpty(info.conversationID, info.requestID)
	info.identity = groupIdentity(id)
	info.fileID = groupFilenameID(id, info.identity)
	group, ok := groups[info.fileID]
	if !ok {
		group.prefix = now.Format(filenameTimeFormat) + "_" + info.fileID
	}
	group.lastUsed = now
	group.identity = info.identity
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
		groupIdentity:  info.identity,
	}
}

// groupIdentity retains the complete hash so abbreviated IDs can be disambiguated.
func groupIdentity(id string) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%x", sum)
}

func shortFilenameID(id string) string {
	return "h." + groupIdentity(id)[:7]
}

// groupFilenameID reuses existing groups before choosing the shortest available
// hash prefix. A group with an unknown identity reserves its filename as well.
// The caller must hold mu.
func groupFilenameID(id, identity string) string {
	legacyID := filenameID(id)
	if group, ok := groups[legacyID]; ok && (group.identity == "" || group.identity == identity) {
		return legacyID
	}
	// Reuse the earlier 96-bit base64 filenames when their identity is known.
	sum := sha256.Sum256([]byte(id))
	oldCompactID := "h." + base64.RawURLEncoding.EncodeToString(sum[:12])
	if group, ok := groups[oldCompactID]; ok && group.identity == identity {
		return oldCompactID
	}

	available := ""
	for n := 7; n <= len(identity); n++ {
		candidate := "h." + identity[:n]
		group, ok := groups[candidate]
		if ok && group.identity == identity {
			return candidate
		}
		if !ok && available == "" {
			available = candidate
		}
	}
	for candidate, group := range groups {
		if strings.HasPrefix(candidate, "h."+identity+"-") && group.identity == identity {
			return candidate
		}
	}
	if available != "" {
		return available
	}
	// All prefixes are occupied. This remains distinct even from unknown files.
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("h.%s-%d", identity, n)
		if group, ok := groups[candidate]; ok && group.identity == identity {
			return candidate
		} else if !ok {
			return candidate
		}
	}
}

func validShortFilenameID(id string) bool {
	if !strings.HasPrefix(id, "h.") {
		return false
	}
	suffix := id[2:]
	if len(suffix) == 16 {
		decoded, err := base64.RawURLEncoding.DecodeString(suffix)
		if err == nil && len(decoded) == 12 && base64.RawURLEncoding.EncodeToString(decoded) == suffix {
			return true
		}
	}
	hash, number, hasNumber := strings.Cut(suffix, "-")
	if len(hash) < 7 || len(hash) > 64 || (hasNumber && (len(hash) != 64 || number == "")) {
		return false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	for _, c := range number {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// filenameID reproduces legacy IDs so existing log groups can be reused.
// The hash namespace was reserved to avoid collisions with literal client IDs.
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
			if !ok || (!validFilenameID(id) && !validShortFilenameID(id)) {
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
			if group.identity == "" {
				group.identity = identityFromFile(filepath.Join(dir, entry.Name()))
			}
			groups[id] = group
			break
		}
	}
	return nil
}

// identityFromFile reads the first entry's IDs. Unreadable or incomplete logs
// reserve their filename without being assigned to an unrelated conversation.
func identityFromFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	if _, err := reader.ReadString('\n'); err != nil {
		return ""
	}
	var entry struct {
		RequestID      string `json:"request_id"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(reader).Decode(&entry); err != nil {
		return ""
	}
	id := firstNonEmpty(strings.TrimSpace(entry.ConversationID), entry.RequestID)
	if id == "" {
		return ""
	}
	return groupIdentity(id)
}
