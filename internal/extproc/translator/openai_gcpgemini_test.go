// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"bytes"
	"encoding/json"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/go-cmp/cmp"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

func TestOpenAIToGCPGeminiTranslator_RequestBody(t *testing.T) {
	testCases := []struct {
		name      string
		input     *openai.ChatCompletionRequest
		wantPath  string
		wantError bool
	}{
		{
			name: "basic model translation",
			input: &openai.ChatCompletionRequest{
				Model: "gcp.gemini-pro",
				Messages: []openai.ChatCompletionMessageParamUnion{
					{
						Type: openai.ChatMessageRoleUser,
						Value: openai.ChatCompletionUserMessageParam{
							Role:    openai.ChatMessageRoleUser,
							Content: openai.StringOrUserRoleContentUnion{Value: "hi"},
						},
					},
				},
			},
			wantPath:  "publishers/google/models/gemini-pro:generateContent",
			wantError: false,
		},
	}

	tr := NewChatCompletionOpenAIToGCPGeminiTranslator().(*openAIToGCPGeminiTranslatorV1ChatCompletion)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			hm, bm, err := tr.RequestBody(nil, tc.input, false)
			if tc.wantError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var gotPath string
			for _, h := range hm.SetHeaders {
				if h.Header.Key == ":path" {
					gotPath = string(h.Header.RawValue)
				}
			}
			if diff := cmp.Diff(tc.wantPath, gotPath); diff != "" {
				t.Errorf("path mismatch (-want +got):\n%s", diff)
			}

			// Only check that the body is valid JSON and has the expected top-level keys
			var gotBody map[string]interface{}
			if err := json.Unmarshal(bm.Mutation.(*extprocv3.BodyMutation_Body).Body, &gotBody); err != nil {
				t.Fatalf("failed to unmarshal body: %v", err)
			}
			if _, ok := gotBody["contents"]; !ok {
				t.Errorf("body missing 'contents' key")
			}
		})
	}
}

func TestOpenAIToGCPGeminiTranslator_ResponseBody(t *testing.T) {
	tr := NewChatCompletionOpenAIToGCPGeminiTranslator().(*openAIToGCPGeminiTranslatorV1ChatCompletion)
	// Use a minimal valid GCP response JSON
	gcpResp := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{"content": map[string]interface{}{"role": "model", "parts": []interface{}{map[string]interface{}{"text": "hello"}}}},
		},
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     float64(5),
			"candidatesTokenCount": float64(7),
			"totalTokenCount":      float64(12),
		},
	}
	gcpRespBytes, _ := json.Marshal(gcpResp)
	body := bytes.NewReader(gcpRespBytes)

	hm, bm, usage, err := tr.ResponseBody(nil, body, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Expect OpenAI-style response
	expectedResp := map[string]interface{}{
		"choices": []interface{}{
			map[string]interface{}{
				"index": float64(0),
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": "hello",
				},
				"finish_reason": "content_filter",
				"logprobs":      map[string]interface{}{},
			},
		},
		"object": "chat.completion",
		"usage": map[string]interface{}{
			"prompt_tokens":     float64(5),
			"completion_tokens": float64(7),
			"total_tokens":      float64(12),
		},
	}

	var gotResp map[string]interface{}
	if err := json.Unmarshal(bm.Mutation.(*extprocv3.BodyMutation_Body).Body, &gotResp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if diff := cmp.Diff(expectedResp, gotResp); diff != "" {
		t.Errorf("response mismatch (-want +got):\n%s", diff)
	}

	wantUsage := LLMTokenUsage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	if diff := cmp.Diff(wantUsage, usage); diff != "" {
		t.Errorf("usage mismatch (-want +got):\n%s", diff)
	}

	var contentType, contentLength string
	for _, h := range hm.SetHeaders {
		if h.Header.Key == "content-type" {
			contentType = string(h.Header.RawValue)
		}
		if h.Header.Key == "content-length" {
			contentLength = string(h.Header.RawValue)
		}
	}
	if contentType != "application/json" {
		t.Errorf("expected content-type application/json, got %s", contentType)
	}
	if contentLength == "" {
		t.Errorf("expected content-length header to be set")
	}
}
