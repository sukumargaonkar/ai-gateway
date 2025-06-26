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
	region         string
	projectName    string
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
		region:         gcpAuth.Region,
		projectName:    gcpAuth.ProjectName,
	}, nil
}

// Do implements [Handler.Do].
//
// It modifies the request headers to include the GCP API path and the Authorization header with the GCP access token.
func (g *gcpHandler) Do(_ context.Context, _ map[string]string, headerMut *extprocv3.HeaderMutation, _ *extprocv3.BodyMutation) error {
	// The GCP API path is built in two parts: a prefix generated here,
	// and a suffix provided by translator.requestBody via the ":path" header in headerMut.
	// We combine the prefix with suffix and update the header in headerMut.
	prefixPath := fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s", g.region, g.projectName, g.region)
	for _, hdr := range headerMut.SetHeaders {
		if hdr.Header != nil && hdr.Header.Key == ":path" {
			if len(hdr.Header.Value) > 0 {
				suffixPath := hdr.Header.Value
				hdr.Header.Value = fmt.Sprintf("%s/%s", prefixPath, suffixPath)
			}
			if len(hdr.Header.RawValue) > 0 {
				suffixPath := string(hdr.Header.RawValue)
				path := fmt.Sprintf("%s/%s", prefixPath, suffixPath)
				hdr.Header.RawValue = []byte(path)
			}
			break
		}
	}

	headerMut.SetHeaders = append(
		headerMut.SetHeaders,
		&corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:      "Authorization",
				RawValue: []byte(fmt.Sprintf("Bearer %s", g.gcpAccessToken)),
			},
		},
	)

	return nil
}
