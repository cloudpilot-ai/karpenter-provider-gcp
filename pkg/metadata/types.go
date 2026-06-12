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
	corev1 "k8s.io/api/core/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
)

const (
	KubeEnvKey       = "kube-env"
	KubeLabelsKey    = "kube-labels"
	KubeletConfigKey = "kubelet-config"
)

// InstanceMetadata is the complete typed metadata model for a provisioned GCE
// instance. Its fields are private so callers use intent-level helpers instead
// of depending on the storage surface used to render Compute Engine metadata.
//
// It deliberately has no generic raw/passthrough map for source-template
// metadata. If the provider needs another GKE-managed source field, add a typed
// field or explicit copy rule so ownership is reviewable.
type InstanceMetadata struct {
	kubeEnv KubeEnv

	// kubeLabels models the GKE "kube-labels" metadata item. This metadata item
	// does not apply labels to the registered Kubernetes Node; kubelet registration
	// labels come from kubeEnv.kubeletArgs.nodeLabels. In GKE source templates this
	// surface appears to contain GKE/bootstrap system labels for metadata readers,
	// so keep it distinct from both kubelet node labels and GCE instance labels.
	kubeLabels Labels

	kubeletConfig kubeletconfig.KubeletConfiguration

	// customMetadata contains explicit user-provided spec.metadata keys. These
	// are user-owned values, not an implicit passthrough of unknown source
	// template metadata.
	customMetadata CustomMetadata

	// instanceLabels models compute.Instance.Labels. These are not Compute
	// metadata items, but they are assembled with the same ownership rules and
	// serialized at the instance boundary.
	instanceLabels InstanceLabels
}

// KubeEnv models the GKE kube-env metadata item. Unknown kube-env entries may be
// preserved inside this type while provider-owned fields are exposed through
// typed sub-structures and setters.
type KubeEnv struct {
	entries     KubeEnvEntries
	kubeletArgs KubeletArgs
}

// KubeEnvEntries contains kube-env key/value entries outside KUBELET_ARGS.
type KubeEnvEntries map[string]string

// KubeletArgs models the KUBELET_ARGS line embedded in kube-env. It is a
// boundary model for metadata surfaces the provider edits today; runtime callers
// should use intent-level helpers on InstanceMetadata rather than reaching into
// this structure.
type KubeletArgs struct {
	// nodeLabels are rendered into kube-env's KUBELET_ARGS --node-labels flag.
	// Unlike InstanceMetadata.kubeLabels, these labels are consumed by kubelet
	// during node registration and become Kubernetes Node labels at bootstrap.
	nodeLabels Labels
	nodeTaints Taints
	maxPods    *int32

	// other contains unknown/unowned raw kubelet arg tokens preserved at the
	// KUBELET_ARGS boundary. It is a best-effort strings.Fields round-trip, not
	// provider-owned typed model state or shell parsing.
	other []string
}

// Labels models a label set rendered to GKE bootstrap metadata surfaces.
type Labels = k8slabels.Set

// Taints models kubelet --register-with-taints values.
type Taints []corev1.Taint

// CustomMetadata models user-provided GCENodeClass spec.metadata values.
type CustomMetadata map[string]string

// InstanceLabels models GCE compute instance labels. These are cloud-resource
// labels used for GCP filtering/accounting and are not Kubernetes node labels or
// Compute metadata items. The apply/render boundary converts this set to the
// GCP API's expected map shape.
type InstanceLabels = k8slabels.Set
