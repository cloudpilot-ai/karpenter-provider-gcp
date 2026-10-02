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
	ConditionTypeImagesReady = "ImagesReady"
)

// GCENodeClassStatus contains the resolved state of the GCENodeClass
type GCENodeClassStatus struct {
	// Image contains the current image that are available to the
	// cluster under the Image selectors.
	// +optional
	Images []Image `json:"images,omitempty"`
	// SubnetRanges contains the pod secondary IPv4 ranges considered for launch
	// and their GKE-reported utilization when known.
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
	// Utilization is GKE's reported usage of the range as a decimal string
	// between "0" and "1" when known.
	// +optional
	Utilization *string `json:"utilization,omitempty"`
}

// Image contains resolved image selector values utilized for node launch
type Image struct {
	// SourceImage represents the source image, format like projects/gke-node-images/global/images/gke-1309-gke1046000-cos-113-18244-291-9-c-pre
	// +required
	SourceImage string `json:"sourceImage"`
	// Requirements of the Image to be utilized on an instance type
	// +required
	Requirements []corev1.NodeSelectorRequirement `json:"requirements"`
}

func (in *GCENodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeImagesReady,
	).For(in, opts...)
}

func (in *GCENodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *GCENodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
