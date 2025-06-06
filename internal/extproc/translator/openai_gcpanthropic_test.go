package translator

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
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
					Value: openai.ChatCompletionMessageParamUnion{
						Type: openai.ChatMessageRoleSystem,
						Value: openai.ChatCompletionSystemMessageParam{
							Role:    openai.ChatMessageRoleSystem,
							Content: openai.StringOrArray{Value: "You are a helpful assistant."},
						},
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
		require.Equal(t, "You are a system array.", got.System)
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
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleAssistant,
					Value: openai.ChatCompletionAssistantMessageParam{
						Role: openai.ChatMessageRoleAssistant,
						Content: openai.StringOrAssistantRoleContentUnion{
							Value: "Here is a tool result.",
						},
						ToolCalls: []openai.ChatCompletionMessageToolCallParam{
							{
								ID: "tool-id-1",
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
			if *c.GetToolUseID() == "tool-id-1" {
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
