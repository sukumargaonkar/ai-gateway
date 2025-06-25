// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"io"

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
// This translator converts OpenAI ChatCompletion API requests to GCP Gemini API format.
func NewChatCompletionOpenAIToGCPGeminiTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPGeminiTranslatorV1ChatCompletion{}
}

type openAIToGCPGeminiTranslatorV1ChatCompletion struct{}

// RequestBody implements [Translator.RequestBody] for GCP Gemini.
// This method translates an OpenAI ChatCompletion request to a GCP Gemini API request.
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) RequestBody(_ []byte, openAIReq *openai.ChatCompletionRequest, onRetry bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	_, _ = openAIReq, onRetry
	pathSuffix := buildGCPModelPathSuffix(GCPModelPublisherGoogle, openAIReq.Model, GCPMethodGenerateContent)

	// TODO: Implement actual translation from OpenAI to Gemini request.

	headerMutation, bodyMutation = buildGCPRequestMutations(pathSuffix, nil)
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

// ResponseBody implements [Translator.ResponseBody] for GCP Gemini.
// This method translates a GCP Gemini API response to the OpenAI ChatCompletion format.
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	// TODO: Implement response body translation from GCP Gemini to OpenAI format
	_, _, _ = respHeaders, body, endOfStream
	return nil, nil, LLMTokenUsage{}, nil
}
