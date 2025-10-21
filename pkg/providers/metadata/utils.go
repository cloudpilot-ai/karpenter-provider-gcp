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
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/go-openapi/swag"
	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

var (
	maxPodsPerNodeRegex  = regexp.MustCompile(`max-pods-per-node=\d+`)
	maxPodsRegex         = regexp.MustCompile(`max-pods=\d+`)
	gkeProvisioningRegex = regexp.MustCompile(`gke-provisioning=\w+`)
)

func GetClusterName(metadata *compute.Metadata) (string, error) {
	// Get cluster name
	clusterNameEntry := lo.Filter(metadata.Items, func(item *compute.MetadataItems, _ int) bool {
		return item.Key == ClusterNameLabel
	})
	if len(clusterNameEntry) != 1 {
		return "", errors.New("cluster name label not found")
	}
	clusterName := swag.StringValue(clusterNameEntry[0].Value)
	if clusterName == "" {
		return "", errors.New("cluster name label is empty")
	}
	return clusterName, nil
}

func RenderKubeletConfigMetadata(metaData *compute.Metadata, instanceType *cloudprovider.InstanceType) error {
	targetEntry, index, ok := lo.FindIndexOf(metaData.Items, func(item *compute.MetadataItems) bool {
		return item.Key == KubeletConfigLabel
	})
	if !ok || index == -1 {
		return errors.New("kubelet-config metadata not found")
	}
	cpuMilliCore := fmt.Sprintf("%dm", instanceType.Overhead.KubeReserved.Cpu().MilliValue())
	memoryMB := fmt.Sprintf("%dMi", instanceType.Overhead.KubeReserved.Memory().Value()/(1024*1024))
	ephemeralStorage := instanceType.Overhead.KubeReserved.StorageEphemeral().String()

	configStr := swag.StringValue(targetEntry.Value)
	if configStr == "" {
		return errors.New("kubelet-config metadata is empty")
	}

	// Parse YAML
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
		return fmt.Errorf("failed to parse kubelet-config YAML: %w", err)
	}

	// Update kubeReserved
	kubeReserved, ok := config["kubeReserved"].(map[string]interface{})
	if !ok {
		kubeReserved = make(map[string]interface{})
	}
	kubeReserved["cpu"] = cpuMilliCore
	kubeReserved["memory"] = memoryMB
	kubeReserved["ephemeral-storage"] = ephemeralStorage
	config["kubeReserved"] = kubeReserved

	// Marshal back to YAML
	updatedYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal updated kubelet-config YAML: %w", err)
	}

	targetEntry.Value = swag.String(string(updatedYAML))
	metaData.Items[index] = targetEntry

	return nil
}

func RemoveGKEBuiltinLabels(metadata *compute.Metadata, nodePoolName string) error {
	nodePoolLabelEntry := fmt.Sprintf("%s=%s", GKENodePoolLabel, nodePoolName)
	// Remove nodePoolLabelEntry from `kube-labels` and `kube-env`
	for _, item := range metadata.Items {
		if item.Key != "kube-labels" && item.Key != "kube-env" {
			continue
		}

		item.Value = swag.String(strings.ReplaceAll(swag.StringValue(item.Value), nodePoolLabelEntry, ""))
	}
	return nil
}

func SetMaxPodsPerNode(metadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass) error {
	maxPods := nodeClass.GetMaxPods()
	keys := []string{"kube-labels", "kube-env"}
	maxPodsPerNode := fmt.Sprintf("max-pods-per-node=%d", maxPods)
	maxPodsStr := fmt.Sprintf("max-pods=%d", maxPods)

	for _, key := range keys {
		targetEntry, index, ok := lo.FindIndexOf(metadata.Items, func(item *compute.MetadataItems) bool {
			return item.Key == key
		})
		if !ok || index == -1 {
			return fmt.Errorf("%s metadata not found", key)
		}
		targetEntry.Value = swag.String(maxPodsPerNodeRegex.ReplaceAllString(*targetEntry.Value, maxPodsPerNode))
		targetEntry.Value = swag.String(maxPodsRegex.ReplaceAllString(*targetEntry.Value, maxPodsStr))

		metadata.Items[index] = targetEntry
	}
	return nil
}

func SetProvisioningModel(metadata *compute.Metadata, model string) error {
	keys := []string{"kube-labels", "kube-env"}
	gkeProvisioning := fmt.Sprintf("gke-provisioning=%s", model)

	for _, key := range keys {
		targetEntry, index, ok := lo.FindIndexOf(metadata.Items, func(item *compute.MetadataItems) bool {
			return item.Key == key
		})
		if !ok || index == -1 {
			return fmt.Errorf("%s metadata not found", key)
		}
		targetEntry.Value = swag.String(gkeProvisioningRegex.ReplaceAllString(*targetEntry.Value, gkeProvisioning))

		metadata.Items[index] = targetEntry
	}
	return nil
}

func PatchUnregisteredTaints(metadata *compute.Metadata) error {
	patchedDone := false

	// Remove nodePoolLabelEntry from kube-labels and kube-env
	for _, item := range metadata.Items {
		if item.Key == "kube-env" {
			kubeEnv := swag.StringValue(item.Value)

			lines := strings.Split(kubeEnv, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "KUBELET_ARGS:") {
					if !strings.Contains(line, UnregisteredTaintArg) {
						// Append the taint argument to the existing KUBELET_ARGS line
						lines[i] = line + " " + UnregisteredTaintArg
						patchedDone = true
					}
				}
			}
			// Rejoin the updated lines into a single string
			item.Value = swag.String(strings.Join(lines, "\n"))
		}
	}

	if !patchedDone {
		return fmt.Errorf("failed to patch unregistered taints")
	}

	return nil
}

func AppendNodeClaimLabel(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, metadata *compute.Metadata) {
	// Remove nodePoolLabelEntry from `kube-labels` and `kube-env`
	for _, item := range metadata.Items {
		if item.Key == "kube-labels" {
			labels := getNodeLabels(nodeClass, nodeClaim)
			labelString := make([]string, 0, len(labels))
			for k, v := range labels {
				// Append the nodeclaim label to kube-labels
				labelString = append(labelString, fmt.Sprintf("%s=%s", k, v))
			}
			item.Value = swag.String(*item.Value + "," + strings.Join(labelString, ","))
		}
	}
}

func AppendRegisteredLabel(metadata *compute.Metadata) {
	// Add registered label in metadata
	for _, item := range metadata.Items {
		if item.Key == "kube-labels" {
			item.Value = swag.String(*item.Value + "," + RegisteredLabel)
		}
	}
}

func getNodeLabels(nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim) map[string]string {
	staticTags := map[string]string{
		karpv1.NodePoolLabelKey: nodeClaim.Labels[karpv1.NodePoolLabelKey],
		v1alpha1.LabelNodeClass: nodeClass.Name,
	}
	return staticTags
}

func AppendSecondaryBootDisks(projectID string, nodeClass *v1alpha1.GCENodeClass, metadata *compute.Metadata) {
	for _, disk := range nodeClass.Spec.Disks {
		if disk.Boot || disk.Image == "" || disk.Mode == "MODE_UNSPECIFIED" {
			continue
		}

		name := GetSecondaryDiskImageName(disk.Image)
		deviceName := GetSecondaryDiskImageDeviceName(disk.Image)
		for _, item := range metadata.Items {
			if item.Key == "kube-env" {
				// Add SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/DISK_IMAGE_NAME
				kubeEnv := swag.StringValue(item.Value)
				lines := strings.Split(kubeEnv, "\n")
				lines = append(lines, fmt.Sprintf("SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/%s", deviceName))
				item.Value = swag.String(strings.Join(lines, "\n"))
			}
			if item.Key == "kube-labels" {
				item.Value = swag.String(*item.Value + "," + secondaryBootDiskLabel(name, projectID, disk.Mode))
			}
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
