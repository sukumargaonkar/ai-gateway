// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package backendauth

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/envoyproxy/ai-gateway/filterapi"
)

type gcpHandler struct {
	gcpAccessToken string
}

func newGCPHandler(gcpAuth *filterapi.GCPAuth) (Handler, error) {
	var accessToken string

	content, err := os.Open(gcpAuth.CredentialFileName)
	if err != nil {
		return nil, fmt.Errorf("failed to open GCP credential file '%s': %w", gcpAuth.CredentialFileName, err)
	}

	scanner := bufio.NewScanner(content)
	scanner.Split(bufio.ScanLines)

	for scanner.Scan() {
		splits := strings.Split(scanner.Text(), ":")
		if len(splits) == 2 && strings.TrimSpace(splits[0]) == "client-secret" {
			accessToken = strings.TrimSpace(splits[1])
		}
	}

	return &gcpHandler{
		gcpAccessToken: accessToken,
	}, nil
}

// Do implements [Handler.Do].
//
// Extracts the azure access token from the local file and set it as an authorization header.
func (g *gcpHandler) Do(_ context.Context, requestHeaders map[string]string, headerMut *extprocv3.HeaderMutation, _ *extprocv3.BodyMutation) error {
	requestHeaders["Authorization"] = fmt.Sprintf("Bearer %s", g.gcpAccessToken)
	headerMut.SetHeaders = append(headerMut.SetHeaders, &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{Key: "Authorization", RawValue: []byte(requestHeaders["Authorization"])},
	})
	return nil
}
