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
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

func TestOpenAIToGCPAnthropicTranslatorV1ChatCompletion_RequestBody(t *testing.T) {
	tests := []struct {
		name   string
		input  openai.ChatCompletionRequest
		output anthropicRequest
	}{
		{
			name: "basic user message string",
			input: openai.ChatCompletionRequest{
				Stream: false,
				Model:  "claude-3-5-haiku",
				Messages: []openai.ChatCompletionMessageParamUnion{
					{
						Value: openai.ChatCompletionUserMessageParam{
							Role: openai.ChatMessageRoleUser,
							Content: openai.StringOrUserRoleContentUnion{
								Value: "Hello, how are you?",
							},
						},
						Type: openai.ChatMessageRoleUser,
					},
				},
				MaxTokens: ptr.To(int64(10)),
			},
			output: anthropicRequest{
				AnthropicVersion: anthropicVersion,
				Model:            "claude-3-5-haiku",
				Messages: []anthropic.MessageParam{
					{
						Role:    anthropic.MessageParamRoleUser,
						Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("Hello, how are you?")},
					},
				},
				MaxTokens: 10,
				Stream:    false,
			},
		},
		{
			name: "user message array of text",
			input: openai.ChatCompletionRequest{
				Stream: false,
				Model:  "claude-3-5-haiku",
				Messages: []openai.ChatCompletionMessageParamUnion{
					{
						Value: openai.ChatCompletionUserMessageParam{
							Role: openai.ChatMessageRoleUser,
							Content: openai.StringOrUserRoleContentUnion{
								Value: []openai.ChatCompletionContentPartUserUnionParam{
									{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "part1"}},
									{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "part2"}},
								},
							},
						},
						Type: openai.ChatMessageRoleUser,
					},
				},
				MaxTokens: ptr.To(int64(5)),
			},
			output: anthropicRequest{
				AnthropicVersion: anthropicVersion,
				Model:            "claude-3-5-haiku",
				Messages: []anthropic.MessageParam{
					{
						Role: anthropic.MessageParamRoleUser,
						Content: []anthropic.ContentBlockParamUnion{
							anthropic.NewTextBlock("part1"),
							anthropic.NewTextBlock("part2"),
						},
					},
				},
				MaxTokens: 5,
				Stream:    false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
			hm, bm, err := translator.RequestBody(nil, &tt.input, false)
			require.NoError(t, err)
			require.NotNil(t, hm)
			require.NotNil(t, bm)
			require.Len(t, hm.SetHeaders, 2)
			require.Equal(t, ":path", hm.SetHeaders[0].Header.Key)
			//require.Contains(t, hm.SetHeaders[0].Header.Value, "anthropic")
			require.Equal(t, "content-length", hm.SetHeaders[1].Header.Key)
			newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body

			require.Equal(t, []byte(fmt.Sprintf("%d", len(newBody))), hm.SetHeaders[1].Header.RawValue)
			var got anthropicRequest
			fmt.Println(string(newBody))
			err = json.Unmarshal(newBody, &got)
			require.NoError(t, err)

			// Compare as JSON to avoid pointer address issues
			expectedJSON, err := json.Marshal(tt.output)
			require.NoError(t, err)
			gotJSON, err := json.Marshal(got)
			require.NoError(t, err)
			require.JSONEq(t, string(expectedJSON), string(gotJSON))
		})
	}
}

//func TestUserMessageStream(t *testing.T) {
//	input := openai.ChatCompletionRequest{
//		Stream: true,
//		Model:  "claude-3-5-haiku",
//		Messages: []openai.ChatCompletionMessageParamUnion{
//			{
//				Value: openai.ChatCompletionUserMessageParam{
//					Role: openai.ChatMessageRoleUser,
//					Content: openai.StringOrUserRoleContentUnion{
//						Value: "Hey how are you?",
//					},
//				},
//				Type: openai.ChatMessageRoleUser,
//			},
//		},
//		MaxTokens: ptr.To(int64(10)),
//	}
//	translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
//	hm, bm, err := translator.RequestBody(nil, &input, false)
//	require.NoError(t, err)
//	require.NotNil(t, hm)
//	require.NotNil(t, bm)
//	newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
//
//	var got anthropicRequest
//	err = json.Unmarshal(newBody, &got)
//	require.NoError(t, err)
//	require.True(t, got.Stream, "stream field should be true in request body")
//	require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
//	require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hey how are you?")
//}

func TestSystemAndDeveloperPromptExtraction(t *testing.T) {
	t.Run("system message string", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleSystem,
					Value: openai.ChatCompletionSystemMessageParam{
						Role:    openai.ChatMessageRoleSystem,
						Content: openai.StringOrArray{Value: "You are a helpful assistant."},
					},
				},
				{
					Type: openai.ChatMessageRoleUser,
					Value: openai.ChatCompletionUserMessageParam{
						Role:    openai.ChatMessageRoleUser,
						Content: openai.StringOrUserRoleContentUnion{Value: "Hello!"},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a helpful assistant.", got.System[0].Text)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
		require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hello!")
	})

	t.Run("system message array", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleSystem,
					Value: openai.ChatCompletionSystemMessageParam{
						Role: openai.ChatMessageRoleSystem,
						Content: openai.StringOrArray(openai.StringOrUserRoleContentUnion{
							Value: []openai.ChatCompletionContentPartUserUnionParam{
								{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "You are a system array."}},
							},
						}),
					},
				},
				{
					Type: openai.ChatMessageRoleUser,
					Value: openai.ChatCompletionUserMessageParam{
						Role:    openai.ChatMessageRoleUser,
						Content: openai.StringOrUserRoleContentUnion{Value: "Hi!"},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a system array.", got.System[0].Text)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
		require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hi!")
	})

	t.Run("developer message string", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleDeveloper,
					Value: openai.ChatCompletionDeveloperMessageParam{
						Role:    openai.ChatMessageRoleDeveloper,
						Content: openai.StringOrArray(openai.StringOrUserRoleContentUnion{Value: "You are a dev system."}),
					},
				},
				{
					Type: openai.ChatMessageRoleUser,
					Value: openai.ChatCompletionUserMessageParam{
						Role:    openai.ChatMessageRoleUser,
						Content: openai.StringOrUserRoleContentUnion{Value: "Hi dev!"},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a dev system.", got.System[0].Text)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
		require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hi dev!")
	})

	t.Run("developer message array", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleDeveloper,
					Value: openai.ChatCompletionDeveloperMessageParam{
						Role: openai.ChatMessageRoleDeveloper,
						Content: openai.StringOrArray(openai.StringOrUserRoleContentUnion{
							Value: []openai.ChatCompletionContentPartUserUnionParam{
								{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "You are a dev system array."}},
							},
						}),
					},
				},
				{
					Type: openai.ChatMessageRoleUser,
					Value: openai.ChatCompletionUserMessageParam{
						Role:    openai.ChatMessageRoleUser,
						Content: openai.StringOrUserRoleContentUnion{Value: "Hi dev array!"},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a dev system array.", got.System[0].Text)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
		require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hi dev array!")
	})

	t.Run("assistant message with content and tool_calls", func(t *testing.T) {
		toolID := "tool-id-1"
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleAssistant,
					Value: openai.ChatCompletionAssistantMessageParam{
						Content: openai.StringOrAssistantRoleContentUnion{
							Value: openai.ChatCompletionAssistantMessageParamContent{
								Type: openai.ChatCompletionAssistantMessageParamContentTypeText,
								Text: ptr.To("Here is a tool result."),
							},
						},
						ToolCalls: []openai.ChatCompletionMessageToolCallParam{
							{
								ID: toolID,
								Function: openai.ChatCompletionMessageToolCallFunctionParam{
									Name:      "get_weather",
									Arguments: `{"location":"NYC"}`,
								},
							},
						},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleAssistant, got.Messages[0].Role)
		// Should contain both the text and the tool_use block
		foundText := false
		foundToolUse := false
		for _, c := range got.Messages[0].Content {
			if c.GetText() != nil && *c.GetText() == "Here is a tool result." {
				foundText = true
			}
			if c.OfToolUse != nil && c.OfToolUse.ID == toolID {
				foundToolUse = true
			}
		}
		require.True(t, foundText, "should contain text content")
		require.True(t, foundToolUse, "should contain tool_use content")
	})

	t.Run("tool message", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleTool,
					Value: openai.ChatCompletionToolMessageParam{
						Role:       openai.ChatMessageRoleTool,
						ToolCallID: "tool-call-123",
						Content: openai.StringOrArray{
							Value: []openai.ChatCompletionContentPartUserUnionParam{
								{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "tool result"}},
							},
						},
					},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got anthropicRequest
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
		require.NotNil(t, got.Messages[0].Content[0].GetToolUseID())
		require.Equal(t, "tool-call-123", got.Messages[0].Content[0].OfToolResult.ToolUseID)
		require.Equal(t, "tool result", got.Messages[0].Content[0].OfToolResult.Content[0].OfText.Text)
	})

	t.Run("unsupported role", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type:  "unknownrole",
					Value: map[string]interface{}{"role": "unknownrole", "content": "bad"},
				},
			},
			MaxTokens: ptr.To(int64(5)),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, _, err := translator.RequestBody(nil, &input, false)
		require.Error(t, err)
	})
}

func TestOpenAIToGCPAnthropicTranslatorV1ChatCompletion_ResponseBody(t *testing.T) {
	// Test case for a body that cannot be unmarshaled by the translator.
	t.Run("invalid json body", func(t *testing.T) {
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, _, _, err := translator.ResponseBody(map[string]string{statusHeaderName: "200"}, bytes.NewBufferString("invalid json"), true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to unmarshal body")
	})

	// Test cases for valid, successful responses
	tests := []struct {
		name                   string
		inputResponse          *anthropic.Message
		respHeaders            map[string]string
		expectedOpenAIResponse openai.ChatCompletionResponse
	}{
		{
			name: "basic text response",
			inputResponse: &anthropic.Message{
				Role:       "assistant",
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
				Role: "assistant",
				Content: []anthropic.ContentBlockUnion{
					{Type: "text", Text: "Ok, I will call the tool."},
					// Use a map for the Input field for better readability
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
							Role:    "assistant",
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

			translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
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
		inputBody       interface{} // Can be a struct to marshal or a string
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
			// Define the input using the SDK's error response struct
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

			o := &openAIToGCPAnthropicTranslatorV1ChatCompletion{}
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
