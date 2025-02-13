/*
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

package instance

import (
	"time"
)

const (
	// Ref: https://cloud.google.com/compute/docs/instances/instance-lifecycle#instance-states
	// PROVISIONING, STAGING, RUNNING, PENDING_STOP, STOPPING, STOPPED, TERMINATED, REPAIRING, SUSPENDING, SUSPENDED
	InstanceStatusProvisioning = "PROVISIONING"
	InstanceStatusStaging      = "STAGING"
	InstanceStatusRunning      = "RUNNING"
	InstanceStatusPendingStop  = "PENDING_STOP"
	InstanceStatusStopping     = "STOPPING"
	InstanceStatusStopped      = "STOPPED"
	InstanceStatusTerminated   = "TERMINATED"
	InstanceStatusRepairing    = "REPAIRING"
	InstanceStatusSuspending   = "SUSPENDING"
	InstanceStatusSuspended    = "SUSPENDED"
)

type Instance struct {
	CreationTime     time.Time         `json:"creationTime"`
	Status           string            `json:"status"`
	ID               string            `json:"id"`
	ImageID          string            `json:"imageId"`
	Type             string            `json:"type"`
	Region           string            `json:"region"`
	Zone             string            `json:"zone"`
	CapacityType     string            `json:"capacityType"`
	SecurityGroupIDs []string          `json:"securityGroupIds"`
	VSwitchID        string            `json:"vSwitchId"`
	Tags             map[string]string `json:"tags"`
}
