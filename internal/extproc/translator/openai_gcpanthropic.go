// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// TODO: support for "system"? https://docs.anthropic.com/en/api/messages#tool-use
// TODO: support for mcp server field, server tier
//TODO: support stream

// currently a requirement for GCP Vertex / Anthropic API https://docs.anthropic.com/en/api/claude-on-vertex-ai
const anthropicVersion = "vertex-2023-10-16"

// Anthropic request/response structs
type AnthropicContent struct {
	Type   string                            `json:"type"`
	Text   string                            `json:"text"`
	Source *anthropic.Base64ImageSourceParam `json:"source,omitempty"`
}

type anthropicResponse struct {
	Content    []AnthropicContent `json:"content"`
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	Role       string             `json:"role"`
	StopReason string             `json:"stop_reason"`
	StopSeq    *string            `json:"stop_sequence"`
	Type       string             `json:"type"`
	Usage      anthropic.Usage    `json:"usage"`
}

type anthropicRequest struct {
	AnthropicVersion string                   `json:"anthropic_version"`
	Messages         []anthropic.MessageParam `json:"messages"`
	MaxTokens        int                      `json:"max_tokens,omitempty"`
	Stream           bool                     `json:"stream,omitempty"`
}


// NewChatCompletionOpenAIToGCPAnthropicTranslator implements [Factory] for OpenAI to GCP Gemini translation.
func NewChatCompletionOpenAIToGCPAnthropicTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPAnthropicTranslatorV1ChatCompletion{}
}

type openAIToGCPAnthropicTranslatorV1ChatCompletion struct{}

// TODO: 3. Implement retry logic for pause_turn(?) https://docs.anthropic.com/en/api/handling-stop-reasons#3-implement-retry-logic-for-pause-turn
// TODO: remove hard code
func anthropicToOpenAIFinishReason(reason string) openai.ChatCompletionChoicesFinishReason {
	switch reason {
	// TODO: "pause_turn" Used with server tools like web search when Claude needs to pause a long-running operation.
	// TODO: "refusal" Claude refused to generate a response due to safety concerns.
	// TODO: "
	// The most common stop reason. Indicates Claude finished its response naturally.
	// or Claude encountered one of your custom stop sequences.
	// TODO: remove hard coding
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens": // Claude stopped because it reached the max_tokens limit specified in your request.
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		// TODO: change/fix/test
		return openai.ChatCompletionChoicesFinishReason(reason)
	}
}

// Helper: Convert OpenAI message content to Anthropic content
func openAIToAnthropicContent(content interface{}) ([]anthropic.ContentBlockParamUnion, error) {
	if v, ok := content.(string); ok {
		return []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(v)}, nil
	} else if contents, ok := content.([]openai.ChatCompletionContentPartUserUnionParam); ok {
		resultContent := make([]anthropic.ContentBlockParamUnion, 0, len(contents))
		for i := range contents {
			contentPart := &contents[i]
			if contentPart.TextContent != nil {
				resultContent = append(resultContent, anthropic.NewTextBlock(contents[i].TextContent.Text))
			} else if contentPart.ImageContent != nil {
				imageContentPart := contentPart.ImageContent
				contentType, b, err := parseDataURI(imageContentPart.ImageURL.URL)
				if err != nil {
					return nil, fmt.Errorf("failed to parse image URL: %w", err)
				}

				resultContent = append(resultContent, anthropic.NewImageBlockBase64(contentType, string(b)))
			}
		}
		return resultContent, nil
	}
	return nil, fmt.Errorf("unsupported OpenAI content type: %T", content)
}

func openAIMessageToGCPAnthropicMessage(openAIReq *openai.ChatCompletionRequest, anthropicReq *anthropicRequest) error {
	anthropicReq.Messages = make([]anthropic.MessageParam, 0, len(openAIReq.Messages))
	for i := range openAIReq.Messages {
		msg := &openAIReq.Messages[i]
		switch msg.Type {
		case openai.ChatMessageRoleUser:
			message := msg.Value.(openai.ChatCompletionUserMessageParam)
			content, err := openAIToAnthropicContent(message.Content.Value)
			if err != nil {
				return err
			}
			anthropicMsg := anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: content,
			}
			anthropicReq.Messages = append(anthropicReq.Messages, anthropicMsg)
		case openai.ChatMessageRoleAssistant:
			message := msg.Value.(openai.ChatCompletionAssistantMessageParam)
			content, err := openAIToAnthropicContent(message.Content.Value)
			if err != nil {
				return err
			}
			anthropicMsg := anthropic.MessageParam{
				Role:    openai.ChatMessageRoleAssistant,
				Content: content,
			}
			anthropicReq.Messages = append(anthropicReq.Messages, anthropicMsg)
			//TODO:  Note that if you want to include a system prompt, you can use the top-level system parameter — there is no "system" role for input messages in the Messages API.
		//case openai.ChatMessageRoleSystem:
		//	systemMessage := msg.Value.(openai.ChatCompletionSystemMessageParam)
		//	anthropicMsg := anthropicMessage{
		//		Role:    openai.ChatMessageRoleSystem,
		//		Content: openAIToAnthropicContent(systemMessage.Content.Value),
		//	}
		//	anthropicReq.Messages = append(anthropicReq.Messages, anthropicMsg)
		default:
			return fmt.Errorf("unsupported OpenAI role type: %s", msg.Type)
		}
	}
	return nil
}

// RequestBody implements [Translator.RequestBody] for GCP.
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, onRetry bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// For now we just hardcoded an example request
	region := "us-east5"
	project := "bb-llm-gateway-dev"
	model := "claude-3-5-haiku@20241022" // TODO: make var
	gcpReqPath := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:rawPredict", region, project, region, model)

	//TODO: update forming the anthropic req with inference config,
	maxTokens := 256
	if openAIReq.MaxTokens != nil {
		maxTokens = int(*openAIReq.MaxTokens)
	}
	anthropicReq := anthropicRequest{
		AnthropicVersion: "vertex-2023-10-16",
		MaxTokens:        maxTokens,
		Stream:           false, // TODO: add support for streaming
	}

	// Transform OpenAI messages to Anthropic messages
	err = openAIMessageToGCPAnthropicMessage(openAIReq, &anthropicReq)

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, nil, err
	}

	headerMutation = &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{
				Header: &corev3.HeaderValue{
					Key:   ":path",
					RawValue: []byte(gcpReqPath),
				},
			},
			{
				Header: &corev3.HeaderValue{
					Key:      "content-length",
					RawValue: []byte(strconv.Itoa(len(body))),
				},
			},
		},
	}
	bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{Body: body},
	}
	return headerMutation, bodyMutation, nil

	//	gcpReqBody := []byte(`{
	//  "anthropic_version": "vertex-2023-10-16",
	//  "messages": [
	//    {
	//      "role": "user",
	//      "content": [
	//        {
	//          "type": "text",
	//          "text": "What is in this image?"
	//        }
	//      ]
	//    }
	//  ],
	//  "max_tokens": 256,
	//  "stream": false
	//}`)

}

// ResponseHeaders implements [Translator.ResponseHeaders].
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) ResponseHeaders(headers map[string]string) (
	headerMutation *extprocv3.HeaderMutation, err error,
) {
	// TODO: Implement if needed.
	return nil, nil
}

// ResponseError implements [Translator.ResponseError].
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) ResponseError(respHeaders map[string]string, body interface{}) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// TODO: Implement error translation.
	return nil, nil, nil
}

// ResponseBody implements [Translator.ResponseBody] for GCP Anthropic.
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	var anthropicResp anthropicResponse
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(body)
	if err != nil {
		return nil, nil, tokenUsage, err
	}
	if err := json.Unmarshal(buf.Bytes(), &anthropicResp); err != nil {
		return nil, nil, tokenUsage, err
	}

	// Concatenate all text parts (usually just one)
	var content string
	for _, part := range anthropicResp.Content {
		if part.Type == "text" {
			content += part.Text
		} //TODO: else?
	}

	openaiResp := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionResponseChoice{
			{
				Message: openai.ChatCompletionResponseChoiceMessage{
					Role:    anthropicResp.Role,
					Content: &content,
				},
				FinishReason: anthropicToOpenAIFinishReason(anthropicResp.StopReason),
			},
		},
		// TODO: what other support do we want to add for usage?
		// this is confusing https://docs.anthropic.com/en/api/service-tiers (three tiers, two values)
		Usage: openai.ChatCompletionResponseUsage{
			PromptTokens:     int(anthropicResp.Usage.InputTokens),
			CompletionTokens: int(anthropicResp.Usage.OutputTokens),
			TotalTokens:      int(anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens),
		},
	}

	respBody, err := json.Marshal(openaiResp)
	if err != nil {
		return nil, nil, tokenUsage, err
	}
	bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{Body: respBody},
	}
	return nil, bodyMutation, tokenUsage, nil
}
