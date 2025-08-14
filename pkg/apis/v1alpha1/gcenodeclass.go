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
	"log"
	"strings"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GCENodeClassSpec is the top level specification for the GCP Karpenter Provider.
// This will contain the configuration necessary to launch instances in GCP.
type GCENodeClassSpec struct {
	// ServiceAccount is the GCP IAM service account email to assign to the instance
	// +kubebuilder:validation:Pattern=`^[^@]+@(developer\.gserviceaccount\.com|[^@]+\.iam\.gserviceaccount\.com)$`
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`
	// Disk defines the disks to attach to the provisioned instance.
	// +kubebuilder:validation:MaxItems=10
	// +optional
	Disks []Disk `json:"disks,omitempty"`
	// ImageSelectorTerms is a list of or image selector terms. The terms are ORed.
	// +kubebuilder:validation:XValidation:message="'alias' is improperly formatted, must match the format 'family'",rule="self.all(x, has(x.alias) || has(x.id))"
	// Remove or adjust mutual exclusivity rules since there's only one field
	// +kubebuilder:validation:MinItems:=1
	// +kubebuilder:validation:MaxItems:=30
	// +required
	ImageSelectorTerms []ImageSelectorTerm `json:"imageSelectorTerms" hash:"ignore"`
	// ImageFamily dictates the instance template used when generating launch templates.
	// If no ImageSelectorTerms alias is specified, this field is required.
	// +kubebuilder:validation:Enum:={Ubuntu,ContainerOptimizedOS}
	// +optional
	ImageFamily *string `json:"imageFamily,omitempty"`
	// KubeletConfiguration defines args to be used when configuring kubelet on provisioned nodes.
	// They are a vswitch of the upstream types, recognizing not all options may be supported.
	// Wherever possible, the types and names should reflect the upstream kubelet types.
	// +kubebuilder:validation:XValidation:message="imageGCHighThresholdPercent must be greater than imageGCLowThresholdPercent",rule="has(self.imageGCHighThresholdPercent) && has(self.imageGCLowThresholdPercent) ?  self.imageGCHighThresholdPercent > self.imageGCLowThresholdPercent  : true"
	// +kubebuilder:validation:XValidation:message="evictionSoft OwnerKey does not have a matching evictionSoftGracePeriod",rule="has(self.evictionSoft) ? self.evictionSoft.all(e, (e in self.evictionSoftGracePeriod)):true"
	// +kubebuilder:validation:XValidation:message="evictionSoftGracePeriod OwnerKey does not have a matching evictionSoft",rule="has(self.evictionSoftGracePeriod) ? self.evictionSoftGracePeriod.all(e, (e in self.evictionSoft)):true"
	// +optional
	KubeletConfiguration *KubeletConfiguration `json:"kubeletConfiguration,omitempty"`
	// Tags to be applied on gce resources like instances and launch templates.
	// +kubebuilder:validation:XValidation:message="empty tag keys aren't supported",rule="self.all(k, k != '')"
	// +kubebuilder:validation:XValidation:message="tag contains a restricted tag matching gce:gce-cluster-name",rule="self.all(k, k !='gce:gce-cluster-name')"
	// +kubebuilder:validation:XValidation:message="tag contains a restricted tag matching kubernetes.io/cluster/",rule="self.all(k, !k.startsWith('kubernetes.io/cluster') )"
	// +kubebuilder:validation:XValidation:message="tag contains a restricted tag matching karpenter.sh/nodepool",rule="self.all(k, k != 'karpenter.sh/nodepool')"
	// +kubebuilder:validation:XValidation:message="tag contains a restricted tag matching karpenter.sh/nodeclaim",rule="self.all(k, k !='karpenter.sh/nodeclaim')"
	// +kubebuilder:validation:XValidation:message="tag contains a restricted tag matching karpenter.k8s.gcp/gcenodeclass",rule="self.all(k, k !='karpenter.k8s.gcp/gcenodeclass')"
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
	// Metadata contains key/value pairs to set as instance metadata
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`
	// NetworkTags is a list of network tags to apply to the node.
	// +optional
	NetworkTags []string `json:"networkTags,omitempty"`
}

// ImageSelectorTerm defines selection logic for an image used by Karpenter to launch nodes.
// If multiple fields are used for selection, the requirements are ANDed.
type ImageSelectorTerm struct {
	// Alias specifies which GKE image to select.
	// Valid families include: ContainerOptimizedOS,Ubuntu
	// +kubebuilder:validation:XValidation:message="'alias' is improperly formatted, must match the format 'family@version'",rule="self.matches('^[a-zA-Z0-9]+@.+$')"
	// +kubebuilder:validation:XValidation:message="family is not supported, must be one of the following: 'ContainerOptimizedOS,Ubuntu'",rule="self.find('^[^@]+') in ['ContainerOptimizedOS', 'Ubuntu']"
	// +kubebuilder:validation:MaxLength=60
	// +optional
	Alias string `json:"alias,omitempty"`
	// ID specifies which GKE image to select.
	// +kubebuilder:validation:XValidation:message="'id' is improperly formatted, must match the format 'id'",rule="self.matches('^.*$')"
	// +kubebuilder:validation:MaxLength=160
	// +optional
	ID string `json:"id,omitempty"`
}

// KubeletConfiguration defines args to be used when configuring kubelet on provisioned nodes.
// They are a vswitch of the upstream types, recognizing not all options may be supported.
// Wherever possible, the types and names should reflect the upstream kubelet types.
// https://pkg.go.dev/k8s.io/kubelet/config/v1beta1#KubeletConfiguration
// https://github.com/kubernetes/kubernetes/blob/9f82d81e55cafdedab619ea25cabf5d42736dacf/cmd/kubelet/app/options/options.go#L53
type KubeletConfiguration struct {
	// clusterDNS is a list of IP addresses for the cluster DNS server.
	// Note that not all providers may use all addresses.
	//+optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// MaxPods is an override for the maximum number of pods that can run on
	// a worker node instance.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`
	// PodsPerCore is an override for the number of pods that can run on a worker node
	// instance based on the number of cpu cores. This value cannot exceed MaxPods, so, if
	// MaxPods is a lower value, that value will be used.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	PodsPerCore *int32 `json:"podsPerCore,omitempty"`
	// SystemReserved contains resources reserved for OS system daemons and kernel memory.
	// +kubebuilder:validation:XValidation:message="valid keys for systemReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="systemReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`
	// KubeReserved contains resources reserved for Kubernetes system components.
	// +kubebuilder:validation:XValidation:message="valid keys for kubeReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="kubeReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`
	// EvictionHard is the map of signal names to quantities that define hard eviction thresholds
	// +kubebuilder:validation:XValidation:message="valid keys for evictionHard are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// EvictionSoft is the map of signal names to quantities that define soft eviction thresholds
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoft are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// EvictionSoftGracePeriod is the map of signal names to quantities that define grace periods for each eviction signal
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoftGracePeriod are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`
	// EvictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use when terminating pods in
	// response to soft eviction thresholds being met.
	// +optional
	EvictionMaxPodGracePeriod *int32 `json:"evictionMaxPodGracePeriod,omitempty"`
	// ImageGCHighThresholdPercent is the percent of disk usage after which image
	// garbage collection is always run. The percent is calculated by dividing this
	// field value by 100, so this field must be between 0 and 100, inclusive.
	// When specified, the value must be greater than ImageGCLowThresholdPercent.
	// +kubebuilder:validation:Minimum:=0
	// +kubebuilder:validation:Maximum:=100
	// +optional
	ImageGCHighThresholdPercent *int32 `json:"imageGCHighThresholdPercent,omitempty"`
	// ImageGCLowThresholdPercent is the percent of disk usage before which image
	// garbage collection is never run. Lowest disk usage to garbage collect to.
	// The percent is calculated by dividing this field value by 100,
	// so the field value must be between 0 and 100, inclusive.
	// When specified, the value must be less than imageGCHighThresholdPercent
	// +kubebuilder:validation:Minimum:=0
	// +kubebuilder:validation:Maximum:=100
	// +optional
	ImageGCLowThresholdPercent *int32 `json:"imageGCLowThresholdPercent,omitempty"`
	// CPUCFSQuota enables CPU CFS quota enforcement for containers that specify CPU limits.
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitempty"`
}

type Disk struct {
	// SizeGiB is the size of the disk. Unit: GiB
	// +kubebuilder:validation:XValidation:message="size invalid",rule="self >= 10"
	// +optional
	SizeGiB int32 `json:"sizeGiB"`
	// The category of the disk (e.g., pd-standard, pd-balanced, pd-ssd, pd-extreme).
	// +optional
	Category DiskCategory `json:"category,omitempty"`
	// Indicates that this is a boot disk
	// +optional
	Boot bool `json:"boot"`
}

// DiskCategory represents a disk category type
// +kubebuilder:validation:Enum=hyperdisk-balanced;hyperdisk-balanced-high-availability;hyperdisk-extreme;hyperdisk-ml;hyperdisk-throughput;local-ssd;pd-balanced;pd-extreme;pd-ssd;pd-standard
type DiskCategory string

// GCENodeClass is the Schema for the GCENodeClass API
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=gcenodeclasses,scope=Cluster,categories=karpenter,shortName={gcenc,gcencs}
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:subresource:status
type GCENodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GCENodeClassSpec   `json:"spec,omitempty"`
	Status GCENodeClassStatus `json:"status,omitempty"`
}

const (
	KubeletMaxPods = 110

	// We need to bump the GCENodeClassHashVersion when we make an update to the GCENodeClass CRD under these conditions:
	// 1. A field changes its default value for an existing field that is already hashed
	// 2. A field is added to the hash calculation with an already-set value
	// 3. A field is removed from the hash calculations
	GCENodeClassHashVersion = "v3"
)

func (in *GCENodeClass) Hash() string {
	return fmt.Sprint(lo.Must(hashstructure.Hash([]interface{}{
		in.Spec,
		in.ImageFamily(),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

// GCENodeClassList contains a list of GCENodeClass
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type GCENodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GCENodeClass `json:"items"`
}

func (in *GCENodeClass) ImageFamily() string {
	if in.Spec.ImageFamily != nil {
		return *in.Spec.ImageFamily
	}

	if alias := in.Alias(); alias != nil {
		return alias.Family
	}

	// Unreachable: validation enforces that one of the above conditions must be met
	return ImageFamilyCustom
}

type Alias struct {
	Family  string
	Version string
}

func (in *GCENodeClass) Alias() *Alias {
	term, ok := lo.Find(in.Spec.ImageSelectorTerms, func(term ImageSelectorTerm) bool {
		return term.Alias != ""
	})
	if !ok {
		return nil
	}
	return &Alias{
		Family:  imageFamilyFromAlias(term.Alias),
		Version: imageVersionFromAlias(term.Alias),
	}
}

func imageFamilyFromAlias(alias string) string {
	components := strings.Split(alias, "@")
	if len(components) != 2 {
		log.Fatalf("failed to parse AMI alias %q, invalid format", alias)
	}
	family, ok := lo.Find([]string{
		ImageFamilyUbuntu,
		ImageFamilyContainerOptimizedOS,
	}, func(family string) bool {
		return family == components[0]
	})
	if !ok {
		log.Fatalf("%q is an invalid alias family", components[0])
	}
	return family
}

func imageVersionFromAlias(alias string) string {
	components := strings.Split(alias, "@")
	if len(components) != 2 {
		log.Fatalf("failed to parse image alias %q, invalid format", alias)
	}
	return components[1]
}
