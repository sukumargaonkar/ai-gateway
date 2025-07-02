// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"

	"github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
)

const (
	MimeTypeImageJPEG = "image/jpeg"
	MimeTypeImagePNG  = "image/png"
	MimeTypeImageGIF  = "image/gif"
	MimeTypeImageWEBP = "image/webp"
)

// regDataURI follows the web uri regex definition.
// https://developer.mozilla.org/en-US/docs/Web/URI/Schemes/data#syntax
var regDataURI = regexp.MustCompile(`\Adata:(.+?)?(;base64)?,`)

// parseDataURI parse data uri example: data:image/jpeg;base64,/9j/4AAQSkZJRgABAgAAZABkAAD.
func parseDataURI(uri string) (string, []byte, error) {
	matches := regDataURI.FindStringSubmatch(uri)
	if len(matches) != 3 {
		return "", nil, fmt.Errorf("data uri does not have a valid format")
	}
	l := len(matches[0])
	contentType := matches[1]
	bin, err := base64.StdEncoding.DecodeString(uri[l:])
	if err != nil {
		return "", nil, err
	}
	return contentType, bin, nil
}

func getGCPPath(model, specifier string) string {
	return fmt.Sprintf("/models/%s:%s", model, specifier)
}

// buildGCPRequestMutations creates header and body mutations for GCP requests
// It sets the ":path" header, the "content-length" header and the request body.
func buildGCPRequestMutations(path string, reqBody []byte) (*ext_procv3.HeaderMutation, *ext_procv3.BodyMutation) {
	// Create header mutation
	headerMutation := &ext_procv3.HeaderMutation{
		SetHeaders: []*corev3.HeaderValueOption{
			{
				Header: &corev3.HeaderValue{
					Key:      ":path",
					RawValue: []byte(path),
				},
			},
			{
				Header: &corev3.HeaderValue{
					Key:      "content-length",
					RawValue: []byte(strconv.Itoa(len(reqBody))),
				},
			},
		},
	}

	// Create body mutation
	bodyMutation := &ext_procv3.BodyMutation{
		Mutation: &ext_procv3.BodyMutation_Body{Body: reqBody},
	}

	return headerMutation, bodyMutation
}
