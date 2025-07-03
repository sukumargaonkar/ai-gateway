// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package translator

import (
	"fmt"
)

const (
	GCPModelPublisherGoogle    = "google"
	GCPModelPublisherAnthropic = "anthropic"
	GCPMethodGenerateContent   = "generateContent"
	HTTPHeaderKeyContentLength = "Content-Length"
)

func buildGCPModelPathSuffix(publisher, model, gcpMethod string) string {
	pathSuffix := fmt.Sprintf("publishers/%s/models/%s:%s", publisher, model, gcpMethod)
	return pathSuffix
}
