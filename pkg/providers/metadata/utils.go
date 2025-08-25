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
	maxPodsPerNodeRegex = regexp.MustCompile(`max-pods-per-node=\d+`)
	maxPodsRegex        = regexp.MustCompile(`max-pods=\d+`)
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

func SetMaxPodsPerNode(metadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass) error {
	maxPodsVal := v1alpha1.KubeletMaxPods
	if nodeClass.Spec.KubeletConfiguration != nil && nodeClass.Spec.KubeletConfiguration.MaxPods != nil {
		maxPodsVal = int(*nodeClass.Spec.KubeletConfiguration.MaxPods)
	}

	keys := []string{"kube-labels", "kube-env"}
	maxPodsPerNode := fmt.Sprintf("max-pods-per-node=%d", maxPodsVal)
	maxPods := fmt.Sprintf("max-pods=%d", maxPodsVal)

	for _, key := range keys {
		targetEntry, index, ok := lo.FindIndexOf(metadata.Items, func(item *compute.MetadataItems) bool {
			return item.Key == key
		})
		if !ok || index == -1 {
			return fmt.Errorf("%s metadata not found", key)
		}
		targetEntry.Value = swag.String(maxPodsPerNodeRegex.ReplaceAllString(*targetEntry.Value, maxPodsPerNode))
		targetEntry.Value = swag.String(maxPodsRegex.ReplaceAllString(*targetEntry.Value, maxPods))

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

func AppendNodeclaimLabel(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass, metadata *compute.Metadata) {
	// Remove nodePoolLabelEntry from `kube-labels` and `kube-env`
	for _, item := range metadata.Items {
		if item.Key == "kube-labels" {
			labels := getTags(nodeClass, nodeClaim)
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

func getTags(nodeClass *v1alpha1.GCENodeClass, nodeClaim *karpv1.NodeClaim) map[string]string {
	staticTags := map[string]string{
		karpv1.NodePoolLabelKey: nodeClaim.Labels[karpv1.NodePoolLabelKey],
		v1alpha1.LabelNodeClass: nodeClass.Name,
	}
	return lo.Assign(nodeClass.Spec.Tags, staticTags)
}
