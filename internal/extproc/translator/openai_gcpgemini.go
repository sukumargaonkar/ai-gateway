// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package translator

import (
	"fmt"
	"io"
	"strconv"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/envoyproxy/ai-gateway/internal/apischema/openai"
)

// NewChatCompletionOpenAIToGCPGeminiTranslator implements [Factory] for OpenAI to GCP Gemini translation.
func NewChatCompletionOpenAIToGCPGeminiTranslator() OpenAIChatCompletionTranslator {
	return &openAIToGCPGeminiTranslatorV1ChatCompletion{}
}

type openAIToGCPGeminiTranslatorV1ChatCompletion struct{}

// RequestBody implements [Translator.RequestBody] for GCP Gemini.
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) RequestBody(_ []byte, req *openai.ChatCompletionRequest, onRetry bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// TODO: Implement actual translation from OpenAI to Gemini request.
	// For now we just hardcoded an example request
	region := "<REPLACE-ME>"
	project := "<REPLACE-ME>"
	gcpReqPath := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/gemini-2.0-flash-001:generateContent", region, project, region)
	gcpReqBody := []byte(`{
  "contents": {
    "role": "user",
    "parts": [
      {
        "text": "who are you?"
      }
    ]
  }
}`)
	headerMutation = &extprocv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{Header: &corev3.HeaderValue{
				Key:      ":path",
				RawValue: []byte(gcpReqPath),
			}},
			{Header: &corev3.HeaderValue{
				Key:      "content-length",
				RawValue: []byte(strconv.Itoa(len(gcpReqBody))),
			}},
		},
	}
	bodyMutation = &extprocv3.BodyMutation{
		Mutation: &extprocv3.BodyMutation_Body{Body: gcpReqBody},
	}
	return headerMutation, bodyMutation, nil
}

// ResponseHeaders implements [Translator.ResponseHeaders].
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseHeaders(headers map[string]string) (
	headerMutation *extprocv3.HeaderMutation, err error,
) {
	// TODO: Implement if needed.
	return nil, nil
}

// ResponseError implements [Translator.ResponseError].
func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseError(respHeaders map[string]string, body interface{}) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, err error,
) {
	// TODO: Implement error translation.
	return nil, nil, nil
}

func (o *openAIToGCPGeminiTranslatorV1ChatCompletion) ResponseBody(respHeaders map[string]string, body io.Reader, endOfStream bool) (
	headerMutation *extprocv3.HeaderMutation, bodyMutation *extprocv3.BodyMutation, tokenUsage LLMTokenUsage, err error,
) {
	// TODO implement me
	return nil, nil, LLMTokenUsage{}, nil
}
