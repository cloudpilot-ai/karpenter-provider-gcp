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

import (
	"fmt"
	"strings"
)

// requiredSourceKubeEnvEntries are kube-env entries that GKE writes into every
// node pool template and that node bootstrap cannot work without. A source
// template without them means GKE changed its bootstrap format or the source
// pool is unusable, so fail the launch instead of creating an instance that
// never registers.
var requiredSourceKubeEnvEntries = []string{
	"CA_CERT",
	"KUBE_MANIFESTS_TAR_URL",
	"KUBERNETES_MASTER_NAME",
	"SERVER_BINARY_TAR_HASH",
	"SERVER_BINARY_TAR_URL",
}

// ValidateSourceKubeEnv rejects sources that cannot bootstrap a node. Errors
// contain only missing field names, never source values.
func ValidateSourceKubeEnv(target *InstanceMetadata) error {
	var missing []string
	for _, key := range requiredSourceKubeEnvEntries {
		if value, ok := target.GetKubeEnvEntry(key); !ok || kubeEnvValueEmpty(value) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("source instance template kube-env is missing required entries: %s", strings.Join(missing, ", "))
	}
	return nil
}

// kubeEnvValueEmpty reports whether a raw kube-env value is empty once
// surrounding whitespace is removed. GKE writes empty values as a quoted
// YAML empty string, such as NODE_LOCAL_SSDS_EXT: "".
func kubeEnvValueEmpty(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == `""` || value == `''`
}
