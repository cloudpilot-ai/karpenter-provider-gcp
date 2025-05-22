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

// Instance is an internal data structure for GCE instances
type Instance struct {
	CapacityReservationID string            `json:"capacityReservationId"`
	CapacityType          string            `json:"capacityType"`
	CreationTime          time.Time         `json:"creationTime"`
	ImageID               string            `json:"imageId"`
	InstanceID            string            `json:"instanceId"`
	InstanceTemplate      string            `json:"instanceTemplate"`
	Labels                map[string]string `json:"labels"`
	Location              string            `json:"location"`
	Name                  string            `json:"name"`
	ProjectID             string            `json:"projectId"`
	Status                string            `json:"status"`
	Tags                  map[string]string `json:"tags"`
	Type                  string            `json:"type"`
}
