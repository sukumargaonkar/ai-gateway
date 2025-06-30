// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/genai"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

type GenerateContentRequest struct {
	Contents          []genai.Content         `json:"contents"`
	Tools             []genai.Tool            `json:"tools"`
	ToolConfig        *genai.ToolConfig       `json:"tool_config,omitempty"`
	GenerationConfig  *genai.GenerationConfig `json:"generation_config,omitempty"`
	SystemInstruction *genai.Content          `json:"system_instruction,omitempty"`
}

// NewChatCompletionOpenAIToGCPGeminiTranslator implements [Factory] for OpenAI to GCP Gemini translation.
func NewChatCompletionOpenAIToGCPGeminiTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPGeminiTranslatorV1ChatCompletion{}
}

type openAIToGCPGeminiTranslatorV1ChatCompletion struct{}

// RequestBody implements [Translator.RequestBody] for GCP Gemini.
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, _ bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	gcpReq, err := o.openAIMessageToGeminiMessage(openAIReq)
	if err != nil {
		return nil, nil, fmt.Errorf("error converting OpenAI request to Gemini request: %w", err)
	}

	// Trim the model prefix if needed
	model := strings.TrimPrefix(openAIReq.Model, "gcp.") // TODO: remove before pushing upstream
	pathSuffix := buildGCPModelPathSuffix(GCPModelPublisherGoogle, model, GCPMethodGenerateContent)

	// Marshal the request body to JSON
	reqBodyBytes, err := json.Marshal(gcpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("error marshaling Gemini request: %w", err)
	}

	headerMutation, bodyMutation = buildGCPRequestMutations(pathSuffix, reqBodyBytes)
	return headerMutation, bodyMutation, nil
}

// ResponseHeaders implements [Translator.ResponseHeaders].
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseHeaders(headers map[string]string) (
	headerMutation *extprocv3.HeaderMutation, err error,
) {
	// TODO: Implement if needed.
	_ = headers
	return nil, nil
}

// ResponseError implements [Translator.ResponseError].
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseError(respHeaders map[string]string, body interface{}) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// TODO: Implement error translation.
	_, _ = respHeaders, body
	return nil, nil, nil
}

func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseBody(_ map[string]string, body io.Reader, _ bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	// Read the body
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, LLMTokenUsage{}, fmt.Errorf("error reading response body: %w", err)
	}

	// Parse the GCP response
	var gcpResp genai.GenerateContentResponse
	if err = json.Unmarshal(bodyBytes, &gcpResp); err != nil {
		return nil, nil, LLMTokenUsage{}, fmt.Errorf("error unmarshaling GCP response: %w", err)
	}

	// Convert to OpenAI format
	openAIResp, err := o.geminiResponseToOpenAIMessage(gcpResp)
	if err != nil {
		return nil, nil, LLMTokenUsage{}, fmt.Errorf("error converting GCP response to OpenAI format: %w", err)
	}

	// Marshal the OpenAI response
	openAIRespBytes, err := json.Marshal(openAIResp)
	if err != nil {
		return nil, nil, LLMTokenUsage{}, fmt.Errorf("error marshaling OpenAI response: %w", err)
	}

	// Update token usage if available
	var usage LLMTokenUsage
	if gcpResp.UsageMetadata != nil {
		usage = LLMTokenUsage{
			InputTokens:  uint32(gcpResp.UsageMetadata.PromptTokenCount),     // nolint:gosec
			OutputTokens: uint32(gcpResp.UsageMetadata.CandidatesTokenCount), // nolint:gosec
			TotalTokens:  uint32(gcpResp.UsageMetadata.TotalTokenCount),      // nolint:gosec
		}
	}

	// Create response header mutation
	headerMutation = &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{
				Header: &corev3.HeaderValue{
					Key:      "content-length",
					RawValue: []byte(strconv.Itoa(len(openAIRespBytes))),
				},
			},
			{
				Header: &corev3.HeaderValue{
					Key:      "content-type",
					RawValue: []byte("application/json"),
				},
			},
		},
	}

	// Create response body mutation
	bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{Body: openAIRespBytes},
	}

	return headerMutation, bodyMutation, usage, nil
}

func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) openAIMessageToGeminiMessage(openAIReq *openai.ChatCompletionRequest) (GenerateContentRequest, error) {
	// Convert OpenAI messages to Gemini Contents and SystemInstruction
	contents, systemInstruction, err := toGeminiContents(openAIReq.Messages)
	if err != nil {
		return GenerateContentRequest{}, err
	}

	// Convert generation config
	generationConfig, err := toGeminiGenerationConfig(openAIReq)
	if err != nil {
		return GenerateContentRequest{}, fmt.Errorf("error converting generation config: %w", err)
	}

	gcr := GenerateContentRequest{
		Contents:          contents,
		Tools:             nil,
		ToolConfig:        nil,
		GenerationConfig:  generationConfig,
		SystemInstruction: systemInstruction,
	}

	return gcr, nil
}

func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) geminiResponseToOpenAIMessage(gcr genai.GenerateContentResponse) (openai.ChatCompletionResponse, error) {
	// Convert candidates to OpenAI choices
	choices, err := toOpenAIChoices(gcr.Candidates)
	if err != nil {
		return openai.ChatCompletionResponse{}, fmt.Errorf("error converting choices: %w", err)
	}

	// Set up the OpenAI response
	openaiResp := openai.ChatCompletionResponse{
		Choices: choices,
		Object:  "chat.completion",
		Usage:   toOpenAIUsage(gcr.UsageMetadata),
	}

	return openaiResp, nil
}
