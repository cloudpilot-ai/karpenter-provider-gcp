/*
Copyright 2026 The CloudPilot AI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metadata

import "strings"

// ParseLabels parses the comma-separated key=value representation used by
// GKE's kube-labels metadata item and kubelet's --node-labels flag. It is a
// source-template boundary helper; provider-owned target labels should still be
// applied through intent-level helpers.
func ParseLabels(raw string) Labels {
	labels := Labels{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(value)
	}
	return labels
}

func mergeLabels(dst Labels, values Labels) {
	if dst == nil {
		return
	}
	for key, value := range values {
		if key == "" {
			continue
		}
		dst[key] = value
	}
}
