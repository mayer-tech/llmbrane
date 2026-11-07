package anthropic

import (
	"encoding/json"
	"testing"
)

func TestTranslateOutputLimit(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		present    bool
		want       int
	}{
		{"absent", `{}`, false, 0},
		{"null", `{"max_tokens":null}`, false, 0},
		{"zero", `{"max_tokens":0}`, true, 0},
		{"positive", `{"max_tokens":1000}`, true, 1000},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var req MessagesRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			got, err := (&Handler{}).translateRequest(&req)
			if err != nil {
				t.Fatal(err)
			}
			if (got.Params.MaxOutputTokens != nil) != tt.present || got.Params.MaxOutputTokens != nil && *got.Params.MaxOutputTokens != tt.want {
				t.Fatalf("limit = %v, want %d (present=%t)", got.Params.MaxOutputTokens, tt.want, tt.present)
			}
		})
	}
}
