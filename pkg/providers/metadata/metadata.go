/*
Copyright 2025 The CloudPilot AI Authors.

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
	"fmt"
	"strings"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const (
	ClusterNameLabel = "cluster-name"
	KubeEnvKey       = "kube-env"
	KubeLabelsKey    = "kube-labels"
	KubeletConfigKey = "kubelet-config"

	GKENodePoolLabel       = "cloud.google.com/gke-nodepool"
	MaxPodsLabel           = "max-pods"
	MaxPodsPerNodeLabel    = "max-pods-per-node"
	GKEProvisioningLabel   = "gke-provisioning"
	KubeletMaxPodsFlagName = "--max-pods"

	UnregisteredTaintValue = "karpenter.sh/unregistered=true:NoExecute"
	// GPUTaintArg is the taint value (not the full flag) merged into the existing
	// --register-with-taints flag to avoid overwriting other taints set earlier.
	GPUTaintArg                     = "nvidia.com/gpu=present:NoSchedule"
	KubeletConfigLabel              = KubeletConfigKey
	GKESecondaryBootDiskLabelPrefix = "cloud.google.com.node-restriction.kubernetes.io/gke-secondary-boot-disk-"
)

var (
	RegisteredLabel = fmt.Sprintf("%s=%s", karpv1.NodeRegisteredLabelKey, "true")
)

// ApplyCustomMetadata applies custom metadata from GCENodeClass to the instance metadata.
// User-supplied keys override any value inherited from the base instance template;
// keys not already present are appended. Empty values are ignored, so a key cannot
// be used to clear a value inherited from the base template.
func ApplyCustomMetadata(values InstanceMetadata, customMetadata map[string]string) {
	if len(customMetadata) == 0 {
		return
	}

	values.MergeUser(customMetadata, nil)
}

func AppendSecondaryBootDisks(projectID string, nodeClass *v1alpha1.GCENodeClass, values InstanceMetadata) {
	for _, disk := range nodeClass.Spec.Disks {
		if disk.Boot || disk.SecondaryBootImage == "" || disk.SecondaryBootMode == "MODE_UNSPECIFIED" {
			continue
		}

		name := GetSecondaryDiskImageName(disk.SecondaryBootImage)
		deviceName := GetSecondaryDiskImageDeviceName(disk.SecondaryBootImage)
		if kubeEnv, ok := values[KubeEnvKey]; ok {
			// Add SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/DISK_IMAGE_NAME
			lines := strings.Split(kubeEnv, "\n")
			lines = append(lines, fmt.Sprintf("SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/%s", deviceName))
			values[KubeEnvKey] = strings.Join(lines, "\n")
		}
		if rawLabels, ok := values[KubeLabelsKey]; ok {
			labels := ParseKubeLabels(rawLabels)
			labels.Merge(ParseKubeLabels(secondaryBootDiskLabel(name, projectID, disk.SecondaryBootMode)))
			values[KubeLabelsKey] = labels.String()
		}
	}
}

// GetSecondaryDiskImageDeviceName returns the conventional device name for GKE
// secondary disks from the source image.
func GetSecondaryDiskImageDeviceName(image string) string {
	return fmt.Sprintf("gke-%s-disk", GetSecondaryDiskImageName(image))
}

// GetSecondaryDiskImageName extracts the name of the image from either:
// - global/images/DISK_IMAGE_NAME
// - projects/PROJECT_ID/global/images/DISK_IMAGE_NAME
func GetSecondaryDiskImageName(image string) string {
	parts := strings.Split(image, "/")
	return parts[len(parts)-1]
}

func secondaryBootDiskLabel(name, projectID string, mode v1alpha1.SecondaryBootDiskMode) string {
	return fmt.Sprintf("%s-%s=%s.%s", GKESecondaryBootDiskLabelPrefix, name, mode, projectID)
}
