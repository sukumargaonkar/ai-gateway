package translator

import (
	"encoding/json"
	"strconv"
	"testing"

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
							Role: openai.ChatMessageRoleUser, //TODO: aws test doesn't have this field, should we?
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
				Messages: []anthropicMessage{
					{
						Role: "user",
						Content: []AnthropicContent{
							{Type: "text", Text: "Hello, how are you?"},
						},
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
				Messages: []anthropicMessage{
					{
						Role: "user",
						Content: []AnthropicContent{
							{Type: "text", Text: "part1"},
							{Type: "text", Text: "part2"},
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
			require.Equal(t, tt.output, got)
		})
	}
}

func ptrToInt64(i int64) *int64 { return &i }
