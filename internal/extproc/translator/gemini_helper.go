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

	"google.golang.org/genai"

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
			inst, err := fromSystemMsg(msg)
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

// fromSystemMsg converts OpenAI system message to Gemini Content.
func fromSystemMsg(msg openai.ChatCompletionSystemMessageParam) ([]*genai.Part, error) {
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
		return nil, fmt.Errorf("unsupported content type in system message: %T", contentValue)

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
					mimeType := "image/jpeg" // Default to jpeg if unknown
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
