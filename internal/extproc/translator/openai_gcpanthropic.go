// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

// TODO: support for "system"? https://docs.anthropic.com/en/api/messages#tool-use
// TODO: support for mcp server field, server tier
//TODO: support stream
// TODO: topk is in anthropic but not openai

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
	AnthropicVersion string                     `json:"anthropic_version"`
	Messages         []anthropic.MessageParam   `json:"messages"`
	MaxTokens        int64                      `json:"max_tokens"`
	Stream           bool                       `json:"stream,omitempty"`
	System           []anthropic.TextBlockParam `json:"system,omitempty"`
	StopSequences    []string                   `json:"stop_sequences,omitempty"`
	Model            anthropic.Model            `json:"model,omitempty"`
	Temperature      *float64                   `json:"temperature,omitempty"`
	Tools            []anthropic.ToolUnionParam `json:"tools,omitempty"`
	// ToolChoice       anthropic.ToolChoiceUnionParam `json:"tool_choice,omitempty"`
	TopP *float64 `json:"top_p,omitempty"`
}

// NewChatCompletionOpenAIToGCPAnthropicTranslator implements [Factory] for OpenAI to GCP Gemini translation.
func NewChatCompletionOpenAIToGCPAnthropicTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPAnthropicTranslatorV1ChatCompletion{}
}

type openAIToGCPAnthropicTranslatorV1ChatCompletion struct{}

func anthropicToOpenAIFinishReason(reason string) openai.ChatCompletionChoicesFinishReason {
	stopReason := anthropic.StopReason(reason)
	switch stopReason {
	// The most common stop reason. Indicates Claude finished its response naturally.
	// or Claude encountered one of your custom stop sequences.
	// TODO: A better way to return pause_turng
	// TODO: "pause_turn" Used with server tools like web search when Claude needs to pause a long-running operation.
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence, anthropic.StopReasonPauseTurn:
		return openai.ChatCompletionChoicesFinishReasonStop
	case anthropic.StopReasonMaxTokens: // Claude stopped because it reached the max_tokens limit specified in your request.
		return openai.ChatCompletionChoicesFinishReasonLength
	case anthropic.StopReasonToolUse:
		return openai.ChatCompletionChoicesFinishReasonToolCalls
	case anthropic.StopReasonRefusal:
		return openai.ChatCompletionChoicesFinishReasonContentFilter
	default:
		return openai.ChatCompletionChoicesFinishReason(reason)
	}
}

// validateTemperatureForAnthropic checks if the temperature is within Anthropic's supported range (0.0 to 1.0).
// Returns an error if the value is greater than 1.0.
func validateTemperatureForAnthropic(temp *float64) error {
	if temp == nil {
		return nil
	}
	if *temp > 1.0 {
		return fmt.Errorf("temperature %.2f is not supported by Anthropic (must be between 0.0 and 1.0)", *temp)
	}
	return nil
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

func isDataURI(uri string) bool {
	return strings.HasPrefix(uri, "data:")
}

func isSupportedImageMediaType(mediaType string) bool {
	switch anthropic.Base64ImageSourceMediaType(mediaType) {
	case anthropic.Base64ImageSourceMediaTypeImageJPEG,
		anthropic.Base64ImageSourceMediaTypeImagePNG,
		anthropic.Base64ImageSourceMediaTypeImageGIF,
		anthropic.Base64ImageSourceMediaTypeImageWebP:
		return true
	default:
		return false
	}
}

func toAnthropicToolUse(
	toolCalls []openai.ChatCompletionMessageToolCallParam,
) ([]anthropic.ToolUseBlockParam, error) {
	var anthropicToolCalls []anthropic.ToolUseBlockParam
	for _, toolCall := range toolCalls {
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &input); err != nil {
			return nil, err
		}
		toolUse := anthropic.ToolUseBlockParam{
			ID:    toolCall.ID,
			Type:  "tool_use",
			Name:  toolCall.Function.Name,
			Input: input,
		}
		anthropicToolCalls = append(anthropicToolCalls, toolUse)
	}
	return anthropicToolCalls, nil
}

// Helper: Convert OpenAI message content to Anthropic content (extended for all types)
func openAIToAnthropicContent(content interface{}) ([]anthropic.ContentBlockParamUnion, error) {
	switch v := content.(type) {
	case nil:
		return nil, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []anthropic.ContentBlockParamUnion{
			anthropic.NewTextBlock(v),
		}, nil
	case []openai.ChatCompletionContentPartUserUnionParam:
		resultContent := make([]anthropic.ContentBlockParamUnion, 0, len(v))
		for _, contentPart := range v {
			switch {
			case contentPart.TextContent != nil:
				resultContent = append(resultContent, anthropic.NewTextBlock(contentPart.TextContent.Text))
			case contentPart.ImageContent != nil:
				imageURL := contentPart.ImageContent.ImageURL.URL
				if isDataURI(imageURL) {
					contentType, data, err := parseDataURI(imageURL)
					if err != nil {
						return nil, fmt.Errorf("failed to parse image URL: %w", err)
					}
					appPDF := constant.ValueOf[constant.ApplicationPDF]()
					base64Data := base64.StdEncoding.EncodeToString(data)
					if contentType == string(appPDF) {
						pdfSource := anthropic.Base64PDFSourceParam{
							Data: base64Data,
						}
						resultContent = append(resultContent, anthropic.NewDocumentBlock(pdfSource))
					} else if isSupportedImageMediaType(contentType) {
						resultContent = append(resultContent, anthropic.NewImageBlockBase64(contentType, base64Data))
					} else {
						return nil, fmt.Errorf("invalid media_type for image '%s'", contentType)
					}
				} else if strings.HasSuffix(strings.ToLower(imageURL), ".pdf") {
					resultContent = append(resultContent, anthropic.NewDocumentBlock(anthropic.URLPDFSourceParam{
						URL: imageURL,
					}))
				} else {
					resultContent = append(resultContent, anthropic.NewImageBlock(anthropic.URLImageSourceParam{
						URL: imageURL,
					}))
				}
			case contentPart.InputAudioContent != nil:
				return nil, fmt.Errorf("input audio content not supported yet")
			}
		}
		return resultContent, nil
	case openai.StringOrArray:
		switch val := v.Value.(type) {
		case string:
			if val == "" {
				return nil, nil
			}
			return []anthropic.ContentBlockParamUnion{
				anthropic.NewTextBlock(val),
			}, nil
		case []openai.ChatCompletionContentPartUserUnionParam:
			return openAIToAnthropicContent(val)
		default:
			return nil, fmt.Errorf("unsupported StringOrArray value type: %T", val)
		}
	}
	return nil, fmt.Errorf("unsupported OpenAI content type: %T", content)
}

func extractSystemOrDeveloperPromptFromSystem(msg openai.ChatCompletionSystemMessageParam) string {
	switch v := msg.Content.Value.(type) {
	case string:
		return v
	case []openai.ChatCompletionContentPartUserUnionParam:
		// Concatenate all text parts for completeness
		var sb strings.Builder
		for _, part := range v {
			if part.TextContent != nil {
				sb.WriteString(part.TextContent.Text)
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		// If msg.Content is a StringOrArray, unwrap it
		if soarr, ok := msg.Content.Value.(openai.StringOrArray); ok {
			switch val := soarr.Value.(type) {
			case string:
				return val
			case []openai.ChatCompletionContentPartUserUnionParam:
				var sb strings.Builder
				for _, part := range val {
					if part.TextContent != nil {
						sb.WriteString(part.TextContent.Text)
					}
				}
				return sb.String()
			}
		}
	}
	return ""
}

func extractSystemOrDeveloperPromptFromDeveloper(msg openai.ChatCompletionDeveloperMessageParam) string {
	switch v := msg.Content.Value.(type) {
	case string:
		return v
	case []openai.ChatCompletionContentPartUserUnionParam:
		// Concatenate all text parts for completeness
		var sb strings.Builder
		for _, part := range v {
			if part.TextContent != nil {
				sb.WriteString(part.TextContent.Text)
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		// If msg.Content is a StringOrArray, unwrap it
		if soarr, ok := msg.Content.Value.(openai.StringOrArray); ok {
			switch val := soarr.Value.(type) {
			case string:
				return val
			case []openai.ChatCompletionContentPartUserUnionParam:
				var sb strings.Builder
				for _, part := range val {
					if part.TextContent != nil {
						sb.WriteString(part.TextContent.Text)
					}
				}
				return sb.String()
			}
		}
	}
	return ""
}

// openAIMessageToAnthropicMessageRoleAssistant converts an OpenAI assistant message to Anthropic content blocks.
// The tool_use content is appended to the Anthropic message content list if tool_calls are present.
func openAIMessageToAnthropicMessageRoleAssistant(openAiMessage *openai.ChatCompletionAssistantMessageParam) (*anthropic.MessageParam, error) {
	contentBlocks := make([]anthropic.ContentBlockParamUnion, 0)
	// Handle text/refusal content
	if v, ok := openAiMessage.Content.Value.(string); ok && len(v) > 0 {
		contentBlocks = append(contentBlocks, anthropic.NewTextBlock(v))
	} else if content, ok := openAiMessage.Content.Value.(openai.ChatCompletionAssistantMessageParamContent); ok {
		switch content.Type {
		case openai.ChatCompletionAssistantMessageParamContentTypeRefusal:
			if content.Refusal != nil {
				contentBlocks = append(contentBlocks, anthropic.NewTextBlock(*content.Refusal))
			}
		case openai.ChatCompletionAssistantMessageParamContentTypeText:
			if content.Text != nil {
				contentBlocks = append(contentBlocks, anthropic.NewTextBlock(*content.Text))
			}
			// TODO: Add more cases here if you support images, etc.
		}
	}

	// Handle tool_calls (if any)
	for i := range openAiMessage.ToolCalls {
		toolCall := &openAiMessage.ToolCalls[i]
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &input); err != nil {
			return nil, err
		}
		toolUse := anthropic.ToolUseBlockParam{
			ID:    toolCall.ID,
			Type:  "tool_use",
			Name:  toolCall.Function.Name,
			Input: input,
		}
		contentBlocks = append(contentBlocks, anthropic.ContentBlockParamUnion{OfToolUse: &toolUse})
	}
	g
	return &anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: contentBlocks,
	}, nil
}

// openAIMessageToGCPAnthropicMessage converts OpenAI messages to Anthropic messages, handling all roles and system/developer logic
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
			assistantMessage := msg.Value.(openai.ChatCompletionAssistantMessageParam)

			var messages *anthropic.MessageParam
			messages, err = openAIMessageToAnthropicMessageRoleAssistant(&assistantMessage)
			if err != nil {
				return err
			}

			// TODO: check works with multi tool
			anthropicReq.Messages = append(anthropicReq.Messages, *messages)
		case openai.ChatMessageRoleDeveloper, openai.ChatMessageRoleSystem:
			var systemPrompt string
			switch v := msg.Value.(type) {
			case openai.ChatCompletionSystemMessageParam:
				systemPrompt = extractSystemOrDeveloperPromptFromSystem(v)
			case openai.ChatCompletionDeveloperMessageParam:
				systemPrompt = extractSystemOrDeveloperPromptFromDeveloper(v)
			default:
				return fmt.Errorf("unexpected type for system/developer message: %T", msg.Value)
			}
			anthropicReq.System = append(anthropicReq.System, anthropic.TextBlockParam{Text: systemPrompt})
		case openai.ChatMessageRoleTool:
			toolMsg := msg.Value.(openai.ChatCompletionToolMessageParam)
			var content []anthropic.ContentBlockParamUnion
			content, err = openAIToAnthropicContent(toolMsg.Content)
			if err != nil {
				return err
			}
			var toolContent []anthropic.ToolResultBlockParamContentUnion
			var trb anthropic.ToolResultBlockParamContentUnion
			for _, c := range content {
				if c.OfText != nil {
					trb.OfText = c.OfText
				} else if c.OfImage != nil {
					trb.OfImage = c.OfImage
				}
				toolContent = append(toolContent, trb)
			}

			toolResultBlock := anthropic.ToolResultBlockParam{
				ToolUseID: toolMsg.ToolCallID,
				Type:      "tool_result",
				Content:   toolContent,
			}
			anthropicMsg := anthropic.MessageParam{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					{OfToolResult: &toolResultBlock},
				},
			}
			anthropicReq.Messages = append(anthropicReq.Messages, anthropicMsg)
		default:
			return fmt.Errorf("unsupported OpenAI role type: %s", msg.Type)
		}
	}
	return nil
}

// RequestBody implements [Translator.RequestBody] for GCP.
func (o *openAIToGCPAnthropicTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, _ bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	region := "us-east5"
	project := ""
	model := openAIReq.Model // TODO: make var
	gcpReqPath := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:rawPredict", region, project, region, model)

	// Validate max_tokens/max_completion_tokens is set
	if openAIReq.MaxTokens == nil && openAIReq.MaxCompletionTokens == nil {
		return nil, nil, fmt.Errorf("max_tokens is required in OpenAI request")
	}

	if validateErr := validateTemperatureForAnthropic(openAIReq.Temperature); validateErr != nil {
		return nil, nil, validateErr
	}

	// TODO: add stream support
	if openAIReq.Stream {
		return nil, nil, fmt.Errorf("streaming is not yet supported for GCP Anthropic translation")
	}

	anthropicReq := anthropicRequest{
		AnthropicVersion: anthropicVersion,
		MaxTokens:        *openAIReq.MaxTokens,
		//Stream:           openAIReq.Stream,
		Model:       anthropic.Model(model),
		Temperature: openAIReq.Temperature,
		TopP:        openAIReq.TopP,
		// TODO: add tool support
		// TODO: add tool_choice support
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
	headerMutation = &extprocv3.HeaderMutation{}
	setContentLength(headerMutation, respBody)
	return headerMutation, bodyMutation, tokenUsage, nil
}
