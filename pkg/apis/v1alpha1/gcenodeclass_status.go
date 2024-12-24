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
)

const (
	ConditionTypeSecurityGroupsReady = "SecurityGroupsReady"
	ConditionTypeInstanceRAMReady    = "InstanceRAMReady"
)

// SecurityGroup contains resolved SecurityGroup selector values utilized for node launch
type SecurityGroup struct {
	// ID of the security group
	// +required
	ID string `json:"id"`
	// Name of the security group
	// +optional
	Name string `json:"name,omitempty"`
}

// GCENodeClassStatus contains the resolved state of the GCENodeClass
type GCENodeClassStatus struct {
	// SecurityGroups contains the current Security Groups values that are available to the
	// cluster under the SecurityGroups selectors.
	// +optional
	SecurityGroups []SecurityGroup `json:"securityGroups,omitempty"`
	// Conditions contains signals for health and readiness
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

func (in *GCENodeClass) StatusConditions() status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeSecurityGroupsReady,
		ConditionTypeInstanceRAMReady,
	).For(in)
}

func (in *GCENodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *GCENodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}
