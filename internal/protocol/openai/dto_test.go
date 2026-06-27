package openai_test

import (
	"encoding/json"
	"testing"

	"moonbridge/internal/protocol/openai"
)

func TestResponseJSONIncludesFunctionCallAndCachedUsage(t *testing.T) {
	response := openai.Response{
		ID:     "resp_123",
		Object: "response",
		Status: "completed",
		Output: []openai.OutputItem{
			{
				Type:      "function_call",
				ID:        "fc_123",
				CallID:    "toolu_123",
				Name:      "lookup",
				Arguments: `{"id":"42"}`,
				Status:    "completed",
			},
		},
		Usage: openai.Usage{
			InputTokens:  100,
			OutputTokens: 10,
			InputTokensDetails: openai.InputTokensDetails{
				CachedTokens: 90,
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded["object"] != "response" {
		t.Fatalf("object = %v", decoded["object"])
	}
	usage := decoded["usage"].(map[string]any)
	details := usage["input_tokens_details"].(map[string]any)
	if details["cached_tokens"].(float64) != 90 {
		t.Fatalf("cached_tokens = %v", details["cached_tokens"])
	}
}

func TestResponseJSONIncludesZeroCachedTokensWhenDetailsPresent(t *testing.T) {
	response := openai.Response{
		ID:     "resp_123",
		Object: "response",
		Status: "completed",
		Output: []openai.OutputItem{},
		Usage: openai.Usage{
			InputTokens:  100,
			OutputTokens: 10,
			InputTokensDetails: openai.InputTokensDetails{
				CachedTokens: 0,
			},
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	usage := decoded["usage"].(map[string]any)
	details := usage["input_tokens_details"].(map[string]any)
	if _, ok := details["cached_tokens"]; !ok {
		t.Fatalf("cached_tokens missing from input_tokens_details: %#v", details)
	}
}

func TestResponseJSONAlwaysIncludesInputAndOutputTokens(t *testing.T) {
	// Regression: when a chat-protocol upstream reports no prompt_tokens,
	// InputTokens is 0. With omitempty the field was dropped from
	// response.completed, and codex failed with "missing field `input_tokens`",
	// treating it as a stream disconnect and retrying until it gave up.
	// input_tokens / output_tokens must always be present, even at 0.
	response := openai.Response{
		ID:     "resp_123",
		Object: "response",
		Status: "completed",
		Output: []openai.OutputItem{},
		Usage: openai.Usage{
			InputTokens:  0,
			OutputTokens: 71,
			TotalTokens:  71,
		},
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	usage, ok := decoded["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing: %#v", decoded["usage"])
	}
	if _, ok := usage["input_tokens"]; !ok {
		t.Fatalf("input_tokens missing from usage (must be present even when 0): %#v", usage)
	}
	if v, ok := usage["input_tokens"].(float64); !ok || v != 0 {
		t.Fatalf("input_tokens = %v, want 0", usage["input_tokens"])
	}
	if _, ok := usage["output_tokens"]; !ok {
		t.Fatalf("output_tokens missing from usage (must be present even when 0): %#v", usage)
	}
}

func TestToolJSONIncludesExplicitStrictFalse(t *testing.T) {
	strict := false
	tool := openai.Tool{
		Type:   "function",
		Name:   "lookup_weather",
		Strict: &strict,
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if value, ok := decoded["strict"]; !ok || value != false {
		t.Fatalf("strict = %v, present = %v; body = %s", value, ok, data)
	}
}
