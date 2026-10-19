package canonical

import (
	"net/http"
	"testing"
)

func TestConversationIDFromHeaders(t *testing.T) {
	for _, test := range []struct {
		name         string
		conversation string
		interaction  string
		want         string
	}{
		{name: "missing"},
		{name: "conversation", conversation: "conversation-1", want: "conversation-1"},
		{name: "interaction fallback", interaction: "interaction-1", want: "interaction-1"},
		{name: "conversation takes precedence", conversation: "conversation-1", interaction: "interaction-1", want: "conversation-1"},
		{name: "whitespace", conversation: "  conversation-1  ", want: "conversation-1"},
		{name: "blank conversation falls back", conversation: "  ", interaction: " interaction-1 ", want: "interaction-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("X-Conversation-Id", test.conversation)
			headers.Set("X-Interaction-Id", test.interaction)
			if got := ConversationIDFromHeaders(headers); got != test.want {
				t.Fatalf("expected %q, got %q", test.want, got)
			}
		})
	}
}
