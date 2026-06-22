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

package instance

import (
	"fmt"
	"strings"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func secondaryBootDiskImageDeviceName(image string) string {
	return fmt.Sprintf("gke-%s-disk", secondaryBootDiskImageName(image))
}

func secondaryBootDiskImageName(image string) string {
	parts := strings.Split(image, "/")
	return parts[len(parts)-1]
}

func secondaryBootDisks(nodeClass *v1alpha1.GCENodeClass) []v1alpha1.Disk {
	disks := make([]v1alpha1.Disk, 0, len(nodeClass.Spec.Disks))
	for _, disk := range nodeClass.Spec.Disks {
		if disk.Boot || disk.SecondaryBootImage == "" || disk.SecondaryBootMode == "MODE_UNSPECIFIED" {
			continue
		}
		disks = append(disks, disk)
	}
	return disks
}
