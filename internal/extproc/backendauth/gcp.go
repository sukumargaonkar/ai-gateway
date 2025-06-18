// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package backendauth

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/envoyproxy/ai-gateway/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/extproc/translator"
)

type gcpHandler struct {
	gcpAccessToken string // The GCP access token used for authentication
	region         string // The GCP region to use for requests
	projectName    string // The GCP project to use for requests
}

func newGCPHandler(gcpAuth *filterapi.GCPAuth) (Handler, error) {
	if gcpAuth == nil {
		return nil, fmt.Errorf("GCP auth configuration cannot be nil")
	}

	return &gcpHandler{
		gcpAccessToken: gcpAuth.AccessToken,
		region:         gcpAuth.Region,
		projectName:    gcpAuth.ProjectName,
	}, nil
}

// Do implements [Handler.Do].
//
// Sets the GCP access token as an authorization header in the Bearer format.
// Also processes the request path template by replacing placeholders for GCP region and project name
// with their actual values. This is necessary because GCP APIs typically require these values in the URL path.
//
// GCP Vertex AI paths follow this pattern:
// /v1/projects/{projectName}/locations/{region}/publishers/google/models/{modelName}:predict
//
// The path template uses Go's text/template syntax where {{.gcpRegion}} and {{.gcpProjectName}}
// are replaced with actual values from the BackendSecurityPolicy.
//
// Example template: /v1/projects/{{.gcpProjectName}}/locations/{{.gcpRegion}}/publishers/google/models/gemini-pro:predict
// After processing: /v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-pro:predict
func (g *gcpHandler) Do(_ context.Context, requestHeaders map[string]string, headerMut *extprocv3.HeaderMutation, _ *extprocv3.BodyMutation) error {
	// Extract the template path which may contain placeholders for GCP region and project name
	gcpReqPathTemplate := requestHeaders[":path"]
	parsedPath, err := template.New("pathTemplate").Parse(gcpReqPathTemplate)
	if err != nil {
		return fmt.Errorf("invalid request path template '%s': expected placeholders for gcpRegion and gcpProjectName in format '{{.gcpRegion}}' and '{{.gcpProjectName}}'. Error: %w", gcpReqPathTemplate, err)
	}
	reqPath := bytes.Buffer{}
	// Populate template data with GCP region and project name
	// These will replace the template variables in the path
	data := map[string]string{
		translator.GCPRegionTemplateKey:  g.region,
		translator.GCPProjectTemplateKey: g.projectName,
	}
	if err = parsedPath.Execute(&reqPath, data); err != nil {
		return fmt.Errorf("failed to evaluate request path template '%s' with values {region: %s, projectName: %s}: %w",
			gcpReqPathTemplate, g.region, g.projectName, err)
	}

	// Set both the Authorization header with the GCP access token
	// and update the path with the processed template containing actual region and project values
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
