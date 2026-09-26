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

package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CustomMachineTypeNamePattern matches GCE custom machine type names, e.g.
// "n2-custom-8-24576" or the extended-memory variant "n2-custom-8-24576-ext".
const CustomMachineTypeNamePattern = `^[a-z][a-z0-9]*-custom-[1-9][0-9]*-[1-9][0-9]*(-ext)?$`

// GCECustomMachineTypeSpec registers a GCE custom machine type (e.g. n2-custom-8-24576) so it
// joins the instance type catalog alongside predefined shapes. GCP does not enumerate custom
// shapes through machineTypes.aggregatedList (the API Karpenter otherwise uses to discover
// instance types), so a shape must be registered here before it can be scheduled onto.
type GCECustomMachineTypeSpec struct {
	// MachineType is the real GCE custom machine type name, e.g. "n2-custom-8-24576". Immutable
	// after creation; create a new object to register a different shape.
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9]*-custom-[1-9][0-9]*-[1-9][0-9]*(-ext)?$`
	// +kubebuilder:validation:XValidation:message="machineType is immutable",rule="self == oldSelf"
	// +required
	MachineType string `json:"machineType"`
	// Prices are the hourly prices Karpenter uses for scheduling and consolidation decisions.
	// GCP does not publish prices for custom shapes (unlike predefined ones), so they must be
	// supplied explicitly until the pricing provider can compute them (see proposals/0007).
	// +required
	Prices GCECustomMachineTypePrices `json:"prices"`
}

// GCECustomMachineTypePrices are decimal-string USD/hour prices, matching the currency
// representation used at other API boundaries in this codebase to avoid float precision
// issues over the wire.
type GCECustomMachineTypePrices struct {
	// OnDemand is the on-demand hourly price in USD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +required
	OnDemand string `json:"onDemand"`
	// Spot is the Spot hourly price in USD.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?$`
	// +required
	Spot string `json:"spot"`
}

// GCECustomMachineTypeStatus contains the resolved state of the GCECustomMachineType, as
// discovered from GCE via machineTypes.get (which, unlike machineTypes.aggregatedList,
// supports resolving a specific valid custom shape on demand).
type GCECustomMachineTypeStatus struct {
	// GuestCpus is the resolved vCPU count for MachineType.
	// +optional
	GuestCpus int32 `json:"guestCpus,omitempty"`
	// MemoryMb is the resolved memory, in MB, for MachineType.
	// +optional
	MemoryMb int32 `json:"memoryMb,omitempty"`
	// Zones lists the cluster zones where MachineType was confirmed available.
	// +optional
	Zones []string `json:"zones,omitempty"`
	// Conditions contains signals for health and readiness.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// GCECustomMachineType registers a GCE custom machine type so the instance type provider can
// discover it, price it, and make it available for scheduling like any predefined shape. See
// proposals/0007-custom-machine-type-catalog.md.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:resource:path=gcecustommachinetypes,scope=Cluster,categories=karpenter,shortName={gcecmt,gcecmts}
// +kubebuilder:printcolumn:name="MachineType",type="string",JSONPath=".spec.machineType",description=""
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:subresource:status
type GCECustomMachineType struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GCECustomMachineTypeSpec   `json:"spec"`
	Status GCECustomMachineTypeStatus `json:"status,omitempty"`
}

func (in *GCECustomMachineType) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions().For(in, opts...)
}

func (in *GCECustomMachineType) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *GCECustomMachineType) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

// GCECustomMachineTypeList contains a list of GCECustomMachineType
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type GCECustomMachineTypeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GCECustomMachineType `json:"items"`
}
