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
	"google.golang.org/api/compute/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const (
	KubeEnvKey       = "kube-env"
	KubeLabelsKey    = "kube-labels"
	KubeletConfigKey = "kubelet-config"
)

// InstanceMetadata is the complete typed metadata model for a provisioned GCE
// instance. It contains only metadata surfaces the provider intentionally owns
// or explicitly copies from the GKE source template.
//
// It deliberately has no generic raw/passthrough map for source-template
// metadata. If the provider needs another GKE-managed source field, add a typed
// field or explicit copy rule so ownership is reviewable.
type InstanceMetadata struct {
	KubeEnv       KubeEnv
	KubeLabels    KubeLabels
	KubeletConfig KubeletConfig

	// CustomMetadata contains explicit user-provided spec.metadata keys. These
	// are user-owned values, not an implicit passthrough of unknown source
	// template metadata.
	CustomMetadata CustomMetadata

	// InstanceLabels models compute.Instance.Labels. These are not Compute
	// metadata items, but they are assembled with the same ownership rules and
	// serialized at the instance boundary.
	InstanceLabels InstanceLabels
}

// SourceTemplateMetadata is the explicitly supported subset copied from a GKE
// source instance template. It is separate from InstanceMetadata so constructor
// code can make source inheritance visible and narrow.
type SourceTemplateMetadata struct {
	KubeEnv       KubeEnv
	KubeLabels    KubeLabels
	KubeletConfig KubeletConfig
}

// KubeEnv models the GKE kube-env metadata item. Unknown kube-env entries may be
// preserved inside this type while provider-owned fields are exposed through
// typed sub-structures and setters.
type KubeEnv struct {
	Entries     KubeEnvEntries
	KubeletArgs KubeletArgs
}

// KubeEnvEntries contains kube-env key/value entries outside KUBELET_ARGS.
type KubeEnvEntries map[string]string

// KubeletArgs models the KUBELET_ARGS line embedded in kube-env.
type KubeletArgs struct {
	NodeLabels         KubeLabels
	RegisterWithTaints Taints
	MaxPods            *int32
	RawArgs            []string
}

// KubeLabels models Kubernetes node labels used by kube-labels metadata and
// kubelet --node-labels.
type KubeLabels map[string]string

// Taints models kubelet --register-with-taints values.
type Taints []Taint

type Taint struct {
	Key    string
	Value  string
	Effect string
}

// KubeletConfig models kubelet-config metadata. The provider API type is used
// for fields already exposed through GCENodeClass; Raw preserves explicitly
// copied source keys until the provider models or drops them.
type KubeletConfig struct {
	Config v1alpha1.KubeletConfiguration
	Raw    map[string]any
}

// CustomMetadata models user-provided GCENodeClass spec.metadata values.
type CustomMetadata map[string]string

// InstanceLabels models GCE instance labels.
type InstanceLabels map[string]string

// FromSourceTemplate will parse only the explicitly supported fields from a GKE
// source template. Implementation is intentionally deferred until the structure
// definitions and ownership boundaries are reviewed.
func FromSourceTemplate(_ *compute.Metadata) (*SourceTemplateMetadata, error) {
	return nil, nil
}
