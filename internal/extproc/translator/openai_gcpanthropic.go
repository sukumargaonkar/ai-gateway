// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	openAIconstant "github.com/openai/openai-go/shared/constant"
	"github.com/tidwall/sjson"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

// currently a requirement for GCP Vertex / Anthropic API https://docs.anthropic.com/en/api/claude-on-vertex-ai
const (
	anthropicVersionKey   = "anthropic_version"
	anthropicVersionValue = "vertex-2023-10-16"
	gcpBackendError       = "GCPBackendError"
	defaultMaxTokens      = int64(100)
	tempNotSupportedError = "temperature %.2f is not supported by Anthropic (must be between 0.0 and 1.0)"
)

var errStreamingNotSupported = errors.New("streaming is not yet supported for GCP Anthropic translation")

// openAIToAnthropicTranslatorV1ChatCompletion where we can store information for streaming requests.
type openAIToAnthropicTranslatorV1ChatCompletion struct{}

// Option defines a function that configures the translator.
type Option func(*openAIToAnthropicTranslatorV1ChatCompletion)

// AnthropicContent Anthropic request/response structs.
type AnthropicContent struct {
	Type   string                            `json:"type"`
	Text   string                            `json:"text"`
	Source *anthropic.Base64ImageSourceParam `json:"source,omitempty"`
}

// NewChatCompletionOpenAIToAnthropicTranslator creates a new translator.
func NewChatCompletionOpenAIToAnthropicTranslator() OpenAIChatCompletionTranslator {
	return &openAIToAnthropicTranslatorV1ChatCompletion{}
}

func anthropicToOpenAIFinishReason(stopReason anthropic.StopReason) (openai.ChatCompletionChoicesFinishReason, error) {
	switch stopReason {
	// The most common stop reason. Indicates Claude finished its response naturally.
	// or Claude encountered one of your custom stop sequences.
	// TODO: A better way to return pause_turning
	// TODO: "pause_turn" Used with server tools like web search when Claude needs to pause a long-running operation.
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence, anthropic.StopReasonPauseTurn:
		return openai.ChatCompletionChoicesFinishReasonStop, nil
	case anthropic.StopReasonMaxTokens: // Claude stopped because it reached the max_tokens limit specified in your request.
		// TODO: do we want to return an error? see: https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/implement-tool-use#handling-the-max-tokens-stop-reason
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
	if temp != nil && *temp > 1.0 {
		return fmt.Errorf(tempNotSupportedError, *temp)
	}
	return nil
}

// Helper: Convert []*string to []string for stop sequences.
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

// translateOpenAItoAnthropicTools translates OpenAI tool and tool_choice parameters
// into the Anthropic format and returns translated tool & tool choice.
func translateOpenAItoAnthropicTools(openAITools []openai.Tool, openAIToolChoice any, parallelToolCalls *bool) (tools []anthropic.ToolUnionParam, toolChoice anthropic.ToolChoiceUnionParam, err error) {
	if len(openAITools) > 0 {
		anthropicTools := make([]anthropic.ToolUnionParam, 0, len(openAITools))
		for _, openAITool := range openAITools {
			toolParam := anthropic.ToolParam{
				Name:        openAITool.Function.Name,
				Description: anthropic.String(openAITool.Function.Description),
			}
			if openAITool.Type != openai.ToolTypeFunction {
				// Anthropic only supports 'function' tools, so we skip others.
				continue
			}

			// The parameters for the function are expected to be a JSON Schema object.
			// We can pass them through as-is.
			var inputSchema map[string]interface{}
			if openAITool.Function.Parameters != nil {
				// Directly assert the 'any' type to the expected map structure.
				schema, ok := openAITool.Function.Parameters.(map[string]interface{})
				if !ok {
					err = fmt.Errorf("tool parameters for '%s' are not a valid JSON object", openAITool.Function.Name)
					return
				}
				inputSchema = schema
			}

			toolParam.InputSchema = anthropic.ToolInputSchemaParam{
				Properties: inputSchema,
				// TODO: support extra fields.
				ExtraFields: nil,
			}

			anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{OfTool: &toolParam})

			if len(anthropicTools) > 0 {
				tools = anthropicTools
			}
		}

		// 2. Handle the tool_choice parameter.
		// disable parallel tool use default value is false
		// see: https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/implement-tool-use#parallel-tool-use
		disableParallelToolUse := anthropic.Bool(false)
		if parallelToolCalls != nil {
			// OpenAI variable checks to allow parallel tool calls.
			// Anthropic variable checks to disable, so need to use the inverse.
			disableParallelToolUse = anthropic.Bool(!*parallelToolCalls)
		}
		if openAIToolChoice != nil {
			switch choice := openAIToolChoice.(type) {
			case string:
				switch choice {
				case string(openAIconstant.ValueOf[openAIconstant.Auto]()):
					toolChoice = anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}
					toolChoice.OfAuto.DisableParallelToolUse = disableParallelToolUse
				case "required", "any":
					toolChoice = anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
					toolChoice.OfAny.DisableParallelToolUse = disableParallelToolUse
				case "none":
					toolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
				case string(openAIconstant.ValueOf[openAIconstant.Function]()):
					// this is how anthropic forces tool use
					// TODO: should we check if strict true in openAI request, and if so, use this?
					toolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: choice}}
					toolChoice.OfTool.DisableParallelToolUse = disableParallelToolUse
				default:
					err = fmt.Errorf("invalid tool choice type '%s'", choice)
				}
			case openai.ToolChoice:
				if choice.Type == openai.ToolTypeFunction && choice.Function.Name != "" {
					toolChoice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Type: constant.Tool(choice.Type), Name: choice.Function.Name}}
					toolChoice.OfTool.DisableParallelToolUse = disableParallelToolUse
				}
			}
		}
	}
	return
}

// Helper: Convert OpenAI message content to Anthropic content (extended for all types).
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
				switch {
				case isDataURI(imageURL):
					contentType, data, err := parseDataURI(imageURL)
					if err != nil {
						return nil, fmt.Errorf("failed to parse image URL: %w", err)
					}
					base64Data := base64.StdEncoding.EncodeToString(data)
					appPDF := string(constant.ValueOf[constant.ApplicationPDF]())
					switch contentType {
					case appPDF:
						pdfSource := anthropic.Base64PDFSourceParam{
							Data: base64Data,
						}
						resultContent = append(resultContent, anthropic.NewDocumentBlock(pdfSource))
					default:
						if isSupportedImageMediaType(contentType) {
							resultContent = append(resultContent, anthropic.NewImageBlockBase64(contentType, base64Data))
						} else {
							return nil, fmt.Errorf("invalid media_type for image '%s'", contentType)
						}
					}
				case strings.HasSuffix(strings.ToLower(imageURL), ".pdf"):
					resultContent = append(resultContent, anthropic.NewDocumentBlock(anthropic.URLPDFSourceParam{
						URL: imageURL,
					}))
				default:
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

func extractSystemPromptFromDeveloperMsg(msg openai.ChatCompletionDeveloperMessageParam) string {
	switch v := msg.Content.Value.(type) {
	case string:
		return v
	case []openai.ChatCompletionContentPartUserUnionParam:
		// Concatenate all text parts for completeness.
		var sb strings.Builder
		for _, part := range v {
			if part.TextContent != nil {
				sb.WriteString(part.TextContent.Text)
			}
		}
		return sb.String()
	case nil:
		return ""
	case openai.StringOrArray:
		switch val := v.Value.(type) {
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
	default:
		return ""
	}
	return ""
}

// systemMsgToDeveloperMsg is a helper to convert an OpenAI system message
// into a developer message to consolidate processing logic.
func systemMsgToDeveloperMsg(msg openai.ChatCompletionSystemMessageParam) openai.ChatCompletionDeveloperMessageParam {
	return openai.ChatCompletionDeveloperMessageParam{
		Name:    msg.Name,
		Role:    openai.ChatMessageRoleDeveloper,
		Content: msg.Content,
	}
}

func anthropicRoleToOpenAIRole(role anthropic.MessageParamRole) (string, error) {
	switch role {
	case anthropic.MessageParamRoleAssistant:
		return openai.ChatMessageRoleAssistant, nil
	case anthropic.MessageParamRoleUser:
		return openai.ChatMessageRoleUser, nil
	default:
		return "", fmt.Errorf("invalid anthropic role %v", role)
	}
}

// openAIMessageToAnthropicMessageRoleAssistant converts an OpenAI assistant message to Anthropic content blocks.
// The tool_use content is appended to the Anthropic message content list if tool_calls are present.
func openAIMessageToAnthropicMessageRoleAssistant(openAiMessage *openai.ChatCompletionAssistantMessageParam) (anthropicMsg *anthropic.MessageParam, err error) {
	contentBlocks := make([]anthropic.ContentBlockParamUnion, 0)
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
		default:
			err = fmt.Errorf("content type not supported: %v", content.Type)
			return
		}
	}

	// Handle tool_calls (if any).
	for i := range openAiMessage.ToolCalls {
		toolCall := &openAiMessage.ToolCalls[i]
		var input map[string]interface{}
		if err = json.Unmarshal([]byte(toolCall.Function.Arguments), &input); err != nil {
			err = fmt.Errorf("failed to unmarshal tool call arguments: %w", err)
			return
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

// openAIToAnthropicMessages converts OpenAI messages to Anthropic message params type, handling all roles and system/developer logic.
func openAIToAnthropicMessages(openAIMsgs []openai.ChatCompletionMessageParamUnion) (anthropicMessages []anthropic.MessageParam, systemBlocks []anthropic.TextBlockParam, err error) {
	for i := range openAIMsgs {
		msg := openAIMsgs[i]
		switch msg.Type {
		case openai.ChatMessageRoleSystem:
			if param, ok := msg.Value.(openai.ChatCompletionSystemMessageParam); ok {
				devParam := systemMsgToDeveloperMsg(param)
				systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: extractSystemPromptFromDeveloperMsg(devParam)})
			}
		case openai.ChatMessageRoleDeveloper:
			if param, ok := msg.Value.(openai.ChatCompletionDeveloperMessageParam); ok {
				systemBlocks = append(systemBlocks, anthropic.TextBlockParam{Text: extractSystemPromptFromDeveloperMsg(param)})
			}
		case openai.ChatMessageRoleUser:
			message := msg.Value.(openai.ChatCompletionUserMessageParam)
			var content []anthropic.ContentBlockParamUnion
			content, err = openAIToAnthropicContent(message.Content.Value)
			if err != nil {
				return
			}
			anthropicMsg := anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: content,
			}
			anthropicMessages = append(anthropicMessages, anthropicMsg)
		case openai.ChatMessageRoleAssistant:
			assistantMessage := msg.Value.(openai.ChatCompletionAssistantMessageParam)

			var messages *anthropic.MessageParam
			messages, err = openAIMessageToAnthropicMessageRoleAssistant(&assistantMessage)
			if err != nil {
				return
			}
			anthropicMessages = append(anthropicMessages, *messages)
		case openai.ChatMessageRoleTool:
			toolMsg := msg.Value.(openai.ChatCompletionToolMessageParam)
			var content []anthropic.ContentBlockParamUnion
			content, err = openAIToAnthropicContent(toolMsg.Content)
			if err != nil {
				return
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
				// IsError:  anthropic.Bool(false), TODO: Should we support isError from openAI.
			}
			anthropicMsg := anthropic.MessageParam{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					{OfToolResult: &toolResultBlock},
				},
			}
			anthropicMessages = append(anthropicMessages, anthropicMsg)
		default:
			err = fmt.Errorf("unsupported OpenAI role type: %s", msg.Type)
			return
		}
	}
	return
}

// buildAnthropicParams is a helper function that translates an OpenAI request
// into the parameter struct required by the Anthropic SDK.
func buildAnthropicParams(openAIReq *openai.ChatCompletionRequest) (params *anthropic.MessageNewParams, err error) {
	// 1. Handle simple parameters and defaults.
	maxTokens := defaultMaxTokens
	if openAIReq.MaxCompletionTokens != nil {
		maxTokens = *openAIReq.MaxCompletionTokens
	} else if openAIReq.MaxTokens != nil {
		maxTokens = *openAIReq.MaxTokens
	}

	// Translate openAI contents to anthropic params.
	// 2. Translate messages and system prompts.
	messages, systemBlocks, err := openAIToAnthropicMessages(openAIReq.Messages)
	if err != nil {
		return
	}

	// Translate tools and tool choice.
	tools, toolChoice, err := translateOpenAItoAnthropicTools(openAIReq.Tools, openAIReq.ToolChoice, openAIReq.ParallelToolCalls)
	if err != nil {
		return
	}

	// 4. Construct the final struct in one place.
	params = &anthropic.MessageNewParams{
		Messages:   messages,
		MaxTokens:  maxTokens,
		System:     systemBlocks,
		Tools:      tools,
		ToolChoice: toolChoice,
	}

	if openAIReq.Temperature != nil {
		if err = validateTemperatureForAnthropic(openAIReq.Temperature); err != nil {
			return &anthropic.MessageNewParams{}, err
		}
		params.Temperature = anthropic.Float(*openAIReq.Temperature)
	}
	if openAIReq.TopP != nil {
		params.TopP = anthropic.Float(*openAIReq.TopP)
	}

	// Handle stop sequences.
	stopSequences, err := extractStopSequencesFromPtrSlice(openAIReq.Stop)
	if err != nil {
		return &anthropic.MessageNewParams{}, err
	}
	if len(stopSequences) > 0 {
		params.StopSequences = stopSequences
	}

	return params, nil
}

// RequestBody implements [Translator.RequestBody] for GCP.
func (o *openAIToAnthropicTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, _ bool) (
	*extprocv3.HeaderMutation, *extprocv3.BodyMutation, error,
) {
	params, err := buildAnthropicParams(openAIReq)
	if err != nil {
		return nil, nil, err
	}

	body, err := json.Marshal(params)
	if err != nil {
		return nil, nil, err
	}

	// TODO: add stream support.

	// GCP VERTEX PATH.
	specifier := "rawPredict"
	if openAIReq.Stream {
		// TODO: specifier = "streamRawPredict" - use this when implementing streaming.
		return nil, nil, errStreamingNotSupported
	}

	pathSuffix := buildGCPModelPathSuffix(GCPModelPublisherAnthropic, openAIReq.Model, specifier)
	// b. Set the "anthropic_version" key in the JSON body
	// Using same logic as anthropic go SDK: https://github.com/anthropics/anthropic-sdk-go/blob/main/vertex/vertex.go#L78
	body, _ = sjson.SetBytes(body, anthropicVersionKey, anthropicVersionValue)

	headerMutation, bodyMutation := buildGCPRequestMutations(pathSuffix, body)
	return headerMutation, bodyMutation, nil
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

	return headerMutation, bodyMutation, nil
}

// anthropicToolUseToOpenAICalls converts Anthropic tool_use content blocks to OpenAI tool calls.
func anthropicToolUseToOpenAICalls(block anthropic.ContentBlockUnion) ([]openai.ChatCompletionMessageToolCallParam, error) {
	var toolCalls []openai.ChatCompletionMessageToolCallParam
	if block.Type != string(constant.ValueOf[constant.ToolUse]()) {
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

// ResponseHeaders implements [Translator.ResponseHeaders].
func (o *openAIToAnthropicTranslatorV1ChatCompletion) ResponseHeaders(headers map[string]string) (
	headerMutation *extprocv3.HeaderMutation, err error,
) {
	// TODO: Implement if needed.
	_ = headers
	return nil, nil
}

// ResponseBody implements [Translator.ResponseBody] for GCP Anthropic.
func (o *openAIToAnthropicTranslatorV1ChatCompletion) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	_ = endOfStream
	if statusStr, ok := respHeaders[statusHeaderName]; ok {
		var status int
		// Use the outer 'err' to catch parsing errors.
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
		Object:  string(openAIconstant.ValueOf[openAIconstant.ChatCompletion]()),
		Choices: make([]openai.ChatCompletionResponseChoice, 0),
	}
	tokenUsage = LLMTokenUsage{
		InputTokens:  uint32(anthropicResp.Usage.InputTokens),                                    //nolint:gosec
		OutputTokens: uint32(anthropicResp.Usage.OutputTokens),                                   //nolint:gosec
		TotalTokens:  uint32(anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens), //nolint:gosec
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
		if output.Type == string(constant.ValueOf[constant.ToolUse]()) && output.ID != "" {
			toolCalls, toolErr := anthropicToolUseToOpenAICalls(output)
			if toolErr != nil {
				return nil, nil, tokenUsage, fmt.Errorf("failed to convert anthropic tool use to openai tool call: %w", toolErr)
			}
			choice.Message.ToolCalls = append(choice.Message.ToolCalls, toolCalls...)
		} else if output.Type == string(constant.ValueOf[constant.Text]()) && output.Text != "" {
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
