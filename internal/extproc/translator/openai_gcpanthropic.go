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
	AnthropicVersion string                         `json:"anthropic_version"`
	Messages         []anthropic.MessageParam       `json:"messages"`
	MaxTokens        int64                          `json:"max_tokens"`
	Stream           bool                           `json:"stream,omitempty"`
	System           []anthropic.TextBlockParam     `json:"system,omitzero"`
	StopSequences    []string                       `json:"stop_sequences,omitzero"`
	Model            anthropic.Model                `json:"model,omitempty"`
	Temperature      *float64                       `json:"temperature,omitempty"`
	Tools            []anthropic.ToolUnionParam     `json:"tools,omitzero"`
	ToolChoice       anthropic.ToolChoiceUnionParam `json:"tool_choice,omitzero"`
	TopP             *float64                       `json:"top_p,omitzero"`
	TopK             *int64                         `json:"top_k,omitzero"`
}

// NewChatCompletionOpenAIToGCPAnthropicTranslator implements [Factory] for OpenAI to GCP Gemini translation.
func NewChatCompletionOpenAIToGCPAnthropicTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPAnthropicTranslatorV1ChatCompletion{}
}

type openAIToGCPAnthropicTranslatorV1ChatCompletion struct{}

func anthropicToOpenAIFinishReason(reason string) openai.ChatCompletionChoicesFinishReason {
	stopReason := anthropic.StopReason(reason)
	switch stopReason {
	// TODO: "pause_turn" Used with server tools like web search when Claude needs to pause a long-running operation.
	// TODO: "refusal" Claude refused to generate a response due to safety concerns.
	// TODO: "
	// The most common stop reason. Indicates Claude finished its response naturally.
	// or Claude encountered one of your custom stop sequences.
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return openai.ChatCompletionChoicesFinishReasonStop
	case anthropic.StopReasonMaxTokens: // Claude stopped because it reached the max_tokens limit specified in your request.
		return openai.ChatCompletionChoicesFinishReasonLength
	case anthropic.StopReasonToolUse:
		return "tool_calls"
	default:
		// TODO: change/fix/test
		// TODO: we are missing pause_turn anthropic.StopReasonPauseTurn & StopReasonRefusal
		return openai.ChatCompletionChoicesFinishReason(reason)
	}
}

// Helper: Extract system/developer prompt from OpenAI messages and return the prompt
func extractSystemOrDeveloperPrompt(msg openai.ChatCompletionMessageParamUnion) (systemPrompt string) {
	switch v := msg.Value.(type) {
	case openai.ChatCompletionSystemMessageParam:
		if s, ok := v.Content.Value.(string); ok {
			systemPrompt = s
		} else if arr, ok := v.Content.Value.([]openai.ChatCompletionContentPartUserUnionParam); ok && len(arr) > 0 && arr[0].TextContent != nil {
			systemPrompt = arr[0].TextContent.Text
		}
	case map[string]interface{}:
		if s, ok := v["content"].(string); ok {
			systemPrompt = s
		}
	}
	return
}

// Helper: Validate and extract stop sequences from OpenAI request
// Helper: Convert []*string to []string for stop sequences
func extractStopSequencesFromPtrSlice(stop []*string) ([]string, error) {
	if stop == nil {
		return nil, nil
	}
	stopSequences := make([]string, 0, len(stop))
	for _, s := range stop {
		if s == nil {
			return nil, fmt.Errorf("invalid stop param: message.stop contains nil value")
		}
		stopSequences = append(stopSequences, *s)
	}
	return stopSequences, nil
}

// Helper: Convert OpenAI message content to Anthropic content (extended for all types)
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
			} else if contentPart.InputAudioContent != nil { //TODO
				return nil, fmt.Errorf("input audio content not supported yet")
			}
		}
		return resultContent, nil
	}
	return nil, fmt.Errorf("unsupported OpenAI content type: %T", content)
}

// Refactored: Convert OpenAI messages to Anthropic messages, handling all roles and system/developer logic
func openAIMessageToGCPAnthropicMessage(openAIReq *openai.ChatCompletionRequest, anthropicReq *anthropicRequest) (err error) {
	anthropicReq.Messages = make([]anthropic.MessageParam, 0, len(openAIReq.Messages))
	for i := range openAIReq.Messages {
		msg := &openAIReq.Messages[i]
		switch msg.Type {
		case openai.ChatMessageRoleUser:
			message := msg.Value.(openai.ChatCompletionUserMessageParam)
			var content []anthropic.ContentBlockParamUnion
			content, err = openAIToAnthropicContent(message.Content.Value)
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
			var content []anthropic.ContentBlockParamUnion
			content, err = openAIToAnthropicContent(message.Content.Value)
			if err != nil {
				return err
			}
			anthropicMsg := anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: content,
			}
			anthropicReq.Messages = append(anthropicReq.Messages, anthropicMsg)
		case openai.ChatMessageRoleDeveloper, openai.ChatMessageRoleSystem:
			// todo: test that the conversion works for both system and developer messages
			// todo: do we assume the openai dev/system is always text? check
			systemPrompt := extractSystemOrDeveloperPrompt(msg.Value.(openai.ChatCompletionMessageParamUnion))
			anthropicReq.System = append(anthropicReq.System, anthropic.TextBlockParam{Text: systemPrompt})
		default:
			return fmt.Errorf("unsupported OpenAI role type: %s", msg.Type)
		}
		// todo: add tool support
	}
	return nil
}

// RequestBody implements [Translator.RequestBody] for GCP.
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, onRetry bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	region := "us-east5"
	project := "bb-llm-gateway-dev"
	model := "claude-3-5-haiku@20241022" // TODO: make var
	gcpReqPath := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:rawPredict", region, project, region, model)

	// Validate max_tokens/max_completion_tokens
	// todo: openAIReq.MaxCompletionTokens == nil -- add it?
	if openAIReq.MaxTokens == nil {
		return nil, nil, fmt.Errorf("max_tokens is required in OpenAI request")
	}

	// todo: check if thinking a chat param? cant find in docs

	if openAIReq.Stream {
		return nil, nil, fmt.Errorf("streaming is not yet supported for GCP Anthropic translation")
	}

	anthropicReq := anthropicRequest{
		AnthropicVersion: anthropicVersion,
		MaxTokens:        *openAIReq.MaxTokens,
		// todo: add stream support
		//Stream:           openAIReq.Stream,
		Model:       anthropic.Model(model),
		Temperature: openAIReq.Temperature,
		TopP:        openAIReq.TopP,
		//TopK:             openAIReq.TopK,
		// todo: we dont support top k?
		// todo: add tool support
		// todo: add tool_choice support
	}

	// Validate and extract stop sequences
	stopSequences, err := extractStopSequencesFromPtrSlice(openAIReq.Stop)
	if err != nil {
		return nil, nil, err // or handle as appropriate
	}
	if len(stopSequences) > 0 {
		anthropicReq.StopSequences = stopSequences
	}

	err = openAIMessageToGCPAnthropicMessage(openAIReq, &anthropicReq)
	if err != nil {
		return nil, nil, err
	}

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, nil, err
	}

	headerMutation = &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{
				Header: &corev3.HeaderValue{
					Key:      ":path",
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
