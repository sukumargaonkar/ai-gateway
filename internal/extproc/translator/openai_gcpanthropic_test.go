package translator

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/require"
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
				MaxTokens: ptrToInt64(10),
			},
			output: anthropicRequest{
				AnthropicVersion: anthropicVersion,
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
				MaxTokens: ptrToInt64(5),
			},
			output: anthropicRequest{
				AnthropicVersion: anthropicVersion,
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
			require.Contains(t, hm.SetHeaders[0].Header.Value, "anthropic")
			require.Equal(t, "content-length", hm.SetHeaders[1].Header.Key)
			newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
			require.Equal(t, strconv.Itoa(len(newBody)), hm.SetHeaders[1].Header.Value)

			var got anthropicRequest
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

func TestUserMessageStream(t *testing.T) {
	input := openai.ChatCompletionRequest{
		Stream: true,
		Model:  "claude-3-5-haiku",
		Messages: []openai.ChatCompletionMessageParamUnion{
			{
				Value: openai.ChatCompletionUserMessageParam{
					Role: openai.ChatMessageRoleUser,
					Content: openai.StringOrUserRoleContentUnion{
						Value: "Hey how are you?",
					},
				},
				Type: openai.ChatMessageRoleUser,
			},
		},
		MaxTokens: ptrToInt64(10),
	}
	translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
	hm, bm, err := translator.RequestBody(nil, &input, false)
	require.NoError(t, err)
	require.NotNil(t, hm)
	require.NotNil(t, bm)
	newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body

	var got anthropicRequest
	err = json.Unmarshal(newBody, &got)
	require.NoError(t, err)
	require.True(t, got.Stream, "stream field should be true in request body")
	require.Equal(t, anthropic.MessageParamRoleUser, got.Messages[0].Role)
	require.Contains(t, *got.Messages[0].Content[0].GetText(), "Hey how are you?")
}

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
						Content: openai.StringOrUserRoleContentUnion{Value: "You are a helpful assistant."},
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
			MaxTokens: ptrToInt64(5),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		hm, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got map[string]interface{}
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a helpful assistant.", got["system"])
		msgs, ok := got["messages"].([]interface{})
		require.True(t, ok)
		require.Len(t, msgs, 1)
		msg := msgs[0].(map[string]interface{})
		require.Equal(t, "user", msg["role"])
	})

	t.Run("system message array", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type: openai.ChatMessageRoleSystem,
					Value: openai.ChatCompletionSystemMessageParam{
						Role:    openai.ChatMessageRoleSystem,
						Content: openai.StringOrUserRoleContentUnion{Value: []openai.ChatCompletionContentPartUserUnionParam{{TextContent: &openai.ChatCompletionContentPartTextParam{Text: "You are a system array."}}}},
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
			MaxTokens: ptrToInt64(5),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		hm, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got map[string]interface{}
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a system array.", got["system"])
		msgs, ok := got["messages"].([]interface{})
		require.True(t, ok)
		require.Len(t, msgs, 1)
		msg := msgs[0].(map[string]interface{})
		require.Equal(t, "user", msg["role"])
	})

	t.Run("developer message string", func(t *testing.T) {
		input := openai.ChatCompletionRequest{
			Stream: false,
			Model:  "claude-3-5-haiku",
			Messages: []openai.ChatCompletionMessageParamUnion{
				{
					Type:  "developer",
					Value: map[string]interface{}{"role": "developer", "content": "You are a dev system."},
				},
				{
					Type: openai.ChatMessageRoleUser,
					Value: openai.ChatCompletionUserMessageParam{
						Role:    openai.ChatMessageRoleUser,
						Content: openai.StringOrUserRoleContentUnion{Value: "Hi dev!"},
					},
				},
			},
			MaxTokens: ptrToInt64(5),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		hm, bm, err := translator.RequestBody(nil, &input, false)
		require.NoError(t, err)
		newBody := bm.Mutation.(*extprocv3.BodyMutation_Body).Body
		var got map[string]interface{}
		err = json.Unmarshal(newBody, &got)
		require.NoError(t, err)
		require.Equal(t, "You are a dev system.", got["system"])
		msgs, ok := got["messages"].([]interface{})
		require.True(t, ok)
		require.Len(t, msgs, 1)
		msg := msgs[0].(map[string]interface{})
		require.Equal(t, "user", msg["role"])
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
			MaxTokens: ptrToInt64(5),
		}
		translator := NewChatCompletionOpenAIToGCPAnthropicTranslator()
		_, _, err := translator.RequestBody(nil, &input, false)
		require.Error(t, err)
	})
}

func ptrToInt64(i int64) *int64 { return &i }
