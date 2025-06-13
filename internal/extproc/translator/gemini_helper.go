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
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cast"
	"google.golang.org/genai"
	"k8s.io/utils/ptr"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
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

// toGeminiTools converts OpenAI tools to Gemini tools
// This function combines all the openai tools into a single Gemini Tool as distinct function declarations.
// This is mainly done because some Gemini models do not support multiple tools in a single request.
// This behavior might need to change in future base don model capabilities.
func toGeminiTools(openaiTools []openai.Tool) ([]genai.Tool, error) {
	if len(openaiTools) == 0 {
		return nil, nil
	}
	var functionDecls []*genai.FunctionDeclaration
	for _, tool := range openaiTools {
		if tool.Type == openai.ToolTypeFunction {
			if tool.Function != nil {
				var params map[string]any
				var schema *genai.Schema
				var err error

				if tool.Function.Parameters != nil {
					switch paramsRaw := tool.Function.Parameters.(type) {
					case string:
						if err = json.Unmarshal([]byte(paramsRaw), &params); err != nil {
							return nil, fmt.Errorf("tool's param should be a valid JSON string. invalid JSON schema string provided for tool '%s': %w", tool.Function.Name, err)
						}
					case map[string]any:
						params = paramsRaw
					}

					schema, err = toGeminiSchema(params)
					if err != nil {
						return nil, fmt.Errorf("invalid JSON schema provided for tool '%s': %w", tool.Function.Name, err)
					}
				}
				functionDecl := &genai.FunctionDeclaration{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  schema,
				}
				functionDecls = append(functionDecls, functionDecl)
			}
		}
	}
	if len(functionDecls) == 0 {
		return nil, nil
	}
	return []genai.Tool{{FunctionDeclarations: functionDecls}}, nil
}

// toGeminiSchema converts OpenAI JSON schema to Gemini Schema.
// Gemini Schema is a strict subset of JSON Schema specification. This function ensures only valid fields are retained.
// JSON Schema fields like $ref which can be transformed to Gemini Schema are automatically transformed. Other fields will be dropped.
//
// Gemini Schema: https://cloud.google.com/vertex-ai/docs/reference/rest/v1/projects.locations.cachedContents#Schema
func toGeminiSchema(jsonSchema map[string]any) (*genai.Schema, error) {
	// First, dereference any $ref in the schema
	derefSchema, err := dereferenceJSONSchema(jsonSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to dereference JSON schema: %w", err)
	}

	schema := genai.Schema{}

	// Handle description
	if description, ok := derefSchema["description"].(string); ok {
		schema.Description = description
	}

	// Handle title
	if title, ok := derefSchema["title"].(string); ok {
		schema.Title = title
	}

	// Handle enum
	if enumValues, ok := derefSchema["enum"].([]interface{}); ok {
		for _, val := range enumValues {
			if strVal, ok := val.(string); ok {
				schema.Enum = append(schema.Enum, strVal)
			} else {
				// Convert non-string enum values to strings
				schema.Enum = append(schema.Enum, fmt.Sprintf("%v", val))
			}
		}
	}

	// Extract type field
	var typeStr string
	if typeVal, ok := derefSchema["type"]; ok {
		switch tv := typeVal.(type) {
		case string:
			typeStr = tv
		case []string:
			switch len(tv) {
			case 0:
				return nil, fmt.Errorf("type field in JSON schema cannot be an empty array")
			case 1:
				typeStr = tv[0]
			case 2:
				// Ensure that when two types are specified, one must be "null" and the other a valid type
				if (tv[0] != "null" && tv[1] != "null") || (tv[0] == "null" && tv[1] == "null") {
					return nil, fmt.Errorf(
						"when two values are specified in the type field of JSON schema, one of them must be 'null' and the other must be a valid type. found types: %v", tv,
					)
				}

				// Set typeStr to the non-null type and mark as nullable
				if tv[0] == "null" {
					typeStr = tv[1]
				} else {
					typeStr = tv[0]
				}
				schema.Nullable = ptr.To(true)
			default:
				return nil, fmt.Errorf("multiple types in JSON schema are not supported by Gemini. found types: %v", tv)
			}
		}
	}

	// Parse constraints based on the type
	switch typeStr {
	case "string":
		schema.Type = genai.TypeString

		// Parse string constraints
		if minLength, ok := derefSchema["minLength"].(float64); ok {
			minLengthInt := int64(minLength)
			schema.MinLength = &minLengthInt
		}
		if maxLength, ok := derefSchema["maxLength"].(float64); ok {
			maxLengthInt := int64(maxLength)
			schema.MaxLength = &maxLengthInt
		}
		if pattern, ok := derefSchema["pattern"].(string); ok {
			schema.Pattern = pattern
		}
	case "number":
		schema.Type = genai.TypeNumber

		// Handle numeric constraints
		if minimum, ok := derefSchema["minimum"].(float64); ok {
			schema.Minimum = &minimum
		}
		if maximum, ok := derefSchema["maximum"].(float64); ok {
			schema.Maximum = &maximum
		}
	case "integer":
		schema.Type = genai.TypeInteger
	case "boolean":
		schema.Type = genai.TypeBoolean
	case "array":
		schema.Type = genai.TypeArray

		// Parse array items
		if items, ok := derefSchema["items"].(map[string]interface{}); ok {
			var itemsSchema *genai.Schema
			itemsSchema, err = toGeminiSchema(items)
			if err != nil {
				return nil, fmt.Errorf("error processing array items: %w", err)
			}
			schema.Items = itemsSchema
		}

		// Handle array constraints
		if minItems, ok := derefSchema["minItems"].(float64); ok {
			minItemsInt := int64(minItems)
			schema.MinItems = &minItemsInt
		}
		if maxItems, ok := derefSchema["maxItems"].(float64); ok {
			maxItemsInt := int64(maxItems)
			schema.MaxItems = &maxItemsInt
		}
	case "object":
		schema.Type = genai.TypeObject

		schema.Properties = make(map[string]*genai.Schema)

		// Process properties
		if properties, ok := derefSchema["properties"].(map[string]interface{}); ok {
			for name, propSchemaRaw := range properties {
				var propSchemaMap map[string]interface{}
				if propSchemaMap, ok = propSchemaRaw.(map[string]interface{}); ok {
					var propSchema *genai.Schema
					propSchema, err = toGeminiSchema(propSchemaMap)
					if err != nil {
						return nil, fmt.Errorf("error processing property %s: %w", name, err)
					}
					schema.Properties[name] = propSchema
				}
			}
		}

		// Process required properties
		if required, ok := derefSchema["required"].([]interface{}); ok {
			for _, req := range required {
				if reqStr, ok := req.(string); ok {
					schema.Required = append(schema.Required, reqStr)
				}
			}
		}

		if derefSchema["maxProperties"] != nil {
			var maxPropsInt int64
			if maxPropsInt, err = cast.ToInt64E(derefSchema["maxProperties"]); err != nil {
				return nil, fmt.Errorf("invalid maxProperties value: %v", derefSchema["maxProperties"])
			}
			schema.MaxProperties = &maxPropsInt

		}

		if derefSchema["minProperties"] != nil {
			var minPropsInt int64
			if minPropsInt, err = cast.ToInt64E(derefSchema["minProperties"]); err != nil {
				return nil, fmt.Errorf("invalid minProperties value: %v", derefSchema["maxProperties"])
			}
			schema.MinProperties = &minPropsInt
		}

	case "null", "":
		schema.Type = genai.TypeNULL

	default:
		return nil, fmt.Errorf("unsupported type in JSON schema: %s", typeStr)
	}

	// Handle format
	if format, ok := derefSchema["format"].(string); ok {
		schema.Format = format
	}

	// Handle nullable
	if nullable, ok := derefSchema["nullable"].(bool); ok {
		schema.Nullable = &nullable
	}

	// Handle default
	if defaultVal, ok := derefSchema["default"]; ok {
		schema.Default = defaultVal
	}

	// Handle anyOf
	if anyOf, ok := derefSchema["anyOf"].([]interface{}); ok {
		for _, subSchema := range anyOf {
			if subSchemaMap, ok := subSchema.(map[string]interface{}); ok {
				subGeminiSchema, err := toGeminiSchema(subSchemaMap)
				if err != nil {
					return nil, fmt.Errorf("error processing anyOf schema: %w", err)
				}
				schema.AnyOf = append(schema.AnyOf, subGeminiSchema)
			}
		}
	}

	return &schema, nil
}

// toGeminiToolConfig converts OpenAI tool_choice to Gemini ToolConfig
func toGeminiToolConfig(toolChoice interface{}) (*genai.ToolConfig, error) {
	if toolChoice == nil {
		return nil, nil
	}
	switch tc := toolChoice.(type) {
	case string:
		switch tc {
		case "auto":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}}, nil
		case "none":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}, nil
		case "required":
			return &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}, nil
		}
	case openai.ToolChoice:
		return &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode:                 genai.FunctionCallingConfigModeAny,
				AllowedFunctionNames: []string{tc.Function.Name},
			},
			RetrievalConfig: nil,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported tool choice type: %T", toolChoice)
	}

	return nil, fmt.Errorf("unsupported tool choice value: %v", toolChoice)
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

	if openAIReq.ResponseFormat != nil {
		switch openAIReq.ResponseFormat.Type {
		case openai.ChatCompletionResponseFormatTypeText:
			gc.ResponseMIMEType = "text/plain"
		case openai.ChatCompletionResponseFormatTypeJSONObject:
			gc.ResponseMIMEType = "application/json"
		case openai.ChatCompletionResponseFormatTypeJSONSchema:
			var schemaMap map[string]interface{}

			switch sch := openAIReq.ResponseFormat.JSONSchema.Schema.(type) {
			case string:
				if err := json.Unmarshal([]byte(sch), &schemaMap); err != nil {
					return nil, fmt.Errorf("invalid JSON schema string: %w", err)
				}
			case map[string]interface{}:
				schemaMap = sch
			}

			// Convert JSON schema to Gemini Schema
			schema, err := toGeminiSchema(schemaMap)
			if err != nil {
				return nil, fmt.Errorf("error converting JSON schema: %w", err)
			}

			gc.ResponseMIMEType = "application/json"
			gc.ResponseSchema = schema
		}
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

// -------------------------------------------------------------
// JSON Schema Dereferencing Helper
// -------------------------------------------------------------

func dereferenceJSONSchema(jsonSchema map[string]any) (map[string]any, error) {
	// Make a deep copy of the schema to avoid modifying the original
	result := deepCopyMap(jsonSchema)

	// Process the schema and its nested properties with a max depth of 10
	return dereferenceJSONSchemaInternal(result, result, 0)
}

// deepCopyMap creates a deep copy of a map[string]any
func deepCopyMap(m map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range m {
		switch val := v.(type) {
		case map[string]any:
			result[k] = deepCopyMap(val)
		case []any:
			result[k] = deepCopySlice(val)
		default:
			result[k] = val
		}
	}
	return result
}

// deepCopySlice creates a deep copy of a []any
func deepCopySlice(s []any) []any {
	result := make([]any, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case map[string]any:
			result[i] = deepCopyMap(val)
		case []any:
			result[i] = deepCopySlice(val)
		default:
			result[i] = val
		}
	}
	return result
}

const MaxJSONSchemaDereferenceDepth = 10

// dereferenceJSONSchemaInternal is the recursive implementation of dereferenceJSONSchema
// It takes three parameters:
// - currentObj: the current object being processed
// - rootObj: the root JSON schema object, used to resolve references
// - depth: current recursion depth, used to prevent infinite recursion with circular references
func dereferenceJSONSchemaInternal(currentObj map[string]any, rootObj map[string]any, depth int) (map[string]any, error) {
	// Prevent infinite recursion by limiting max depth to 10
	if depth > MaxJSONSchemaDereferenceDepth {
		return currentObj, fmt.Errorf("maximum recursion depth exceeded while dereferencing JSON schema, possible circular reference detected")
	}
	// Check if the schema has a $ref field
	if ref, ok := currentObj["$ref"].(string); ok {
		// If it does, we need to dereference it
		if ref == "" {
			return nil, fmt.Errorf("empty $ref in JSON schema")
		}

		// Check if the reference is internal (#/...) or external (http://, file://)
		if strings.HasPrefix(ref, "#/") {
			// Handle internal references in JSON Schema
			// Internal references are specified with JSON Pointer notation
			// e.g., #/definitions/Person points to the "Person" definition within the same document

			// Remove the "#/" prefix and split by "/"
			refPath := strings.TrimPrefix(ref, "#/")
			segments := strings.Split(refPath, "/")

			// Extract the referenced data by traversing the JSON structure
			var current any = rootObj
			for _, segment := range segments {
				// JSON pointers might use ~ escaping, so we need to handle that
				segment = strings.ReplaceAll(segment, "~1", "/")
				segment = strings.ReplaceAll(segment, "~0", "~")

				// Try to traverse to the next level
				if m, ok := current.(map[string]any); ok {
					if val, exists := m[segment]; exists {
						current = val
					} else {
						return nil, fmt.Errorf("failed to resolve JSON schema reference: %s - segment %s not found", ref, segment)
					}
				} else {
					return nil, fmt.Errorf("failed to resolve JSON schema reference: %s - not an object at segment %s", ref, segment)
				}
			}

			// Current now points to the referenced data
			if refSchema, ok := current.(map[string]any); ok {
				// Make a deep copy of the referenced schema
				refSchemaCopy := deepCopyMap(refSchema)

				// Recursively dereference the referenced schema
				resolvedRefSchema, err := dereferenceJSONSchemaInternal(refSchemaCopy, rootObj, depth+1)
				if err != nil {
					return nil, err
				}

				// Remove the $ref field
				delete(currentObj, "$ref")

				// Copy all properties from the referenced schema to the current object
				for k, v := range resolvedRefSchema {
					currentObj[k] = v
				}
			} else {
				return nil, fmt.Errorf("referenced schema is not an object: %s", ref)
			}
		} else {
			// Handle external references (not implemented in this simplified version)
			return nil, fmt.Errorf("external schema references are not supported: %s", ref)
		}
	}

	// Process nested properties and objects
	for k, v := range currentObj {
		switch val := v.(type) {
		case map[string]any:
			// Recursively dereference nested objects
			derefValue, err := dereferenceJSONSchemaInternal(val, rootObj, depth+1)
			if err != nil {
				return nil, err
			}
			currentObj[k] = derefValue
		case []any:
			// Process arrays of objects
			newArray := make([]any, len(val))
			for i, item := range val {
				if itemObj, ok := item.(map[string]any); ok {
					derefItem, err := dereferenceJSONSchemaInternal(itemObj, rootObj, depth+1)
					if err != nil {
						return nil, err
					}
					newArray[i] = derefItem
				} else {
					newArray[i] = item
				}
			}
			currentObj[k] = newArray
		}
	}

	return currentObj, nil
}
