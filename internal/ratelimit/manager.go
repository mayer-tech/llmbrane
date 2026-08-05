// Package ratelimit implements an optional in-memory TPM queue for one
// llmbrane instance. It never changes requests sent to the provider.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/logger"
)

const (
	windowDuration        = time.Minute
	defaultQueueTimeout   = 30 * time.Second
	maximumOutputSamples  = 50
	conversationRetention = 30 * time.Minute
)

// Settings configures one endpoint's optional local TPM queue.
type Settings struct {
	TokensPerMinute         int
	Utilization             float64
	QueueTimeout            time.Duration
	QueueTimeoutConfigured  bool
	OutputReservationTokens int
}

// Estimate describes the token reservation derived without modifying a model request.
type Estimate struct {
	Input        int
	Output       int
	InputSource  string
	OutputSource string
}

func (e Estimate) Total() int { return e.Input + e.Output }

// QueueError is returned when an active queue cannot admit a request in time.
type QueueError struct {
	RetryAfter time.Duration
	Message    string
}

func (e *QueueError) Error() string { return e.Message }

// Manager owns independent in-memory budgets for configured endpoint/model scopes.
type Manager struct {
	mu            sync.Mutex
	budgets       map[string]*budget
	conversations map[string]conversation
	outputSamples map[string][]int
}

type budget struct {
	settings      Settings
	learnedTotal  int
	learnedInput  int
	learnedOutput int
	events        map[uint64]*reservationEvent
	nextID        uint64
	changed       chan struct{}
}

type reservationEvent struct {
	created time.Time
	input   int
	output  int
}

type conversation struct {
	staticHash       string
	messageHashes    []string
	actualInputToken int
	updatedAt        time.Time
}

// Reservation is a local accounting handle. It is not sent to the provider.
type Reservation struct {
	manager *Manager
	scope   string
	id      uint64
	effort  string
	request requestFingerprint
}

type requestFingerprint struct {
	staticHash    string
	messageHashes []string
}

// NewManager returns a manager with no active limit. Limits become active only
// through Settings.TokensPerMinute or a provider response header.
func NewManager() *Manager {
	return &Manager{
		budgets:       make(map[string]*budget),
		conversations: make(map[string]conversation),
		outputSamples: make(map[string][]int),
	}
}

// Acquire waits for local budget, then records a reservation. It is a no-op
// when neither config nor observed provider headers expose a token limit.
func (m *Manager) Acquire(ctx context.Context, scope, conversationID string, settings Settings, req *canonical.CanonicalChatRequest) (*Reservation, Estimate, error) {
	fingerprint := makeFingerprint(req)

	m.mu.Lock()
	m.pruneConversationsLocked(time.Now())
	b := m.budgetLocked(scope, settings)
	estimate := m.estimateLocked(scope, conversationID, req, fingerprint, settings)
	limits := b.limitsLocked()
	if !limits.active() {
		b.nextID++
		id := b.nextID
		b.events[id] = &reservationEvent{created: time.Now(), input: estimate.Input, output: estimate.Output}
		m.mu.Unlock()
		logger.Info.Printf("TPM budget inactive scope=%q estimate_input=%d(%s) estimate_output=%d(%s)", scope, estimate.Input, estimate.InputSource, estimate.Output, estimate.OutputSource)
		return &Reservation{manager: m, scope: scope, id: id, effort: reasoningEffort(req), request: fingerprint}, estimate, nil
	}

	queueTimeout := settings.QueueTimeout
	if !settings.QueueTimeoutConfigured {
		queueTimeout = defaultQueueTimeout
	}
	queueDeadline := time.Now().Add(queueTimeout)

	for {
		now := time.Now()
		b.pruneLocked(now)
		used := b.usedLocked()
		if limits.fits(used, estimate) {
			b.nextID++
			id := b.nextID
			b.events[id] = &reservationEvent{created: now, input: estimate.Input, output: estimate.Output}
			remaining := limits.remaining(used.add(estimate))
			m.mu.Unlock()
			logger.Info.Printf("TPM budget reserved scope=%q input=%d(%s) output=%d(%s) used=%s remaining=%s limits=%s", scope, estimate.Input, estimate.InputSource, estimate.Output, estimate.OutputSource, used, remaining, limits)
			return &Reservation{manager: m, scope: scope, id: id, effort: reasoningEffort(req), request: fingerprint}, estimate, nil
		}

		if !limits.canFit(estimate) {
			m.mu.Unlock()
			return nil, estimate, &QueueError{
				RetryAfter: windowDuration,
				Message:    fmt.Sprintf("estimated request tokens (%d) exceed configured local TPM budget", estimate.Total()),
			}
		}

		waitFor := b.waitForLocked(now, used, estimate, limits)
		if waitFor <= 0 {
			waitFor = time.Millisecond
		}
		if now.Add(waitFor).After(queueDeadline) {
			m.mu.Unlock()
			return nil, estimate, &QueueError{
				RetryAfter: waitFor,
				Message:    "local TPM queue timeout exceeded",
			}
		}

		changed := b.changed
		m.mu.Unlock()
		logger.Info.Printf("TPM budget queueing scope=%q estimate_total=%d wait=%s used=%s limits=%s", scope, estimate.Total(), waitFor, used, limits)

		timer := time.NewTimer(waitFor)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, estimate, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}

		m.mu.Lock()
		b = m.budgetLocked(scope, settings)
		limits = b.limitsLocked()
		if !limits.active() {
			m.mu.Unlock()
			return &Reservation{manager: m, scope: scope, effort: reasoningEffort(req), request: fingerprint}, estimate, nil
		}
	}
}

// ObserveHeaders learns supported provider token limits. A configured TPM is
// never replaced; both sources are enforced conservatively when present.
func (m *Manager) ObserveHeaders(scope string, settings Settings, headers http.Header) {
	observed := parseTokenLimits(headers)
	if !observed.any() {
		return
	}

	m.mu.Lock()
	b := m.budgetLocked(scope, settings)
	changed := b.learnedTotal != observed.total || b.learnedInput != observed.input || b.learnedOutput != observed.output
	b.learnedTotal = observed.total
	b.learnedInput = observed.input
	b.learnedOutput = observed.output
	if changed {
		b.notifyLocked()
	}
	limits := b.limitsLocked()
	m.mu.Unlock()

	if changed {
		logger.Info.Printf("TPM budget learned from provider headers scope=%q limits=%s", scope, limits)
	}
}

// Finalize replaces a reservation estimate with actual provider usage when it
// is available and stores conversation/output history for later estimates.
func (r *Reservation) Finalize(usage *canonical.CanonicalUsage, conversationID string) {
	if r == nil || r.manager == nil || r.id == 0 {
		return
	}

	r.manager.mu.Lock()
	b := r.manager.budgets[r.scope]
	if b == nil {
		r.manager.mu.Unlock()
		return
	}
	event, ok := b.events[r.id]
	if !ok {
		r.manager.mu.Unlock()
		return
	}

	if usage != nil {
		actualInput, actualOutput, ok := actualUsage(usage, event.input, event.output)
		if ok {
			event.input = actualInput
			event.output = actualOutput
			if actualOutput > 0 {
				r.manager.recordOutputSampleLocked(r.scope, r.effort, actualOutput)
			}
			if conversationID != "" && usage.InputTokens > 0 {
				r.manager.conversations[conversationKey(r.scope, conversationID)] = conversation{
					staticHash:       r.request.staticHash,
					messageHashes:    append([]string(nil), r.request.messageHashes...),
					actualInputToken: usage.InputTokens,
					updatedAt:        time.Now(),
				}
			}
		}
	}
	b.notifyLocked()
	actualInput := event.input
	actualOutput := event.output
	used := b.usedLocked()
	limits := b.limitsLocked()
	r.manager.mu.Unlock()

	if usage != nil {
		logger.Info.Printf("TPM budget finalized scope=%q actual_input=%d actual_output=%d used=%s limits=%s", r.scope, actualInput, actualOutput, used, limits)
	}
}

// Cancel removes a reservation when no provider request was accepted.
func (r *Reservation) Cancel() {
	if r == nil || r.manager == nil || r.id == 0 {
		return
	}
	r.manager.mu.Lock()
	if b := r.manager.budgets[r.scope]; b != nil {
		if _, ok := b.events[r.id]; ok {
			delete(b.events, r.id)
			b.notifyLocked()
		}
	}
	r.manager.mu.Unlock()
}

func (m *Manager) budgetLocked(scope string, settings Settings) *budget {
	b := m.budgets[scope]
	if b == nil {
		b = &budget{events: make(map[uint64]*reservationEvent), changed: make(chan struct{})}
		m.budgets[scope] = b
	}
	b.settings = settings
	return b
}

func (m *Manager) pruneConversationsLocked(now time.Time) {
	for key, conversation := range m.conversations {
		if now.Sub(conversation.updatedAt) >= conversationRetention {
			delete(m.conversations, key)
		}
	}
}

func (m *Manager) estimateLocked(scope, conversationID string, req *canonical.CanonicalChatRequest, fingerprint requestFingerprint, settings Settings) Estimate {
	output, outputSource := m.outputEstimateLocked(scope, req, settings)
	fullInput := estimateStatic(req) + estimateMessages(req.Messages)
	if conversationID == "" {
		return Estimate{Input: withMargin(fullInput), Output: output, InputSource: "full_heuristic", OutputSource: outputSource}
	}

	previous, ok := m.conversations[conversationKey(scope, conversationID)]
	if !ok || previous.staticHash != fingerprint.staticHash || !isPrefix(previous.messageHashes, fingerprint.messageHashes) {
		return Estimate{Input: withMargin(fullInput), Output: output, InputSource: "full_heuristic", OutputSource: outputSource}
	}

	addedMessages := req.Messages[len(previous.messageHashes):]
	increment := estimateMessages(addedMessages)
	return Estimate{
		Input:        previous.actualInputToken + withMargin(increment),
		Output:       output,
		InputSource:  "conversation_continuation",
		OutputSource: outputSource,
	}
}

func (m *Manager) outputEstimateLocked(scope string, req *canonical.CanonicalChatRequest, settings Settings) (int, string) {
	if req.Params.MaxOutputTokens != nil && *req.Params.MaxOutputTokens > 0 {
		return *req.Params.MaxOutputTokens, "client_max_output_tokens"
	}
	if settings.OutputReservationTokens > 0 {
		return settings.OutputReservationTokens, "configured_output_reservation"
	}

	bootstrap := max(4096, canonical.ReasoningTokenBudget(reasoningEffort(req)))
	samples := m.outputSamples[outputSampleKey(scope, reasoningEffort(req))]
	if len(samples) == 0 {
		return bootstrap, "reasoning_bootstrap"
	}
	learned := percentile(samples, 0.95)
	if learned > bootstrap {
		return learned, "learned_output_p95"
	}
	return bootstrap, "reasoning_bootstrap"
}

func (m *Manager) recordOutputSampleLocked(scope, effort string, tokens int) {
	key := outputSampleKey(scope, effort)
	samples := append(m.outputSamples[key], tokens)
	if len(samples) > maximumOutputSamples {
		samples = samples[len(samples)-maximumOutputSamples:]
	}
	m.outputSamples[key] = samples
}

func (b *budget) pruneLocked(now time.Time) {
	for id, event := range b.events {
		if now.Sub(event.created) >= windowDuration {
			delete(b.events, id)
		}
	}
}

func (b *budget) usedLocked() usage {
	var used usage
	for _, event := range b.events {
		used.input += event.input
		used.output += event.output
	}
	return used
}

func (b *budget) waitForLocked(now time.Time, used usage, estimate Estimate, limits limits) time.Duration {
	events := make([]*reservationEvent, 0, len(b.events))
	for _, event := range b.events {
		events = append(events, event)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].created.Before(events[j].created) })
	for _, event := range events {
		used.input -= event.input
		used.output -= event.output
		if limits.fits(used, estimate) {
			return time.Until(event.created.Add(windowDuration))
		}
	}
	return windowDuration
}

func (b *budget) limitsLocked() limits {
	utilization := b.settings.Utilization
	if utilization == 0 {
		utilization = 1
	}
	return limits{
		total:  applyUtilization(minPositive(b.settings.TokensPerMinute, b.learnedTotal), utilization),
		input:  applyUtilization(b.learnedInput, utilization),
		output: applyUtilization(b.learnedOutput, utilization),
	}
}

func (b *budget) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

type usage struct {
	input  int
	output int
}

func (u usage) total() int { return u.input + u.output }
func (u usage) add(e Estimate) usage {
	return usage{input: u.input + e.Input, output: u.output + e.Output}
}
func (u usage) String() string {
	return fmt.Sprintf("input=%d output=%d total=%d", u.input, u.output, u.total())
}

type limits struct {
	total  int
	input  int
	output int
}

func (l limits) active() bool { return l.total > 0 || l.input > 0 || l.output > 0 }
func (l limits) fits(used usage, estimate Estimate) bool {
	return (l.total == 0 || used.total()+estimate.Total() <= l.total) &&
		(l.input == 0 || used.input+estimate.Input <= l.input) &&
		(l.output == 0 || used.output+estimate.Output <= l.output)
}
func (l limits) canFit(estimate Estimate) bool {
	return (l.total == 0 || estimate.Total() <= l.total) &&
		(l.input == 0 || estimate.Input <= l.input) &&
		(l.output == 0 || estimate.Output <= l.output)
}
func (l limits) remaining(used usage) remainingBudget {
	return remainingBudget{
		input:  remaining(l.input, used.input),
		output: remaining(l.output, used.output),
		total:  remaining(l.total, used.total()),
	}
}
func (l limits) String() string {
	return fmt.Sprintf("input=%d output=%d total=%d", l.input, l.output, l.total)
}
func remaining(limit, used int) int {
	if limit == 0 {
		return 0
	}
	return max(0, limit-used)
}

type remainingBudget struct {
	input  int
	output int
	total  int
}

func (r remainingBudget) String() string {
	return fmt.Sprintf("input=%d output=%d total=%d", r.input, r.output, r.total)
}

func (o observedLimits) any() bool { return o.total > 0 || o.input > 0 || o.output > 0 }

type observedLimits struct{ total, input, output int }

func parseTokenLimits(headers http.Header) observedLimits {
	var observed observedLimits
	for name, values := range headers {
		lowerName := strings.ToLower(name)
		isRateLimit := strings.Contains(lowerName, "ratelimit") || strings.Contains(lowerName, "rate-limit")
		isLimit := strings.Contains(lowerName, "-limit-") || strings.HasSuffix(lowerName, "-limit")
		if !isRateLimit || !strings.Contains(lowerName, "token") || !isLimit {
			continue
		}
		value := firstPositive(values)
		if value == 0 {
			continue
		}
		switch {
		case strings.Contains(lowerName, "input"):
			observed.input = chooseLimit(observed.input, value)
		case strings.Contains(lowerName, "output"):
			observed.output = chooseLimit(observed.output, value)
		default:
			observed.total = chooseLimit(observed.total, value)
		}
	}
	return observed
}

func firstPositive(values []string) int {
	for _, value := range values {
		if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}
func chooseLimit(current, next int) int {
	if current == 0 || next < current {
		return next
	}
	return current
}
func minPositive(values ...int) int {
	result := 0
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}
func applyUtilization(value int, utilization float64) int {
	if value == 0 {
		return 0
	}
	return int(math.Floor(float64(value) * utilization))
}

func makeFingerprint(req *canonical.CanonicalChatRequest) requestFingerprint {
	static := struct {
		System         []canonical.CanonicalContentPart   `json:"system,omitempty"`
		Tools          []canonical.CanonicalTool          `json:"tools,omitempty"`
		ToolChoice     *canonical.CanonicalToolChoice     `json:"tool_choice,omitempty"`
		ResponseFormat *canonical.CanonicalResponseFormat `json:"response_format,omitempty"`
	}{req.System, req.Tools, req.ToolChoice, req.ResponseFormat}
	fingerprint := requestFingerprint{staticHash: hashJSON(static)}
	for _, message := range req.Messages {
		fingerprint.messageHashes = append(fingerprint.messageHashes, hashJSON(message))
	}
	return fingerprint
}

func hashJSON(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func isPrefix(prefix, values []string) bool {
	if len(prefix) > len(values) {
		return false
	}
	for i := range prefix {
		if prefix[i] != values[i] {
			return false
		}
	}
	return true
}
func conversationKey(scope, conversationID string) string { return scope + "\x00" + conversationID }
func outputSampleKey(scope, effort string) string         { return scope + "\x00" + effort }

func estimateStatic(req *canonical.CanonicalChatRequest) int {
	tokens := 0
	for _, part := range req.System {
		tokens += 12 + estimateContentPart(part)
	}
	for _, tool := range req.Tools {
		tokens += 20 + estimateJSON(tool)
	}
	if req.ToolChoice != nil {
		tokens += 10 + estimateJSON(req.ToolChoice)
	}
	if req.ResponseFormat != nil {
		tokens += 20 + estimateJSON(req.ResponseFormat)
	}
	return tokens
}
func estimateMessages(messages []canonical.CanonicalMessage) int {
	tokens := 0
	for _, message := range messages {
		tokens += 8 + estimateText(string(message.Role)) + estimateText(message.Name) + estimateText(message.ToolCallID)
		for _, part := range message.Content {
			tokens += 4 + estimateContentPart(part)
		}
	}
	return tokens
}
func estimateContentPart(part canonical.CanonicalContentPart) int {
	switch part.Type {
	case "text", "reasoning":
		if part.Reasoning != nil {
			return estimateText(part.Reasoning.Content)
		}
		return estimateText(part.Text)
	case "image":
		return 1024 + estimateText(part.Image.URL)
	case "audio":
		if part.Audio != nil {
			return max(1024, len(part.Audio.Data)/3)
		}
	case "file":
		if part.File != nil {
			return max(256, len(part.File.Data)/3)
		}
	case "tool_call":
		if part.ToolCall != nil {
			return 12 + estimateText(part.ToolCall.Name) + estimateBytes(part.ToolCall.Arguments)
		}
	case "tool_result":
		if part.ToolResult != nil {
			tokens := 12 + estimateText(part.ToolResult.ToolCallID)
			for _, result := range part.ToolResult.Content {
				tokens += estimateContentPart(result)
			}
			return tokens
		}
	}
	return estimateText(part.Text) + estimateJSON(part.Data)
}
func estimateJSON(value any) int {
	if value == nil {
		return 0
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return estimateBytes(data)
}
func estimateBytes(data []byte) int { return estimateText(string(data)) }
func estimateText(text string) int {
	if text == "" {
		return 0
	}
	ascii, nonASCII := 0, 0
	for _, r := range text {
		if r <= 127 {
			ascii++
		} else {
			nonASCII++
		}
	}
	// This deliberately overestimates mixed prose, JSON, and source code. The
	// queue can be conservative because it does not impose a model token cap.
	estimate := int(math.Ceil(float64(ascii)/3.0 + float64(nonASCII)*1.5))
	if estimate == 0 && utf8.RuneCountInString(text) > 0 {
		return 1
	}
	return estimate
}
func withMargin(tokens int) int {
	if tokens == 0 {
		return 32
	}
	return int(math.Ceil(float64(tokens)*1.15)) + 32
}
func reasoningEffort(req *canonical.CanonicalChatRequest) string {
	if req.Params.ReasoningEffort == nil {
		return ""
	}
	return *req.Params.ReasoningEffort
}
func actualUsage(usage *canonical.CanonicalUsage, fallbackInput, fallbackOutput int) (int, int, bool) {
	if usage == nil || usage.TotalTokens <= 0 {
		return fallbackInput, fallbackOutput, false
	}
	input := usage.InputTokens
	if input <= 0 {
		input = fallbackInput
	}
	output := usage.TotalTokens - usage.InputTokens
	if output < 0 {
		output = usage.OutputTokens
	}
	if output <= 0 && usage.OutputTokens > 0 {
		output = usage.OutputTokens
	}
	if output < 0 {
		output = fallbackOutput
	}
	return input, output, true
}
func percentile(samples []int, p float64) int {
	values := append([]int(nil), samples...)
	sort.Ints(values)
	index := int(math.Ceil(p*float64(len(values)))) - 1
	index = max(0, min(index, len(values)-1))
	return values[index]
}
