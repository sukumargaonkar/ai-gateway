// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
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
	"github.com/tidwall/sjson"
)

// TODO: support for mcp server field, server tier(?)

// currently a requirement for GCP Vertex / Anthropic API https://docs.anthropic.com/en/api/claude-on-vertex-ai
const (
	anthropicVersion = "vertex-2023-10-16"
	gcpBackendError  = "GCPBackendError"
)

type openAIToAnthropicTranslatorV1ChatCompletion struct {
	// Configuration flags for Vertex AI
	isVertex        bool
	vertexRegion    string
	vertexProjectID string
}

// WithVertexAI returns a TranslatorOption that configures the translator to target
// the Google Cloud Vertex AI endpoint instead of the default Anthropic API.
// It requires the GCP region and project ID.
func WithVertexAI(region, projectID string) TranslatorOption {
	return func(t *openAIToAnthropicTranslatorV1ChatCompletion) {
		t.isVertex = true
		t.vertexRegion = region
		t.vertexProjectID = projectID
	}
}

// TranslatorOption defines a function that configures the translator.
type TranslatorOption func(*openAIToAnthropicTranslatorV1ChatCompletion)

// AnthropicContent Anthropic request/response structs
type AnthropicContent struct {
	Type   string                            `json:"type"`
	Text   string                            `json:"text"`
	Source *anthropic.Base64ImageSourceParam `json:"source,omitempty"`
}

// NewChatCompletionOpenAIToAnthropicTranslator creates a new translator.
// It can be configured with options, such as WithVertexAI, to change its target endpoint.
func NewChatCompletionOpenAIToAnthropicTranslator(opts ...TranslatorOption) OpenAIChatCompletionTranslator {
	translator := &openAIToAnthropicTranslatorV1ChatCompletion{}
	for _, opt := range opts {
		opt(translator)
	}
	return translator
}

func anthropicToOpenAIFinishReason(stopReason anthropic.StopReason) (openai.ChatCompletionChoicesFinishReason, error) {
	switch stopReason {
	// The most common stop reason. Indicates Claude finished its response naturally.
	// or Claude encountered one of your custom stop sequences.
	// TODO: A better way to return pause_turng
	// TODO: "pause_turn" Used with server tools like web search when Claude needs to pause a long-running operation.
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence, anthropic.StopReasonPauseTurn:
		return openai.ChatCompletionChoicesFinishReasonStop, nil
	case anthropic.StopReasonMaxTokens: // Claude stopped because it reached the max_tokens limit specified in your request.
		return openai.ChatCompletionChoicesFinishReasonLength, nil
	case anthropic.StopReasonToolUse:
		return openai.ChatCompletionChoicesFinishReasonToolCalls, nil
	case anthropic.StopReasonRefusal:
		return openai.ChatCompletionChoicesFinishReasonContentFilter, nil
	default:
		return "", fmt.Errorf("received invalid stop reason %v", stopReason)
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

func anthropicRoleToOpenAIRole(role anthropic.MessageParamRole) (string, error) {
	switch role {
	case "assistant":
		return openai.ChatMessageRoleAssistant, nil
	case "user":
		return openai.ChatMessageRoleUser, nil
	default:
		return "", fmt.Errorf("invalid anthropic role %v", role)
	}
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

	return &anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: contentBlocks,
	}, nil
}

// openAIMessagesToAnthropicParams converts OpenAI messages to Anthropic message params type, handling all roles and system/developer logic
func openAIMessagesToAnthropicParams(openAIReq *openai.ChatCompletionRequest, anthropicReq *anthropic.MessageNewParams) (err error) {
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
func (o *openAIToAnthropicTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, _ bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// Validate max_tokens/max_completion_tokens is set
	if openAIReq.MaxTokens == nil && openAIReq.MaxCompletionTokens == nil {
		return nil, nil, fmt.Errorf("max_tokens is required in OpenAI request")
	}

	anthropicReq := anthropic.MessageNewParams{
		MaxTokens: *openAIReq.MaxTokens,
		Model:     anthropic.Model(openAIReq.Model),
	}
	// TODO: add tool support
	// TODO: add tool_choice support

	// 3. Handle optional parameters with type conversion
	if validateErr := validateTemperatureForAnthropic(openAIReq.Temperature); validateErr != nil {
		return nil, nil, validateErr
	}
	if openAIReq.Temperature != nil {
		if err := validateTemperatureForAnthropic(openAIReq.Temperature); err != nil {
			return nil, nil, err
		}
		anthropicReq.Temperature = anthropic.Float(*openAIReq.Temperature)
	}
	if openAIReq.TopP != nil {
		anthropicReq.TopP = anthropic.Float(*openAIReq.TopP)
	}
	stopSequences, err := extractStopSequencesFromPtrSlice(openAIReq.Stop)
	if err != nil {
		return nil, nil, err
	}
	if len(stopSequences) > 0 {
		anthropicReq.StopSequences = stopSequences
	}

	err = openAIMessagesToAnthropicParams(openAIReq, &anthropicReq)
	if err != nil {
		return nil, nil, err
	}

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, nil, err
	}

	path := "/v1/messages" // Default to standard Anthropic path

	// TODO: add stream support
	if o.isVertex {
		// --- VERTEX AI PATH ---
		specifier := "rawPredict"
		if openAIReq.Stream {
			// Note: Full streaming support is not yet implemented in the response handler.
			// specifier = "streamRawPredict"
			return nil, nil, fmt.Errorf("streaming is not yet supported for GCP Anthropic translation")
		}
		path = fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s", o.vertexProjectID, o.vertexRegion, openAIReq.Model, specifier)

		// a. Delete the "model" key from the JSON body
		body, _ = sjson.DeleteBytes(body, "model")

		// b. Set the "anthropic_version" key in the JSON body
		body, _ = sjson.SetBytes(body, "anthropic_version", anthropicVersion)
	}

	headerMutation = &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{
				Header: &corev3.HeaderValue{
					Key:      ":path",
					RawValue: []byte(path),
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
func (o *openAIToAnthropicTranslatorV1ChatCompletion) ResponseHeaders(headers map[string]string) (
	headerMutation *extprocv3.HeaderMutation, err error,
) {
	// TODO: Implement if needed.
	return nil, nil
}

// ResponseError implements [Translator.ResponseError].
func (o *openAIToAnthropicTranslatorV1ChatCompletion) ResponseError(respHeaders map[string]string, body io.Reader) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	statusCode := respHeaders[statusHeaderName]
	var openaiError openai.Error
	var decodeErr error

	// Check for a JSON content type to decide how to parse the error.
	if v, ok := respHeaders[contentTypeHeaderName]; ok && strings.Contains(v, jsonContentType) {
		var gcpError anthropic.ErrorResponse
		if decodeErr = json.NewDecoder(body).Decode(&gcpError); decodeErr != nil {
			// If we expect JSON but fail to decode, it's an internal translator error.
			return nil, nil, fmt.Errorf("failed to unmarshal JSON error body: %w", decodeErr)
		}
		openaiError = openai.Error{
			Type: "error",
			Error: openai.ErrorType{
				Type:    gcpError.Error.Type,
				Message: gcpError.Error.Message,
				Code:    &statusCode,
			},
		}
	} else {
		// If not JSON, read the raw body as the error message.
		var buf []byte
		buf, decodeErr = io.ReadAll(body)
		if decodeErr != nil {
			return nil, nil, fmt.Errorf("failed to read raw error body: %w", decodeErr)
		}
		openaiError = openai.Error{
			Type: "error",
			Error: openai.ErrorType{
				Type:    gcpBackendError,
				Message: string(buf),
				Code:    &statusCode,
			},
		}
	}

	// Marshal the translated OpenAI error.
	mut := &extprocv3.BodyMutation_Body{}
	mut.Body, err = json.Marshal(openaiError)
	if err != nil {
		// This is an internal failure to create the response.
		return nil, nil, fmt.Errorf("failed to marshal OpenAI error body: %w", err)
	}

	headerMutation = &extprocv3.HeaderMutation{}
	setContentLength(headerMutation, mut.Body)
	bodyMutation = &extprocv3.BodyMutation{Mutation: mut}

	// On successful translation of an error, return err = nil.
	return headerMutation, bodyMutation, nil
}

// anthropicToolUseToOpenAICalls converts Anthropic tool_use content blocks to OpenAI tool calls.
func anthropicToolUseToOpenAICalls(block anthropic.ContentBlockUnion) ([]openai.ChatCompletionMessageToolCallParam, error) {
	var toolCalls []openai.ChatCompletionMessageToolCallParam
	if block.Type != "tool_use" {
		return toolCalls, nil
	}
	argsBytes, err := json.Marshal(block.Input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal tool_use input: %w", err)
	}
	toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallParam{
		ID:   block.ID,
		Type: openai.ChatCompletionMessageToolCallTypeFunction,
		Function: openai.ChatCompletionMessageToolCallFunctionParam{
			Name:      block.Name,
			Arguments: string(argsBytes),
		},
	})

	return toolCalls, nil
}

// ResponseBody implements [Translator.ResponseBody] for GCP Anthropic.
func (o *openAIToAnthropicTranslatorV1ChatCompletion) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	if statusStr, ok := respHeaders[statusHeaderName]; ok {
		var status int
		// Use the outer 'err' to catch parsing errors
		if status, err = strconv.Atoi(statusStr); err == nil {
			if !isGoodStatusCode(status) {
				// Let ResponseError handle the translation. It returns its own internal error status.
				headerMutation, bodyMutation, err = o.ResponseError(respHeaders, body)
				return headerMutation, bodyMutation, LLMTokenUsage{}, err
			}
		} else {
			// Fail if the status code isn't a valid integer.
			return nil, nil, LLMTokenUsage{}, fmt.Errorf("failed to parse status code '%s': %w", statusStr, err)
		}
	}

	mut := &extprocv3.BodyMutation_Body{}
	var anthropicResp anthropic.Message
	if err = json.NewDecoder(body).Decode(&anthropicResp); err != nil {
		return nil, nil, tokenUsage, fmt.Errorf("failed to unmarshal body: %w", err)
	}

	openAIResp := openai.ChatCompletionResponse{
		Object:  "chat.completion",
		Choices: make([]openai.ChatCompletionResponseChoice, 0),
	}
	tokenUsage = LLMTokenUsage{
		InputTokens:  uint32(anthropicResp.Usage.InputTokens),
		OutputTokens: uint32(anthropicResp.Usage.OutputTokens),
		TotalTokens:  uint32(anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens),
	}
	openAIResp.Usage = openai.ChatCompletionResponseUsage{
		CompletionTokens: int(anthropicResp.Usage.OutputTokens),
		PromptTokens:     int(anthropicResp.Usage.InputTokens),
		TotalTokens:      int(anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens),
	}

	finishReason, err := anthropicToOpenAIFinishReason(anthropicResp.StopReason)
	if err != nil {
		return nil, nil, LLMTokenUsage{}, err
	}

	role, err := anthropicRoleToOpenAIRole(anthropic.MessageParamRole(anthropicResp.Role))
	if err != nil {
		return nil, nil, LLMTokenUsage{}, err
	}

	choice := openai.ChatCompletionResponseChoice{
		Index:        0,
		Message:      openai.ChatCompletionResponseChoiceMessage{Role: role},
		FinishReason: finishReason,
	}

	for _, output := range anthropicResp.Content {
		if output.Type == "tool_use" && output.ID != "" {
			toolCalls, toolErr := anthropicToolUseToOpenAICalls(output)
			if toolErr != nil {
				return nil, nil, tokenUsage, fmt.Errorf("failed to convert anthropic tool use to openai tool call: %w", toolErr)
			}
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, toolCalls...)
		} else if output.Type == "text" && output.Text != "" {
			if choice.Message.Content == nil {
				choice.Message.Content = &output.Text
			}
		}
	}
	openAIResp.Choices = append(openAIResp.Choices, choice)

	mut.Body, err = json.Marshal(openAIResp)
	if err != nil {
		return nil, nil, tokenUsage, fmt.Errorf("failed to marshal body: %w", err)
	}

	headerMutation = &extprocv3.HeaderMutation{}
	setContentLength(headerMutation, mut.Body)
	return headerMutation, &extprocv3.BodyMutation{Mutation: mut}, tokenUsage, nil
}
