package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"k8s.io/utils/ptr"
)

func TestOpenAIToGCPAnthropicTranslatorV1ChatCompletion_RequestBody(t *testing.T) {
	// Define a common input request to use for both standard and vertex tests.
	claudeOpusModel := "claude-3-opus-20240229"
	openAIReq := &openai.ChatCompletionRequest{
		Model: claudeOpusModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			{
				Type:  openai.ChatMessageRoleSystem,
				Value: openai.ChatCompletionSystemMessageParam{Content: openai.StringOrArray{Value: "You are a helpful assistant."}},
			},
			{
				Type:  openai.ChatMessageRoleUser,
				Value: openai.ChatCompletionUserMessageParam{Content: openai.StringOrUserRoleContentUnion{Value: "Hello!"}},
			},
		},
		MaxTokens:   ptr.To(int64(1024)),
		Temperature: ptr.To(0.7),
	}

	// Subtest for the standard Anthropic API endpoint
	t.Run("Standard Anthropic API", func(t *testing.T) {
		translator := NewChatCompletionOpenAIToAnthropicTranslator() // No options
		hm, bm, err := translator.RequestBody(nil, openAIReq, false)
		require.NoError(t, err)
		require.NotNil(t, hm)
		require.NotNil(t, bm)

		// Check the path header
		pathHeader := hm.SetHeaders[0]
		require.Equal(t, ":path", pathHeader.Header.Key)
		require.Equal(t, fmt.Sprintf("/models/%s:rawPredict", openAIReq.Model), string(pathHeader.Header.RawValue))

		// Check the body content
		body := bm.GetBody()
		require.NotNil(t, body)
		// Anthropic version should be present
		require.True(t, gjson.GetBytes(body, "anthropic_version").Exists())
	})

	// TOOD: update
	// Sub-test for the Vertex AI endpoint
	t.Run("Vertex Values Configured Correctly", func(t *testing.T) {

		// Create the translator with the VertexAI option
		translator := NewChatCompletionOpenAIToAnthropicTranslator()
		hm, bm, err := translator.RequestBody(nil, openAIReq, false)
		require.NoError(t, err)
		require.NotNil(t, hm)
		require.NotNil(t, bm)

		// Check the path header
		pathHeader := hm.SetHeaders[0]
		require.Equal(t, ":path", pathHeader.Header.Key)
		expectedPath := fmt.Sprintf("/models/%s:rawPredict", openAIReq.Model)
		require.Equal(t, expectedPath, string(pathHeader.Header.RawValue))

		// Check the body content
		body := bm.GetBody()
		require.NotNil(t, body)
		// Model should NOT be present in the body
		require.False(t, gjson.GetBytes(body, "model").Exists())
		// Anthropic version SHOULD be present
		require.Equal(t, anthropicVersion, gjson.GetBytes(body, "anthropic_version").String())
	})

	// Test for missing required parameter
	t.Run("Missing MaxTokens Uses Default", func(t *testing.T) {
		missingTokensReq := &openai.ChatCompletionRequest{
			Model:     "claude-3",
			Messages:  []openai.ChatCompletionMessageParamUnion{},
			MaxTokens: nil, // Missing
		}
		translator := NewChatCompletionOpenAIToAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, missingTokensReq, false)
		require.NoError(t, err)
		body := bm.GetBody()
		require.Equal(t, defaultMaxTokens, gjson.GetBytes(body, "max_tokens").Int())
	})
}

func TestOpenAIToGCPAnthropicTranslatorV1ChatCompletion_ResponseBody(t *testing.T) {
	t.Run("invalid json body", func(t *testing.T) {
		translator := NewChatCompletionOpenAIToAnthropicTranslator()
		_, _, _, err := translator.ResponseBody(map[string]string{statusHeaderName: "200"}, bytes.NewBufferString("invalid json"), true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to unmarshal body")
	})

	tests := []struct {
		name                   string
		inputResponse          *anthropic.Message
		respHeaders            map[string]string
		expectedOpenAIResponse openai.ChatCompletionResponse
	}{
		{
			name: "basic text response",
			inputResponse: &anthropic.Message{
				Role:       constant.Assistant(anthropic.MessageParamRoleAssistant),
				Content:    []anthropic.ContentBlockUnion{{Type: "text", Text: "Hello there!"}},
				StopReason: anthropic.StopReasonEndTurn,
				Usage:      anthropic.Usage{InputTokens: 10, OutputTokens: 20},
			},
			respHeaders: map[string]string{statusHeaderName: "200"},
			expectedOpenAIResponse: openai.ChatCompletionResponse{
				Object: "chat.completion",
				Usage:  openai.ChatCompletionResponseUsage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
				Choices: []openai.ChatCompletionResponseChoice{
					{
						Index:        0,
						Message:      openai.ChatCompletionResponseChoiceMessage{Role: "assistant", Content: ptr.To("Hello there!")},
						FinishReason: openai.ChatCompletionChoicesFinishReasonStop,
					},
				},
			},
		},
		{
			name: "response with tool use",
			inputResponse: &anthropic.Message{
				Role: constant.Assistant(anthropic.MessageParamRoleAssistant),
				Content: []anthropic.ContentBlockUnion{
					{Type: "text", Text: "Ok, I will call the tool."},
					{Type: "tool_use", ID: "toolu_01", Name: "get_weather", Input: json.RawMessage(`{"location": "Tokyo", "unit": "celsius"}`)},
				},
				StopReason: anthropic.StopReasonToolUse,
				Usage:      anthropic.Usage{InputTokens: 25, OutputTokens: 15},
			},
			respHeaders: map[string]string{statusHeaderName: "200"},
			expectedOpenAIResponse: openai.ChatCompletionResponse{
				Object: "chat.completion",
				Usage:  openai.ChatCompletionResponseUsage{PromptTokens: 25, CompletionTokens: 15, TotalTokens: 40},
				Choices: []openai.ChatCompletionResponseChoice{
					{
						Index:        0,
						FinishReason: openai.ChatCompletionChoicesFinishReasonToolCalls,
						Message: openai.ChatCompletionResponseChoiceMessage{
							Role:    string(anthropic.MessageParamRoleAssistant),
							Content: ptr.To("Ok, I will call the tool."),
							ToolCalls: []openai.ChatCompletionMessageToolCallParam{
								{
									ID:   "toolu_01",
									Type: openai.ChatCompletionMessageToolCallTypeFunction,
									Function: openai.ChatCompletionMessageToolCallFunctionParam{
										Name:      "get_weather",
										Arguments: `{"location":"Tokyo","unit":"celsius"}`,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.inputResponse)
			require.NoError(t, err, "Test setup failed: could not marshal input struct")

			translator := NewChatCompletionOpenAIToAnthropicTranslator()
			hm, bm, usedToken, err := translator.ResponseBody(tt.respHeaders, bytes.NewBuffer(body), true)

			require.NoError(t, err, "Translator returned an unexpected internal error")
			require.NotNil(t, hm)
			require.NotNil(t, bm)

			newBody := bm.GetBody()
			require.NotNil(t, newBody)
			require.Len(t, hm.SetHeaders, 1)
			require.Equal(t, "content-length", hm.SetHeaders[0].Header.Key)
			require.Equal(t, strconv.Itoa(len(newBody)), string(hm.SetHeaders[0].Header.RawValue))

			var gotResp openai.ChatCompletionResponse
			err = json.Unmarshal(newBody, &gotResp)
			require.NoError(t, err)

			expectedTokenUsage := LLMTokenUsage{
				InputTokens:  uint32(tt.expectedOpenAIResponse.Usage.PromptTokens),
				OutputTokens: uint32(tt.expectedOpenAIResponse.Usage.CompletionTokens),
				TotalTokens:  uint32(tt.expectedOpenAIResponse.Usage.TotalTokens),
			}
			require.Equal(t, expectedTokenUsage, usedToken)

			if diff := cmp.Diff(tt.expectedOpenAIResponse, gotResp); diff != "" {
				t.Errorf("ResponseBody mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOpenAIToGCPAnthropicTranslator_ResponseError(t *testing.T) {
	tests := []struct {
		name            string
		responseHeaders map[string]string
		inputBody       interface{}
		expectedOutput  openai.Error
	}{
		{
			name: "non-json error response",
			responseHeaders: map[string]string{
				statusHeaderName:      "503",
				contentTypeHeaderName: "text/plain; charset=utf-8",
			},
			inputBody: "Service Unavailable",
			expectedOutput: openai.Error{
				Type: "error",
				Error: openai.ErrorType{
					Type:    gcpBackendError,
					Code:    ptr.To("503"),
					Message: "Service Unavailable",
				},
			},
		},
		{
			name: "json error response",
			responseHeaders: map[string]string{
				statusHeaderName:      "400",
				contentTypeHeaderName: "application/json",
			},
			inputBody: &anthropic.ErrorResponse{
				Type: "error",
				Error: shared.ErrorObjectUnion{
					Type:    "invalid_request_error",
					Message: "Your max_tokens is too high.",
				},
			},
			expectedOutput: openai.Error{
				Type: "error",
				Error: openai.ErrorType{
					Type:    "invalid_request_error",
					Code:    ptr.To("400"),
					Message: "Your max_tokens is too high.",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reader io.Reader
			if bodyStr, ok := tt.inputBody.(string); ok {
				reader = bytes.NewBufferString(bodyStr)
			} else {
				bodyBytes, err := json.Marshal(tt.inputBody)
				require.NoError(t, err)
				reader = bytes.NewBuffer(bodyBytes)
			}

			o := &openAIToAnthropicTranslatorV1ChatCompletion{}
			hm, bm, err := o.ResponseError(tt.responseHeaders, reader)

			require.NoError(t, err)
			require.NotNil(t, bm)
			require.NotNil(t, hm)

			newBody := bm.GetBody()
			require.NotNil(t, newBody)
			require.Len(t, hm.SetHeaders, 1)
			require.Equal(t, "content-length", hm.SetHeaders[0].Header.Key)
			require.Equal(t, strconv.Itoa(len(newBody)), string(hm.SetHeaders[0].Header.RawValue))

			var gotError openai.Error
			err = json.Unmarshal(newBody, &gotError)
			require.NoError(t, err)

			if diff := cmp.Diff(tt.expectedOutput, gotError); diff != "" {
				t.Errorf("ResponseError() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
