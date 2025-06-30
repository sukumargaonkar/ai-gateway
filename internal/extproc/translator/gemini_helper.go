// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path"

	"github.com/google/uuid"
	"google.golang.org/genai"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

const (
	// GCPModelPublisherGoogle is using by Gemini model family.
	GCPModelPublisherGoogle = "google"
	// GCPModelPublisherAnthropic is using by Calude model family
	GCPModelPublisherAnthropic = "anthropic"
	GCPMethodGenerateContent   = "generateContent"
)

// -------------------------------------------------------------
// Request Conversion Helper for OpenAI to GCP Gemini Translator
// -------------------------------------------------------------

// toGeminiContents converts OpenAI messages to Gemini Contents and SystemInstruction
func toGeminiContents(messages []openai.ChatCompletionMessageParamUnion) ([]genai.Content, *genai.Content, error) {
	var gcpContents []genai.Content
	var systemInstruction *genai.Content
	knownToolCalls := make(map[string]string)
	var gcpParts []*genai.Part

	for _, msgUnion := range messages {
		switch msgUnion.Type {
		case openai.ChatMessageRoleDeveloper:
			msg := msgUnion.Value.(openai.ChatCompletionDeveloperMessageParam)
			inst, err := fromDeveloperMsg(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting developer message: %w", err)
			}
			if len(inst) != 0 {
				if systemInstruction == nil {
					systemInstruction = &genai.Content{Role: genai.RoleUser}
				}
				systemInstruction.Parts = append(systemInstruction.Parts, inst...)
			}
		case openai.ChatMessageRoleSystem:
			msg := msgUnion.Value.(openai.ChatCompletionSystemMessageParam)
			devMsg := systemMsgToDeveloperMsg(msg)
			inst, err := fromDeveloperMsg(devMsg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting developer message: %w", err)
			}
			if len(inst) != 0 {
				if systemInstruction == nil {
					systemInstruction = &genai.Content{Role: genai.RoleUser}
				}
				systemInstruction.Parts = append(systemInstruction.Parts, inst...)
			}
		case openai.ChatMessageRoleUser:
			msg := msgUnion.Value.(openai.ChatCompletionUserMessageParam)
			parts, err := fromUserMsg(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting user message: %w", err)
			}
			gcpParts = append(gcpParts, parts...)
		case openai.ChatMessageRoleTool:
			msg := msgUnion.Value.(openai.ChatCompletionToolMessageParam)
			part, err := fromToolMsg(msg, knownToolCalls)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting tool message: %w", err)
			}
			gcpParts = append(gcpParts, part)
		case openai.ChatMessageRoleAssistant:
			// flush any accumulated user/tool parts before assistant

			// When using Gemini with parallel tool calls, multiple consecutive tool call messages can appear without an intervening assistant/model message.
			// In this case, all tool message responses for the parallel tool calls must be grouped into a single GCP content.
			// If not grouped, GCP returns an error like:
			// {
			//   "error": {
			//     "code": 400,
			//     "message": "Please ensure that the number of function response parts should be equal to number of function call parts of the function call turn.",
			//     "status": "INVALID_ARGUMENT"
			//   }
			// }

			if len(gcpParts) > 0 {
				gcpContents = append(gcpContents, genai.Content{Role: genai.RoleUser, Parts: gcpParts})
				gcpParts = nil
			}
			msg := msgUnion.Value.(openai.ChatCompletionAssistantMessageParam)
			assistantParts, toolCalls, err := fromAssistantMsg(msg)
			if err != nil {
				return nil, nil, fmt.Errorf("error converting assistant message: %w", err)
			}
			for k, v := range toolCalls {
				knownToolCalls[k] = v
			}
			gcpContents = append(gcpContents, genai.Content{Role: genai.RoleModel, Parts: assistantParts})
		default:
			return nil, nil, fmt.Errorf("invalid role in message: %s", msgUnion.Type)
		}
	}

	// If there are any remaining parts after processing all messages, add them as user content
	if len(gcpParts) > 0 {
		gcpContents = append(gcpContents, genai.Content{Role: genai.RoleUser, Parts: gcpParts})
	}
	return gcpContents, systemInstruction, nil
}

// systemMsgToDeveloperMsg converts OpenAI system message to developer message.
// Since systemMsg is deprecated, this function is provided to maintain backward compatibility
func systemMsgToDeveloperMsg(msg openai.ChatCompletionSystemMessageParam) openai.ChatCompletionDeveloperMessageParam {
	// Convert OpenAI system message to developer message
	return openai.ChatCompletionDeveloperMessageParam{
		Name:    msg.Name,
		Role:    openai.ChatMessageRoleDeveloper,
		Content: msg.Content,
	}
}

// fromDeveloperMsg converts OpenAI developer message to Gemini Content.
func fromDeveloperMsg(msg openai.ChatCompletionDeveloperMessageParam) ([]*genai.Part, error) {
	var parts []*genai.Part

	switch contentValue := msg.Content.Value.(type) {
	case string:
		if contentValue != "" {
			parts = append(parts, genai.NewPartFromText(contentValue))
		}
	case []openai.ChatCompletionContentPartTextParam:
		if len(contentValue) > 0 {
			for _, textParam := range contentValue {
				if textParam.Text != "" {
					parts = append(parts, genai.NewPartFromText(textParam.Text))
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in developer message: %T", contentValue)

	}
	return parts, nil
}

// fromUserMsg converts OpenAI user message to Gemini Parts.
func fromUserMsg(msg openai.ChatCompletionUserMessageParam) ([]*genai.Part, error) {
	var parts []*genai.Part
	switch contentValue := msg.Content.Value.(type) {
	case string:
		if contentValue != "" {
			parts = append(parts, genai.NewPartFromText(contentValue))
		}
	case []openai.ChatCompletionContentPartUserUnionParam:
		for _, content := range contentValue {
			switch {
			case content.TextContent != nil:
				parts = append(parts, genai.NewPartFromText(content.TextContent.Text))
			case content.ImageContent != nil:
				imgURL := content.ImageContent.ImageURL.URL
				if imgURL == "" {
					// If image URL is empty, we skip it
					continue
				}

				parsedURL, err := url.Parse(imgURL)
				if err != nil {
					return nil, fmt.Errorf("invalid image URL: %w", err)
				}

				if parsedURL.Scheme == "data" {
					mimeType, imgBytes, err := parseDataURI(imgURL)
					if err != nil {
						return nil, fmt.Errorf("failed to parse data URI: %w", err)
					}
					parts = append(parts, genai.NewPartFromBytes(imgBytes, mimeType))
				} else {
					// Identify mimeType based in image url
					mimeType := MimeTypeImageJPEG // Default to jpeg if unknown
					if mt := mime.TypeByExtension(path.Ext(imgURL)); mt != "" {
						mimeType = mt
					}

					parts = append(parts, genai.NewPartFromURI(imgURL, mimeType))
				}
			case content.InputAudioContent != nil:
				// Audio content is currently not supported in this implementation
				return nil, fmt.Errorf("audio content not supported yet")
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in user message: %T", contentValue)
	}
	return parts, nil
}

// fromToolMsg converts OpenAI tool message to Gemini Parts.
func fromToolMsg(msg openai.ChatCompletionToolMessageParam, knownToolCalls map[string]string) (*genai.Part, error) {
	var part *genai.Part
	name := knownToolCalls[msg.ToolCallID]
	funcResponse := ""
	switch contentValue := msg.Content.Value.(type) {
	case string:
		funcResponse = contentValue
	case []openai.ChatCompletionContentPartTextParam:
		for _, textParam := range contentValue {
			if textParam.Text != "" {
				funcResponse += textParam.Text
			}
		}
	default:
		return nil, fmt.Errorf("unsupported content type in tool message: %T", contentValue)
	}

	part = genai.NewPartFromFunctionResponse(name, map[string]any{"output": funcResponse})
	return part, nil
}

// fromAssistantMsg converts OpenAI assistant message to Gemini Parts and known tool calls.
func fromAssistantMsg(msg openai.ChatCompletionAssistantMessageParam) ([]*genai.Part, map[string]string, error) {
	var parts []*genai.Part

	// Handle tool calls in the assistant message
	knownToolCalls := make(map[string]string)
	for _, toolCall := range msg.ToolCalls {
		knownToolCalls[toolCall.ID] = toolCall.Function.Name
		var parsedArgs map[string]any
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &parsedArgs); err != nil {
			return nil, nil, fmt.Errorf("function arguments should be valid json string. failed to parse function arguments: %w", err)
		}
		parts = append(parts, genai.NewPartFromFunctionCall(toolCall.Function.Name, parsedArgs))
	}

	// Handle content in the assistant message
	switch v := msg.Content.Value.(type) {
	case string:
		if v != "" {
			parts = append(parts, genai.NewPartFromText(v))
		}
	case []openai.ChatCompletionAssistantMessageParamContent:
		for _, contPart := range v {
			switch contPart.Type {
			case openai.ChatCompletionAssistantMessageParamContentTypeText:
				if contPart.Text != nil && *contPart.Text != "" {
					parts = append(parts, genai.NewPartFromText(*contPart.Text))
				}
			case openai.ChatCompletionAssistantMessageParamContentTypeRefusal:
				// Refusal messages are currently ignored in this implementation
			default:
				return nil, nil, fmt.Errorf("unsupported content type in assistant message: %s", contPart.Type)
			}
		}
	case nil:
		// No content provided, this is valid
	default:
		return nil, nil, fmt.Errorf("unsupported content type in assistant message: %T", v)
	}

	return parts, knownToolCalls, nil
}

// toGeminiGenerationConfig converts OpenAI request to Gemini GenerationConfig
func toGeminiGenerationConfig(openAIReq *openai.ChatCompletionRequest) (*genai.GenerationConfig, error) {
	if openAIReq == nil {
		return nil, fmt.Errorf("input request is nil")
	}
	gc := &genai.GenerationConfig{}
	if openAIReq.Temperature != nil {
		f := float32(*openAIReq.Temperature)
		gc.Temperature = &f
	}
	if openAIReq.TopP != nil {
		f := float32(*openAIReq.TopP)
		gc.TopP = &f
	}

	if openAIReq.Seed != nil {
		seed := int32(*openAIReq.Seed) // nolint:gosec
		gc.Seed = &seed
	}

	if openAIReq.TopLogProbs != nil {
		logProbs := int32(*openAIReq.TopLogProbs) // nolint:gosec
		gc.Logprobs = &logProbs
	}

	if openAIReq.LogProbs != nil {
		gc.ResponseLogprobs = *openAIReq.LogProbs
	}

	if openAIReq.N != nil {
		gc.CandidateCount = int32(*openAIReq.N) // nolint:gosec
	}
	if openAIReq.MaxTokens != nil {
		gc.MaxOutputTokens = int32(*openAIReq.MaxTokens) // nolint:gosec
	}
	if openAIReq.PresencePenalty != nil {
		gc.PresencePenalty = openAIReq.PresencePenalty
	}
	if openAIReq.FrequencyPenalty != nil {
		gc.FrequencyPenalty = openAIReq.FrequencyPenalty
	}
	if len(openAIReq.Stop) > 0 {
		var stops []string
		for _, s := range openAIReq.Stop {
			if s != nil {
				stops = append(stops, *s)
			}
		}
		gc.StopSequences = stops
	}
	return gc, nil
}

// --------------------------------------------------------------
// Response Conversion Helper for GCP Gemini to OpenAI Translator
// --------------------------------------------------------------

// toOpenAIChoices converts Gemini candidates to OpenAI choices
func toOpenAIChoices(candidates []*genai.Candidate) ([]openai.ChatCompletionResponseChoice, error) {
	choices := make([]openai.ChatCompletionResponseChoice, 0, len(candidates))

	for idx, candidate := range candidates {
		if candidate == nil {
			continue
		}

		// Create the choice
		choice := openai.ChatCompletionResponseChoice{
			Index:        int64(idx),
			FinishReason: toOpenAIFinishReason(candidate.FinishReason),
		}

		if candidate.Content != nil {
			message := openai.ChatCompletionResponseChoiceMessage{
				Role: openai.ChatMessageRoleAssistant,
			}
			// Extract text from parts
			content := extractTextParts(candidate.Content.Parts)
			message.Content = &content

			// Extract tool calls if any
			toolCalls, err := extractToolCalls(candidate.Content.Parts)
			if err != nil {
				return nil, fmt.Errorf("error extracting tool calls: %w", err)
			}
			message.ToolCalls = toolCalls

			// If there's no content but there are tool calls, set content to nil
			if content == "" && len(toolCalls) > 0 {
				message.Content = nil
			}

			choice.Message = message
		}

		// Handle logprobs if available
		if candidate.LogprobsResult != nil {
			choice.Logprobs = toLogprobs(*candidate.LogprobsResult)
		}

		choices = append(choices, choice)
	}

	return choices, nil
}

// toOpenAIFinishReason converts Gemini finish reason to OpenAI finish reason
func toOpenAIFinishReason(reason genai.FinishReason) openai.ChatCompletionChoicesFinishReason {
	switch reason {
	case genai.FinishReasonStop:
		return openai.ChatCompletionChoicesFinishReasonStop
	case genai.FinishReasonMaxTokens:
		return openai.ChatCompletionChoicesFinishReasonLength
	default:
		return openai.ChatCompletionChoicesFinishReasonContentFilter
	}
}

// extractTextParts extracts text from Gemini parts
func extractTextParts(parts []*genai.Part) string {
	var text string
	for _, part := range parts {
		if part != nil && part.Text != "" {
			text += part.Text
		}
	}
	return text
}

// extractToolCalls extracts tool calls from Gemini parts
func extractToolCalls(parts []*genai.Part) ([]openai.ChatCompletionMessageToolCallParam, error) {
	var toolCalls []openai.ChatCompletionMessageToolCallParam

	for _, part := range parts {
		if part == nil || part.FunctionCall == nil {
			continue
		}

		// Convert function call arguments to JSON string
		args, err := json.Marshal(part.FunctionCall.Args)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal function arguments: %w", err)
		}

		// Generate a random ID for the tool call
		toolCallID := uuid.New().String()

		toolCall := openai.ChatCompletionMessageToolCallParam{
			ID:   toolCallID,
			Type: "function",
			Function: openai.ChatCompletionMessageToolCallFunctionParam{
				Name:      part.FunctionCall.Name,
				Arguments: string(args),
			},
		}

		toolCalls = append(toolCalls, toolCall)
	}

	if len(toolCalls) == 0 {
		return nil, nil
	}

	return toolCalls, nil
}

// toOpenAIUsage converts Gemini usage metadata to OpenAI usage
func toOpenAIUsage(metadata *genai.GenerateContentResponseUsageMetadata) openai.ChatCompletionResponseUsage {
	if metadata == nil {
		return openai.ChatCompletionResponseUsage{}
	}

	return openai.ChatCompletionResponseUsage{
		CompletionTokens: int(metadata.CandidatesTokenCount),
		PromptTokens:     int(metadata.PromptTokenCount),
		TotalTokens:      int(metadata.TotalTokenCount),
	}
}

// toLogprobs converts Gemini logprobs to OpenAI logprobs
func toLogprobs(logprobsResult genai.LogprobsResult) openai.ChatCompletionChoicesLogprobs {
	if len(logprobsResult.ChosenCandidates) == 0 {
		return openai.ChatCompletionChoicesLogprobs{}
	}

	content := make([]openai.ChatCompletionTokenLogprob, 0, len(logprobsResult.ChosenCandidates))

	for i := 0; i < len(logprobsResult.ChosenCandidates); i++ {
		chosen := logprobsResult.ChosenCandidates[i]

		var topLogprobs []openai.ChatCompletionTokenLogprobTopLogprob

		// Process top candidates if available
		if i < len(logprobsResult.TopCandidates) && logprobsResult.TopCandidates[i] != nil {
			topCandidates := logprobsResult.TopCandidates[i].Candidates
			if len(topCandidates) > 0 {
				topLogprobs = make([]openai.ChatCompletionTokenLogprobTopLogprob, 0, len(topCandidates))
				for _, tc := range topCandidates {
					topLogprobs = append(topLogprobs, openai.ChatCompletionTokenLogprobTopLogprob{
						Token:   tc.Token,
						Logprob: float64(tc.LogProbability),
					})
				}
			}
		}

		// Create token logprob
		tokenLogprob := openai.ChatCompletionTokenLogprob{
			Token:       chosen.Token,
			Logprob:     float64(chosen.LogProbability),
			TopLogprobs: topLogprobs,
		}

		content = append(content, tokenLogprob)
	}

	// Return the logprobs
	return openai.ChatCompletionChoicesLogprobs{
		Content: content,
	}
}

func buildGCPModelPathSuffix(publisher, model, gcpMethod string) string {
	pathSuffix := fmt.Sprintf("publishers/%s/models/%s:%s", publisher, model, gcpMethod)
	return pathSuffix
}
