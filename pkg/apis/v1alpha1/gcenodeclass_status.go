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
	"github.com/awslabs/operatorpkg/status"
	corev1 "k8s.io/api/core/v1"
)

const (
	ConditionTypeImagesReady  = "ImagesReady"
	ConditionTypeSandboxReady = "SandboxReady"
)

// GCENodeClassStatus contains the resolved state of the GCENodeClass
type GCENodeClassStatus struct {
	// Image contains the current image that are available to the
	// cluster under the Image selectors.
	// +optional
	Images []Image `json:"images,omitempty"`
	// SubnetRanges contains the pod secondary IPv4 ranges considered for launch
	// and their Compute-reported free IPv4 address counts when known.
	// +optional
	SubnetRanges []SubnetRangeStatus `json:"subnetRanges,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// SubnetRangeStatus is a resolved pod secondary range considered for node launch.
type SubnetRangeStatus struct {
	// Name is the subnetwork secondary IPv4 range name.
	// +required
	Name string `json:"name"`
	// TotalFreeIP is the Compute-reported number of free IPv4 addresses in this
	// secondary range. Omitted when unknown; zero means no free addresses were reported.
	// Counts are snapshots, not a guarantee of an allocatable contiguous pod CIDR block.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TotalFreeIP *int64 `json:"totalFreeIP,omitempty"`
}

// Image contains resolved image selector values utilized for node launch
type Image struct {
	// SandboxInstaller binds the immutable runtime artifact to this image.
	// +optional
	SandboxInstaller *SandboxInstaller `json:"sandboxInstaller,omitempty"`
	// SourceImage represents the source image, format like projects/gke-node-images/global/images/gke-1309-gke1046000-cos-113-18244-291-9-c-pre
	// +required
	SourceImage string `json:"sourceImage"`
	// Requirements of the Image to be utilized on an instance type
	// +required
	Requirements []corev1.NodeSelectorRequirement `json:"requirements"`
}

func (in *GCENodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	conditions := []string{ConditionTypeImagesReady}
	if in.SandboxEnabled() {
		conditions = append(conditions, ConditionTypeSandboxReady)
	}
	return status.NewReadyConditions(conditions...).For(in, opts...)
}

func (in *GCENodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *GCENodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

// SandboxInstaller is a public, content-verified gVisor installer identity.
type SandboxInstaller struct {
	// Version is the dated installer tag derived from the image label.
	// +required
	Version string `json:"version"`
	// Digest is the sha256 digest of the validated OCI manifest or index.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +required
	Digest string `json:"digest"`
}
