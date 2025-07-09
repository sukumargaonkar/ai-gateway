// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/require"

	"github.com/envoyproxy/ai-gateway/filterapi"
	internaltesting "github.com/envoyproxy/ai-gateway/internal/testing"
)

// setupDefaultAIGatewayResourcesWithAvailableCredentials sets up the default AI Gateway resources with available
// credentials and returns the path to the resources file and the credentials context.
func setupDefaultAIGatewayResourcesWithAvailableCredentials(t *testing.T) (string, internaltesting.CredentialsContext) {
	credCtx := internaltesting.RequireNewCredentialsContext()
	// Set up the credential substitution.
	t.Setenv("OPENAI_API_KEY", credCtx.OpenAIAPIKey)
	aiGatewayResourcesPath := filepath.Join(t.TempDir(), "ai-gateway-resources.yaml")
	awsCredTmpFile := filepath.Join(t.TempDir(), "aws-credentials")
	err := os.WriteFile(awsCredTmpFile, []byte(credCtx.AWSFileLiteral), 0o600)
	require.NoError(t, err)
	aiGatewayResources := strings.ReplaceAll(aiGatewayDefaultResources, "~/.aws/credentials", awsCredTmpFile)
	err = os.WriteFile(aiGatewayResourcesPath, []byte(aiGatewayResources), 0o600)
	require.NoError(t, err)
	return aiGatewayResourcesPath, credCtx
}

func TestRun(t *testing.T) {
	resourcePath, cc := setupDefaultAIGatewayResourcesWithAvailableCredentials(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		require.NoError(t, run(ctx, cmdRun{Debug: true, Path: resourcePath}, os.Stdout, os.Stderr))
		close(done)
	}()
	defer func() {
		// Make sure the external processor is stopped regardless of the test result.
		cancel()
		<-done
	}()

	// This is the health checking to see the envoy admin is working as expected.
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9901/ready",
			strings.NewReader(""))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("error: %v", err)
			return false
		}
		defer func() {
			require.NoError(t, resp.Body.Close())
		}()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		body := string(raw)
		t.Logf("status=%d, response: %s", resp.StatusCode, body)
		if resp.StatusCode != http.StatusOK && body != "live" {
			return false
		}
		return true
	}, 120*time.Second, 1*time.Second)

	type testCase struct {
		testName, modelName string
		required            internaltesting.RequiredCredential
	}
	cases := []testCase{
		{
			testName:  "openai",
			modelName: "gpt-4o-mini",
			required:  internaltesting.RequiredCredentialOpenAI,
		},
		{
			testName:  "aws",
			modelName: "us.meta.llama3-2-1b-instruct-v1:0",
			required:  internaltesting.RequiredCredentialAWS,
		},
		{
			testName: "openai with fallback route",
			// gpt-4o is not explicitly listed in the route, but it should still work by matching the fallback route.
			modelName: "gpt-4o",
			required:  internaltesting.RequiredCredentialOpenAI,
		},
	}

	const ollamaModelName = "qwen3:0.6b"
	if checkIfOllamaReady(t, ollamaModelName) {
		cases = append(cases, testCase{
			testName:  "ollama",
			modelName: ollamaModelName,
		})
	} else {
		t.Logf("Ollama is not ready for serving the model %s. Skipping the test case. If ollama is already running, then `ollama pull %[1]s`", ollamaModelName)
	}

	for _, tc := range cases {
		t.Run(tc.testName, func(t *testing.T) {
			client := openai.NewClient(option.WithBaseURL("http://localhost:1975" + "/v1/"))
			cc.MaybeSkip(t, tc.required)
			require.Eventually(t, func() bool {
				chatCompletion, err := client.Chat.Completions.New(t.Context(), openai.ChatCompletionNewParams{
					Messages: []openai.ChatCompletionMessageParamUnion{
						openai.UserMessage("Say this is a test"),
					},
					Model: tc.modelName,
				})
				if err != nil {
					t.Logf("error: %v", err)
					return false
				}
				nonEmptyCompletion := false
				for _, choice := range chatCompletion.Choices {
					t.Logf("choice: %s", choice.Message.Content)
					if choice.Message.Content != "" {
						nonEmptyCompletion = true
					}
				}
				return nonEmptyCompletion
			}, 30*time.Second, 2*time.Second)
		})
	}

	t.Run("access metrics", func(t *testing.T) {
		require.Eventually(t, func() bool {
			req, err := http.NewRequest(http.MethodGet, "http://localhost:1064/metrics", nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Logf("Failed to query Prometheus: %v", err)
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			t.Logf("Response: status=%d, body=%s", resp.StatusCode, string(body))
			return resp.StatusCode == http.StatusOK
		}, 2*time.Minute, 1*time.Second)
	})
}

func TestRunCmdContext_writeEnvoyResourcesAndRunExtProc(t *testing.T) {
	resourcePath, _ := setupDefaultAIGatewayResourcesWithAvailableCredentials(t)
	runCtx := &runCmdContext{
		stderrLogger:             slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})),
		envoyGatewayResourcesOut: &bytes.Buffer{},
		tmpdir:                   t.TempDir(),
		// UNIX doesn't like a long UDS path, so we use a short one.
		// https://unix.stackexchange.com/questions/367008/why-is-socket-path-length-limited-to-a-hundred-chars
		udsPath: filepath.Join("/tmp", "run.sock"),
	}
	content, err := os.ReadFile(resourcePath)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	_, err = runCtx.writeEnvoyResourcesAndRunExtProc(ctx, string(content))
	require.NoError(t, err)
	time.Sleep(1 * time.Second)
	cancel()
	// Wait for the external processor to stop.
	time.Sleep(1 * time.Second)
}

func Test_mustStartExtProc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runCtx := &runCmdContext{
		tmpdir: t.TempDir(),
		// UNIX doesn't like a long UDS path, so we use a short one.
		// https://unix.stackexchange.com/questions/367008/why-is-socket-path-length-limited-to-a-hundred-chars
		udsPath:      filepath.Join("/tmp", "run.sock"),
		stderrLogger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})),
	}
	runCtx.mustStartExtProc(ctx, filterapi.MustLoadDefaultConfig())
	time.Sleep(1 * time.Second)
	cancel()
	// Wait for the external processor to stop.
	time.Sleep(1 * time.Second)
}

// checkIfOllamaReady checks if the Ollama server is ready and if the specified model is available.
func checkIfOllamaReady(t *testing.T, modelName string) bool {
	req, err := http.NewRequest(http.MethodGet, "http://localhost:11434/api/tags", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	tags := string(body)
	t.Logf("Ollama tags: %s", tags)
	return strings.Contains(tags, modelName)
}
