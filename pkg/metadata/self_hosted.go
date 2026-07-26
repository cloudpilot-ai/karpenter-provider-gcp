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
	"maps"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
)

const (
	// ClusterNameKey is the provider-owned metadata key carrying the exact cluster name.
	// Distributions that authorize joining nodes through instance metadata (e.g. kOps'
	// TPM node authorizer) match on this value.
	ClusterNameKey = "cluster-name"
	// StartupScriptKey is the metadata key executed natively by the GCE guest agent.
	StartupScriptKey = "startup-script"

	// MaxMetadataValueBytes is GCE's limit on a single metadata value, in UTF-8 bytes.
	MaxMetadataValueBytes = 262144
	// MaxMetadataTotalBytes is GCE's limit on the combined size of all metadata keys and
	// values on one instance, in UTF-8 bytes.
	MaxMetadataTotalBytes = 524288
)

// SelfHostedComputeMetadata renders self-hosted instance metadata: the user's
// spec.metadata keys, the provider-owned cluster-name key, and startup-script when
// non-empty — never the GKE bootstrap keys the InstanceMetadata model always renders.
func SelfHostedComputeMetadata(clusterName, startupScript string, custom CustomMetadata) *compute.Metadata {
	values := metadataValues{}
	maps.Copy(values, custom)
	delete(values, "")
	values[ClusterNameKey] = clusterName
	if startupScript != "" {
		values[StartupScriptKey] = startupScript
	}
	return toComputeMetadata(values)
}

// HasValue reports whether md carries key with a value exactly equal to value.
func HasValue(md *compute.Metadata, key, value string) bool {
	if md == nil {
		return false
	}
	for _, item := range md.Items {
		if item != nil && item.Key == key {
			return lo.FromPtr(item.Value) == value
		}
	}
	return false
}

// ValidateComputeMetadataSize checks rendered metadata against GCE's byte limits.
// CRD maxLength counts characters, not bytes, so a multi-byte value can pass CEL yet
// be rejected by GCE; failing here names the offending key.
func ValidateComputeMetadataSize(md *compute.Metadata) error {
	total := 0
	for _, item := range md.Items {
		value := lo.FromPtr(item.Value)
		if len(value) > MaxMetadataValueBytes {
			return fmt.Errorf("metadata value for key %q is %d bytes, exceeding the GCE per-value limit of %d bytes",
				item.Key, len(value), MaxMetadataValueBytes)
		}
		total += len(item.Key) + len(value)
	}
	if total > MaxMetadataTotalBytes {
		return fmt.Errorf("metadata keys and values total %d bytes, exceeding the GCE per-instance limit of %d bytes",
			total, MaxMetadataTotalBytes)
	}
	return nil
}
