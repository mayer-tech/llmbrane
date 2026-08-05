package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
)

func TestAcquireIsInactiveWithoutConfiguredOrObservedLimit(t *testing.T) {
	manager := NewManager()
	req := requestWithMessages("hello")

	reservation, estimate, err := manager.Acquire(context.Background(), "scope", "", Settings{}, req)
	if err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	if reservation == nil {
		t.Fatal("expected reservation")
	}
	if estimate.InputSource != "full_heuristic" {
		t.Fatalf("expected full heuristic input, got %q", estimate.InputSource)
	}
	if estimate.Output != 4096 || estimate.OutputSource != "reasoning_bootstrap" {
		t.Fatalf("unexpected default output estimate: %+v", estimate)
	}
	reservation.Cancel()
}

func TestObservedHeaderActivatesLocalBudget(t *testing.T) {
	manager := NewManager()
	settings := Settings{QueueTimeoutConfigured: true}
	req := requestWithMessages("hello")

	first, estimate, err := manager.Acquire(context.Background(), "scope", "", settings, req)
	if err != nil {
		t.Fatalf("unexpected initial acquire error: %v", err)
	}
	defer first.Cancel()

	manager.ObserveHeaders("scope", settings, http.Header{
		"X-RateLimit-Limit-Tokens": []string{itoa(estimate.Total())},
	})

	_, _, err = manager.Acquire(context.Background(), "scope", "", settings, req)
	queueErr, ok := err.(*QueueError)
	if !ok {
		t.Fatalf("expected QueueError after header activation, got %T: %v", err, err)
	}
	if queueErr.RetryAfter <= 0 {
		t.Fatalf("expected positive retry delay, got %s", queueErr.RetryAfter)
	}
}

func TestRemainingTokenHeaderDoesNotActivateBudget(t *testing.T) {
	manager := NewManager()
	manager.ObserveHeaders("scope", Settings{}, http.Header{
		"X-RateLimit-Remaining-Tokens": []string{"1"},
	})

	reservation, _, err := manager.Acquire(context.Background(), "scope", "", Settings{}, requestWithMessages("hello"))
	if err != nil {
		t.Fatalf("remaining header must not activate the queue: %v", err)
	}
	reservation.Cancel()
}

func TestEstimateUsesConversationContinuation(t *testing.T) {
	manager := NewManager()
	initial := requestWithMessages("initial request")

	first, _, err := manager.Acquire(context.Background(), "scope", "conversation", Settings{}, initial)
	if err != nil {
		t.Fatalf("unexpected initial acquire error: %v", err)
	}
	first.Finalize(&canonical.CanonicalUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}, "conversation")

	continued := &canonical.CanonicalChatRequest{
		Messages: append(append([]canonical.CanonicalMessage{}, initial.Messages...),
			canonical.CanonicalMessage{Role: canonical.RoleAssistant, Content: []canonical.CanonicalContentPart{{Type: "text", Text: "previous answer"}}},
			canonical.CanonicalMessage{Role: canonical.RoleUser, Content: []canonical.CanonicalContentPart{{Type: "text", Text: "follow-up"}}},
		),
	}
	second, estimate, err := manager.Acquire(context.Background(), "scope", "conversation", Settings{}, continued)
	if err != nil {
		t.Fatalf("unexpected continuation acquire error: %v", err)
	}
	defer second.Cancel()

	if estimate.InputSource != "conversation_continuation" {
		t.Fatalf("expected continuation estimate, got %q", estimate.InputSource)
	}
	if estimate.Input <= 100 {
		t.Fatalf("expected previous actual input plus incremental tokens, got %d", estimate.Input)
	}
}

func TestEstimateFallsBackWhenStaticPrefixChanges(t *testing.T) {
	manager := NewManager()
	initial := requestWithMessages("initial request")

	first, _, err := manager.Acquire(context.Background(), "scope", "conversation", Settings{}, initial)
	if err != nil {
		t.Fatalf("unexpected initial acquire error: %v", err)
	}
	first.Finalize(&canonical.CanonicalUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}, "conversation")

	changed := requestWithMessages("initial request")
	changed.Tools = []canonical.CanonicalTool{{Type: "function", Name: "new_tool"}}
	second, estimate, err := manager.Acquire(context.Background(), "scope", "conversation", Settings{}, changed)
	if err != nil {
		t.Fatalf("unexpected changed-prefix acquire error: %v", err)
	}
	defer second.Cancel()

	if estimate.InputSource != "full_heuristic" {
		t.Fatalf("expected full heuristic after static prefix change, got %q", estimate.InputSource)
	}
}

func TestOutputEstimatePriorities(t *testing.T) {
	manager := NewManager()
	maxTokens := 777
	req := requestWithMessages("hello")
	req.Params.MaxOutputTokens = &maxTokens

	reservation, estimate, err := manager.Acquire(context.Background(), "scope", "", Settings{OutputReservationTokens: 123}, req)
	if err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	defer reservation.Cancel()
	if estimate.Output != maxTokens || estimate.OutputSource != "client_max_output_tokens" {
		t.Fatalf("expected client max tokens priority, got %+v", estimate)
	}

	req.Params.MaxOutputTokens = nil
	high := "high"
	req.Params.ReasoningEffort = &high
	reservation, estimate, err = manager.Acquire(context.Background(), "another-scope", "", Settings{}, req)
	if err != nil {
		t.Fatalf("unexpected acquire error: %v", err)
	}
	defer reservation.Cancel()
	if estimate.Output != 8192 || estimate.OutputSource != "reasoning_bootstrap" {
		t.Fatalf("expected reasoning bootstrap, got %+v", estimate)
	}
}

func requestWithMessages(text string) *canonical.CanonicalChatRequest {
	return &canonical.CanonicalChatRequest{
		Messages: []canonical.CanonicalMessage{{
			Role:    canonical.RoleUser,
			Content: []canonical.CanonicalContentPart{{Type: "text", Text: text}},
		}},
	}
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}
