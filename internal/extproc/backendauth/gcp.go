// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package backendauth

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"text/template"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/envoyproxy/ai-gateway/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/extproc/translator"
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
// Extracts the azure access token from the local file and set it as an authorization header.
func (g *gcpHandler) Do(_ context.Context, requestHeaders map[string]string, headerMut *extprocv3.HeaderMutation, _ *extprocv3.BodyMutation) error {
	gcpReqPathTemplate := requestHeaders[":path"]
	parsedPath, err := template.New("pathTemplate").Parse(gcpReqPathTemplate)
	if err != nil {
		return fmt.Errorf("invalid request path '%s'. expected placeholders for gcpRegion and gcpProjectName. Error: %w", gcpReqPathTemplate, err)
	}
	reqPath := bytes.Buffer{}
	data := map[string]string{
		translator.GCPRegionTemplateKey:  g.region,
		translator.GCPProjectTemplateKey: g.projectName,
	}
	if err = parsedPath.Execute(&reqPath, data); err != nil {
		return fmt.Errorf("failed to evaluate request path '%s': %w", gcpReqPathTemplate, err)
	}

	headerMut.SetHeaders = append(
		headerMut.SetHeaders,
		&corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:      "Authorization",
				RawValue: []byte(fmt.Sprintf("Bearer %s", g.gcpAccessToken)),
			},
		},
		&corev3.HeaderValueOption{
			Header: &corev3.HeaderValue{
				Key:      ":path",
				RawValue: reqPath.Bytes(),
			},
		},
	)

	return nil
}
