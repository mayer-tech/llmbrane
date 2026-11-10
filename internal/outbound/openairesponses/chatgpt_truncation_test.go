package openairesponses

import (
	"encoding/json"
	"testing"

	"github.com/mayer-tech/llmbrane/internal/canonical"
	"github.com/mayer-tech/llmbrane/internal/config"
)

func TestChatGPTNativeOmitsTruncation(t *testing.T) {
	for _, chatgpt := range []bool{false, true} {
		endpoint := config.Endpoint{}
		if chatgpt {
			endpoint.Auth = &config.EndpointAuth{Type: "chatgpt"}
		}
		req := &canonical.CanonicalChatRequest{Responses: &canonical.CanonicalResponsesRequest{Settings: map[string]json.RawMessage{"truncation": json.RawMessage(`"auto"`)}}}
		body, err := (&Adapter{}).nativeBody(req, "model", endpoint)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		_, present := fields["truncation"]
		if present == chatgpt {
			t.Errorf("chatgpt=%t: unexpected truncation field in %s", chatgpt, body)
		}
		if string(req.Responses.Settings["truncation"]) != `"auto"` {
			t.Fatal("request settings were modified")
		}
	}
}
