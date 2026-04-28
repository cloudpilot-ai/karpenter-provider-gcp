/*
Copyright 2024 The CloudPilot AI Authors.

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

package v1alpha1

import (
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis"
)

func init() {
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(RestrictedLabelDomains...)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Insert(
		LabelInstanceCategory,
		LabelInstanceFamily,
		LabelInstanceShape,
		LabelInstanceGeneration,
		LabelInstanceSize,
		LabelInstanceCPU,
		LabelInstanceCPUModel,
		LabelInstanceMemory,
		LabelInstanceGPUName,
		LabelInstanceGPUManufacturer,
		LabelInstanceGPUCount,
		LabelInstanceGPUMemory,
		LabelTopologyZoneID,
		corev1.LabelWindowsBuild,
		LabelGKEAccelerator,
		LabelGKEReadinessCalicoReady,
		LabelGKEReadinessKubeProxyReady,
		LabelGKEReadinessMetadataServerEnabled,
		LabelGKEReadinessMasqAgentReady,
		LabelGKEReadinessNetdReady,
		LabelGKEReadinessNodeLocalDNSReady,
	)
}

var (
	TerminationFinalizer   = apis.Group + "/termination"
	GCPToKubeArchitectures = map[string]string{
		"x86_64": karpv1.ArchitectureAmd64,
		"arm64":  karpv1.ArchitectureArm64,
	}
	WellKnownArchitectures = sets.NewString(
		karpv1.ArchitectureAmd64,
		karpv1.ArchitectureArm64,
	)
	RestrictedLabelDomains = []string{
		apis.Group,
	}
	RestrictedTagPatterns = []*regexp.Regexp{
		// Adheres to cluster name pattern matching as specified in the API spec
		regexp.MustCompile(`^kubernetes\.io/cluster/[0-9A-Za-z][A-Za-z0-9\-_]*$`),
		regexp.MustCompile(fmt.Sprintf("^%s$", regexp.QuoteMeta(karpv1.NodePoolLabelKey))),
		regexp.MustCompile(fmt.Sprintf("^%s$", regexp.QuoteMeta(GCEClusterIDTagKey))),
		regexp.MustCompile(fmt.Sprintf("^%s$", regexp.QuoteMeta(LabelNodeClass))),
		regexp.MustCompile(fmt.Sprintf("^%s$", regexp.QuoteMeta(TagNodeClaim))),
	}

	ResourceNVIDIAGPU  corev1.ResourceName = "nvidia.com/gpu"
	ResourceAMDGPU     corev1.ResourceName = "amd.com/gpu"
	GCEClusterIDTagKey                     = "gce:gce-cluster-id"

	ImageFamilyUbuntu               = "Ubuntu"
	ImageFamilyContainerOptimizedOS = "ContainerOptimizedOS"
	ImageFamilyCustom               = "Custom"

	// LabelGKEAccelerator is applied by Karpenter to GPU nodes so that GKE's
	// NVIDIA device-plugin DaemonSets (which all gate on this label via nodeAffinity)
	// schedule onto Karpenter-provisioned GPU nodes. GKE's NodeRestriction policy
	// prevents the kubelet from self-applying this label via kube-labels metadata,
	// so Karpenter must patch it onto the Node object directly via the NodeClaim
	// scheduling requirements.
	LabelGKEAccelerator = "cloud.google.com/gke-accelerator"

	// GKE readiness-gate labels are applied by the GKE control plane after node
	// registration. System DaemonSets use these as nodeSelector to gate scheduling
	// until the node is ready for each subsystem. Registering them as well-known
	// allows Karpenter's DaemonSet overhead simulation (isDaemonPodCompatible) to
	// include these DaemonSets when sizing instance types, preventing undersizing
	// and node churn. See https://github.com/cloudpilot-ai/karpenter-provider-gcp/issues/202
	LabelGKEReadinessCalicoReady           = "projectcalico.org/ds-ready"
	LabelGKEReadinessKubeProxyReady        = "node.kubernetes.io/kube-proxy-ds-ready"
	LabelGKEReadinessMetadataServerEnabled = "iam.gke.io/gke-metadata-server-enabled"
	LabelGKEReadinessMasqAgentReady        = "node.kubernetes.io/masq-agent-ds-ready"
	LabelGKEReadinessNetdReady             = "cloud.google.com/gke-netd-ready"
	LabelGKEReadinessNodeLocalDNSReady     = "addon.gke.io/node-local-dns-ds-ready"

	LabelNodeClass                           = apis.Group + "/gcenodeclass"
	LabelTopologyZoneID                      = "topology.k8s.gcp/zone-id"
	LabelInstanceCategory                    = apis.Group + "/instance-category"
	LabelInstanceFamily                      = apis.Group + "/instance-family"
	LabelInstanceShape                       = apis.Group + "/instance-shape"
	LabelInstanceGeneration                  = apis.Group + "/instance-generation"
	LabelInstanceSize                        = apis.Group + "/instance-size"
	LabelInstanceCPU                         = apis.Group + "/instance-cpu"
	LabelInstanceCPUModel                    = apis.Group + "/instance-cpu-model"
	LabelInstanceMemory                      = apis.Group + "/instance-memory"
	LabelInstanceGPUName                     = apis.Group + "/instance-gpu-name"
	LabelInstanceGPUManufacturer             = apis.Group + "/instance-gpu-manufacturer"
	LabelInstanceGPUCount                    = apis.Group + "/instance-gpu-count"
	LabelInstanceGPUMemory                   = apis.Group + "/instance-gpu-memory"
	AnnotationGCENodeClassHash               = apis.Group + "/gcenodeclass-hash"
	AnnotationClusterNameTaggedCompatability = apis.CompatibilityGroup + "/cluster-name-tagged"
	AnnotationGCENodeClassHashVersion        = apis.Group + "/gcenodeclass-hash-version"
	AnnotationInstanceTagged                 = apis.Group + "/tagged"

	TagNodeClaim = coreapis.Group + "/nodeclaim"
	TagName      = "Name"
)
