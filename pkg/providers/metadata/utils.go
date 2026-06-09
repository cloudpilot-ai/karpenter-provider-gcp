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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/samber/lo"
	"google.golang.org/api/compute/v1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

var (
	maxPodsRegex                  = regexp.MustCompile(`max-pods=\d+`)
	gkeProvisioningRegex          = regexp.MustCompile(`gke-provisioning=\w+`)
	kubeEnvArchRegex              = regexp.MustCompile(`\barch=(amd64|arm64)\b`)
	kubeEnvFamilyRegex            = regexp.MustCompile(`cloud\.google\.com/machine-family=[^,;\s]+`)
	diskTypeLabelRegex            = regexp.MustCompile(`^disk-type\.gke\.io/`)
	diskTypeLabelEntryRegex       = regexp.MustCompile(`,?disk-type\.gke\.io/[^=,\s]+=[^,\s]*`)
	kubeEnvNodeLabelsEmptyOKRegex = regexp.MustCompile(`--node-labels=\S*`)
	registerWithTaintsRegex       = regexp.MustCompile(`(--register-with-taints=)(\S+)`)
	// kubeEnvNodeLabelsRegex matches the entire --node-labels=<value> flag in kube-env.
	// The value is comma-separated Kubernetes labels with no whitespace.
	kubeEnvNodeLabelsRegex    = regexp.MustCompile(`--node-labels=\S+`)
	osDistributionCOSRegex    = regexp.MustCompile(`\bgke-os-distribution=cos\b`)
	osDistributionUbuntuRegex = regexp.MustCompile(`\bgke-os-distribution=ubuntu\b`)
)

func GetClusterName(metadata *compute.Metadata) (string, error) {
	// Get cluster name
	clusterNameEntry := lo.Filter(metadata.Items, func(item *compute.MetadataItems, _ int) bool {
		return item.Key == ClusterNameLabel
	})
	if len(clusterNameEntry) != 1 {
		return "", errors.New("cluster name label not found")
	}
	clusterName := lo.FromPtr(clusterNameEntry[0].Value)
	if clusterName == "" {
		return "", errors.New("cluster name label is empty")
	}
	return clusterName, nil
}

func RenderKubeletConfigMetadata(metaData *compute.Metadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, capacityType string) error {
	targetEntry, index, ok := lo.FindIndexOf(metaData.Items, func(item *compute.MetadataItems) bool {
		return item.Key == KubeletConfigLabel
	})
	if !ok || index == -1 {
		return errors.New("kubelet-config metadata not found")
	}
	cpuMilliCore := fmt.Sprintf("%dm", instanceType.Overhead.KubeReserved.Cpu().MilliValue())
	memoryMB := fmt.Sprintf("%dMi", instanceType.Overhead.KubeReserved.Memory().Value()/(1024*1024))
	ephemeralStorage := instanceType.Overhead.KubeReserved.StorageEphemeral().String()

	configStr := lo.FromPtr(targetEntry.Value)
	if configStr == "" {
		return errors.New("kubelet-config metadata is empty")
	}

	// Parse YAML
	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
		return fmt.Errorf("failed to parse kubelet-config YAML: %w", err)
	}

	// Update kubeReserved with provider-computed defaults first; user values
	// in spec.kubeletConfiguration.kubeReserved overlay these per-sub-key
	// below via mergeUserKubeletConfig.
	kubeReserved, ok := config["kubeReserved"].(map[string]interface{})
	if !ok {
		kubeReserved = make(map[string]interface{})
	}
	kubeReserved["cpu"] = cpuMilliCore
	kubeReserved["memory"] = memoryMB
	kubeReserved["ephemeral-storage"] = ephemeralStorage
	config["kubeReserved"] = kubeReserved

	// Merge user-supplied KubeletConfiguration on top of the provider defaults.
	// Map fields (kubeReserved, systemReserved, eviction*) merge one level
	// deep so user sub-keys win but computed sub-keys for unset keys survive
	// (e.g. computed kubeReserved.ephemeral-storage from boot disk size).
	if err := mergeUserKubeletConfig(config, nodeClass.Spec.KubeletConfiguration); err != nil {
		return fmt.Errorf("failed to merge user KubeletConfiguration: %w", err)
	}

	// Spot-specific overrides are applied AFTER the user merge: graceful
	// node shutdown is required for Spot preemption handling and must not
	// be overridable from the nodeclass.
	if capacityType == karpv1.CapacityTypeSpot {
		featureGates, ok := config["featureGates"].(map[string]interface{})
		if !ok {
			featureGates = make(map[string]interface{})
		}
		featureGates["GracefulNodeShutdown"] = true
		config["featureGates"] = featureGates
		config["shutdownGracePeriod"] = "30s"
		config["shutdownGracePeriodCriticalPods"] = "15s"
	}

	// Marshal back to YAML
	updatedYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal updated kubelet-config YAML: %w", err)
	}

	targetEntry.Value = lo.ToPtr(string(updatedYAML))
	metaData.Items[index] = targetEntry

	return nil
}

// mergeUserKubeletConfig overlays user KubeletConfiguration onto config.
// Scalar/slice keys replace verbatim; map keys merge one level deep so
// provider-computed sub-keys (e.g. kubeReserved.ephemeral-storage from #220)
// survive a partial user override.
func mergeUserKubeletConfig(config map[string]interface{}, kc *v1alpha1.KubeletConfiguration) error {
	if kc == nil {
		return nil
	}
	raw, err := json.Marshal(kc)
	if err != nil {
		return fmt.Errorf("marshal KubeletConfiguration: %w", err)
	}
	userMap := map[string]interface{}{}
	if err := json.Unmarshal(raw, &userMap); err != nil {
		return fmt.Errorf("unmarshal KubeletConfiguration: %w", err)
	}
	for k, v := range userMap {
		if userSub, ok := v.(map[string]interface{}); ok {
			if existingSub, ok := config[k].(map[string]interface{}); ok {
				for sk, sv := range userSub {
					existingSub[sk] = sv
				}
				continue
			}
		}
		config[k] = v
	}
	return nil
}

// SetNodeLabels replaces all Kubernetes node-label bootstrap surfaces with the
// target node labels Karpenter/provider owns. It intentionally discards labels
// inherited from the GKE bootstrap source pool. kubeLabels and kubeEnvLabels are
// separate because lifecycle labels such as karpenter.sh/registered must not be
// present at kubelet registration time.
func SetNodeLabels(metadata *compute.Metadata, kubeLabels, kubeEnvLabels map[string]string) error {
	if metadata == nil {
		return fmt.Errorf("metadata must be non-nil")
	}
	kubeLabelString := formatLabels(kubeLabels)
	kubeEnvLabelString := formatLabels(kubeEnvLabels)
	kubeLabelsFound := false
	kubeEnvFound := false

	for _, item := range metadata.Items {
		switch item.Key {
		case "kube-labels":
			kubeLabelsFound = true
			item.Value = lo.ToPtr(kubeLabelString)
		case "kube-env":
			kubeEnvFound = true
			item.Value = lo.ToPtr(setNodeLabelsInKubeEnv(lo.FromPtr(item.Value), kubeEnvLabelString))
		}
	}
	if !kubeLabelsFound {
		return fmt.Errorf("kube-labels metadata key not found in instance template")
	}
	if !kubeEnvFound {
		return fmt.Errorf("kube-env metadata key not found in instance template")
	}
	return nil
}

func formatLabels(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key, value := range labels {
		if key != "" && value != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, labels[key]))
	}
	return strings.Join(parts, ",")
}

func setNodeLabelsInKubeEnv(kubeEnv, labels string) string {
	flag := "--node-labels=" + labels
	if kubeEnvNodeLabelsEmptyOKRegex.MatchString(kubeEnv) {
		return kubeEnvNodeLabelsEmptyOKRegex.ReplaceAllString(kubeEnv, flag)
	}
	return appendKubeletArg(kubeEnv, flag)
}

func upsertLabelString(labels, key, value string) string {
	parts := strings.Split(labels, ",")
	updated := make([]string, 0, len(parts)+1)
	prefix := key + "="
	inserted := false
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, prefix) {
			if !inserted {
				updated = append(updated, prefix+value)
				inserted = true
			}
			continue
		}
		updated = append(updated, part)
	}
	if !inserted {
		updated = append(updated, prefix+value)
	}
	return strings.Join(updated, ",")
}

func upsertNodeLabelInKubeEnv(kubeEnv, key, value string) string {
	if kubeEnvNodeLabelsEmptyOKRegex.MatchString(kubeEnv) {
		return kubeEnvNodeLabelsEmptyOKRegex.ReplaceAllStringFunc(kubeEnv, func(match string) string {
			labels := strings.TrimPrefix(match, "--node-labels=")
			return "--node-labels=" + upsertLabelString(labels, key, value)
		})
	}
	return appendKubeletArg(kubeEnv, "--node-labels="+key+"="+value)
}

// SetRegisterWithTaints replaces the kubelet register-with-taints flag with the
// target node taints Karpenter/provider owns. It intentionally discards taints
// inherited from the GKE bootstrap source pool.
func SetRegisterWithTaints(metadata *compute.Metadata, taints []string) error {
	if metadata == nil {
		return fmt.Errorf("metadata must be non-nil")
	}
	kubeEnvFound := false
	for _, item := range metadata.Items {
		if item.Key != "kube-env" {
			continue
		}
		kubeEnvFound = true
		item.Value = lo.ToPtr(setRegisterWithTaintsInKubeEnv(lo.FromPtr(item.Value), taints))
	}
	if !kubeEnvFound {
		return fmt.Errorf("kube-env metadata key not found in instance template")
	}
	return nil
}

func setRegisterWithTaintsInKubeEnv(kubeEnv string, taints []string) string {
	cleaned := compactStrings(taints)
	if len(cleaned) == 0 {
		return registerWithTaintsRegex.ReplaceAllString(kubeEnv, "")
	}
	flag := "--register-with-taints=" + strings.Join(cleaned, ",")
	if registerWithTaintsRegex.MatchString(kubeEnv) {
		return registerWithTaintsRegex.ReplaceAllString(kubeEnv, flag)
	}
	return appendKubeletArg(kubeEnv, flag)
}

func compactStrings(values []string) []string {
	ret := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		ret = append(ret, value)
	}
	return ret
}

func appendKubeletArg(kubeEnv, arg string) string {
	lines := strings.Split(kubeEnv, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "KUBELET_ARGS:") {
			lines[i] = line + " " + arg
			return strings.Join(lines, "\n")
		}
	}
	if strings.HasSuffix(kubeEnv, "\n") {
		return kubeEnv + "KUBELET_ARGS: " + arg + "\n"
	}
	if kubeEnv == "" {
		return "KUBELET_ARGS: " + arg + "\n"
	}
	return kubeEnv + "\nKUBELET_ARGS: " + arg + "\n"
}

func SetMaxPodsPerNode(metadata *compute.Metadata, nodeClass *v1alpha1.GCENodeClass) error {
	maxPods := nodeClass.GetMaxPods()
	maxPodsStr := fmt.Sprintf("max-pods=%d", maxPods)

	targetEntry, index, ok := lo.FindIndexOf(metadata.Items, func(item *compute.MetadataItems) bool {
		return item.Key == "kube-env"
	})
	if !ok || index == -1 {
		return fmt.Errorf("kube-env metadata not found")
	}
	targetEntry.Value = lo.ToPtr(maxPodsRegex.ReplaceAllString(*targetEntry.Value, maxPodsStr))
	metadata.Items[index] = targetEntry
	return nil
}

func SetProvisioningModel(metadata *compute.Metadata, model string) error {
	kubeLabelsFound := false
	kubeEnvFound := false
	for _, item := range metadata.Items {
		switch item.Key {
		case "kube-labels":
			kubeLabelsFound = true
			item.Value = lo.ToPtr(upsertLabelString(lo.FromPtr(item.Value), "gke-provisioning", model))
		case "kube-env":
			kubeEnvFound = true
			updated := gkeProvisioningRegex.ReplaceAllString(lo.FromPtr(item.Value), "gke-provisioning="+model)
			item.Value = lo.ToPtr(upsertNodeLabelInKubeEnv(updated, "gke-provisioning", model))
		}
	}
	if !kubeLabelsFound {
		return fmt.Errorf("kube-labels metadata not found")
	}
	if !kubeEnvFound {
		return fmt.Errorf("kube-env metadata not found")
	}
	return nil
}

func AppendGPUTaint(metadata *compute.Metadata) error {
	found := false
	for _, item := range metadata.Items {
		if item.Key == "kube-env" {
			kubeEnv := lo.FromPtr(item.Value)
			lines := strings.Split(kubeEnv, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "KUBELET_ARGS:") {
					found = true
					if strings.Contains(line, GPUTaintArg) {
						// already present, idempotent
						continue
					}
					if loc := registerWithTaintsRegex.FindStringIndex(line); loc != nil {
						// Append into the first --register-with-taints value only; using
						// FindStringIndex avoids ReplaceAll touching a second flag if the
						// template happens to carry one.
						lines[i] = line[:loc[1]] + "," + GPUTaintArg + line[loc[1]:]
					} else {
						lines[i] = line + " --register-with-taints=" + GPUTaintArg
					}
				}
			}
			item.Value = lo.ToPtr(strings.Join(lines, "\n"))
		}
	}
	if !found {
		return fmt.Errorf("KUBELET_ARGS not found in kube-env")
	}
	return nil
}

func PatchKubeEnvForInstanceType(metadata *compute.Metadata, instanceType *cloudprovider.InstanceType) error {
	if metadata == nil || instanceType == nil {
		return fmt.Errorf("metadata and instanceType must be non-nil")
	}

	arch, family := kubeEnvPatchTargets(instanceType)
	for _, item := range metadata.Items {
		switch item.Key {
		case "kube-labels":
			if family != "" {
				item.Value = lo.ToPtr(upsertLabelString(lo.FromPtr(item.Value), "cloud.google.com/machine-family", family))
			}
		case "kube-env":
			kubeEnv := lo.FromPtr(item.Value)
			if kubeEnv == "" {
				return fmt.Errorf("kube-env metadata is empty")
			}

			// Note: SERVER_BINARY_TAR_URL and SERVER_BINARY_TAR_HASH are left untouched.
			// They are set correctly per-architecture by GKE in the node pool template.
			// Patching the URL without the corresponding hash causes bootstrap failures.
			updated := patchKubeEnvKeyValue(kubeEnv, kubeEnvArchRegex, "arch="+arch)
			if family != "" {
				updated = patchKubeEnvKeyValue(updated, kubeEnvFamilyRegex, "cloud.google.com/machine-family="+family)
				updated = upsertNodeLabelInKubeEnv(updated, "cloud.google.com/machine-family", family)
			}

			if updated != kubeEnv {
				item.Value = lo.ToPtr(updated)
			}
		}
	}

	return nil
}

func kubeEnvPatchTargets(instanceType *cloudprovider.InstanceType) (arch string, family string) {
	arch = instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	if arch == "" {
		arch = "amd64"
	}
	family = instanceType.Requirements.Get(v1alpha1.LabelInstanceFamily).Any()
	return arch, family
}

func patchKubeEnvKeyValue(kubeEnv string, re *regexp.Regexp, replacement string) string {
	if !re.MatchString(kubeEnv) {
		return kubeEnv
	}
	return re.ReplaceAllString(kubeEnv, replacement)
}

func SetKubeEnvZone(metadata *compute.Metadata, zone string) error {
	if metadata == nil || zone == "" {
		return fmt.Errorf("metadata and zone must be non-empty")
	}
	for _, item := range metadata.Items {
		if item.Key != "kube-env" {
			continue
		}
		kubeEnv := lo.FromPtr(item.Value)
		if kubeEnv == "" {
			return fmt.Errorf("kube-env metadata is empty")
		}
		zoneLine := "ZONE: " + zone
		if regexp.MustCompile(`(?m)^ZONE: [^\n]+`).MatchString(kubeEnv) {
			item.Value = lo.ToPtr(patchKubeEnvKeyValue(kubeEnv, regexp.MustCompile(`(?m)^ZONE: [^\n]+`), zoneLine))
		} else {
			item.Value = lo.ToPtr(kubeEnv + "\n" + zoneLine + "\n")
		}
		return nil
	}
	return fmt.Errorf("kube-env metadata key not found in instance template")
}

func BaselineKubeLabels(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) map[string]string {
	labels := BaselineKubeEnvLabels(nodeClaim, nodeClass)
	labels[karpv1.NodeRegisteredLabelKey] = "true"
	return labels
}

func BaselineKubeEnvLabels(nodeClaim *karpv1.NodeClaim, nodeClass *v1alpha1.GCENodeClass) map[string]string {
	return map[string]string{
		"max-pods-per-node":                             fmt.Sprintf("%d", nodeClass.GetMaxPods()),
		"cloud.google.com/gke-os-distribution":          gkeOSDistribution(nodeClass),
		karpv1.NodePoolLabelKey:                         nodeClaim.Labels[karpv1.NodePoolLabelKey],
		v1alpha1.LabelNodeClass:                         nodeClass.Name,
		v1alpha1.LabelGKEReadinessMetadataServerEnabled: "true",
		v1alpha1.LabelGKEReadinessNetdReady:             "true",
		v1alpha1.LabelGKEReadinessNodeLocalDNSReady:     "true",
	}
}

func gkeOSDistribution(nodeClass *v1alpha1.GCENodeClass) string {
	if nodeClass.ImageFamily() == v1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}

func AppendSecondaryBootDisks(projectID string, nodeClass *v1alpha1.GCENodeClass, metadata *compute.Metadata) {
	for _, disk := range nodeClass.Spec.Disks {
		if disk.Boot || disk.SecondaryBootImage == "" || disk.SecondaryBootMode == "MODE_UNSPECIFIED" {
			continue
		}

		name := GetSecondaryDiskImageName(disk.SecondaryBootImage)
		deviceName := GetSecondaryDiskImageDeviceName(disk.SecondaryBootImage)
		for _, item := range metadata.Items {
			if item.Key == "kube-env" {
				// Add SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/DISK_IMAGE_NAME
				kubeEnv := lo.FromPtr(item.Value)
				lines := strings.Split(kubeEnv, "\n")
				lines = append(lines, fmt.Sprintf("SECONDARY_BOOT_DISKS: /mnt/disks/gke-secondary-disks/%s", deviceName))
				item.Value = lo.ToPtr(strings.Join(lines, "\n"))
			}
			if item.Key == "kube-labels" {
				item.Value = lo.ToPtr(*item.Value + "," + secondaryBootDiskLabel(name, projectID, disk.SecondaryBootMode))
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

// appendNodeLabelToKubeEnv appends (or replaces) a label in the --node-labels flag within
// kube-env. In GKE COS, kubelet reads node labels from --node-labels in KUBELET_ARGS of
// kube-env, not from the kube-labels metadata item. Modifying kube-labels alone is not
// sufficient for labels that must be present at kubelet startup (e.g. GPU driver version).
func appendNodeLabelToKubeEnv(metadata *compute.Metadata, labelKey, labelValue string) error {
	prefix := regexp.QuoteMeta(labelKey + "=")
	stripRe := regexp.MustCompile(`,?` + prefix + `[^,\s]*`)
	label := labelKey + "=" + labelValue

	for _, item := range metadata.Items {
		if item.Key != "kube-env" {
			continue
		}
		kubeEnv := lo.FromPtr(item.Value)
		if !strings.Contains(kubeEnv, "--node-labels=") {
			return fmt.Errorf("--node-labels flag not found in kube-env KUBELET_ARGS")
		}
		// Remove any existing entry with this label key (idempotent).
		// The strip regex eats the preceding comma; when the label is the first entry
		// in --node-labels= there is no preceding comma, so a trailing comma is left
		// behind. Clean that up explicitly.
		updated := stripRe.ReplaceAllString(kubeEnv, "")
		updated = strings.ReplaceAll(updated, "--node-labels=,", "--node-labels=")
		// Append new label to the --node-labels value.
		updated = kubeEnvNodeLabelsRegex.ReplaceAllStringFunc(updated, func(match string) string {
			return match + "," + label
		})
		item.Value = lo.ToPtr(updated)
		return nil
	}
	return fmt.Errorf("kube-env metadata key not found in instance template")
}

func SetDiskTypeLabels(metadata *compute.Metadata, labels map[string]string) error {
	if metadata == nil {
		return fmt.Errorf("metadata must be non-nil")
	}

	kubeLabelsFound := false
	kubeEnvFound := false
	for _, item := range metadata.Items {
		switch item.Key {
		case "kube-labels":
			kubeLabelsFound = true
			item.Value = lo.ToPtr(replaceDiskTypeLabelsInNodeLabels(lo.FromPtr(item.Value), labels))
		case "kube-env":
			kubeEnvFound = true
			updated, err := replaceDiskTypeLabelsInKubeEnv(lo.FromPtr(item.Value), labels)
			if err != nil {
				return err
			}
			item.Value = lo.ToPtr(updated)
		}
	}
	if !kubeLabelsFound {
		return fmt.Errorf("kube-labels metadata key not found in instance template")
	}
	if !kubeEnvFound {
		return fmt.Errorf("kube-env metadata key not found in instance template")
	}
	return nil
}

func replaceDiskTypeLabelsInKubeEnv(kubeEnv string, labels map[string]string) (string, error) {
	if !strings.Contains(kubeEnv, "--node-labels=") {
		return "", fmt.Errorf("--node-labels flag not found in kube-env KUBELET_ARGS")
	}
	stripped := diskTypeLabelEntryRegex.ReplaceAllString(kubeEnv, "")
	stripped = strings.ReplaceAll(stripped, "--node-labels=,", "--node-labels=")
	return kubeEnvNodeLabelsEmptyOKRegex.ReplaceAllStringFunc(stripped, func(match string) string {
		return "--node-labels=" + replaceDiskTypeLabelsInNodeLabels(strings.TrimPrefix(match, "--node-labels="), labels)
	}), nil
}

func replaceDiskTypeLabelsInNodeLabels(current string, labels map[string]string) string {
	parts := strings.Split(current, ",")
	filtered := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, _, _ := strings.Cut(part, "=")
		if diskTypeLabelRegex.MatchString(key) {
			continue
		}
		filtered = append(filtered, part)
	}

	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered = append(filtered, fmt.Sprintf("%s=%s", key, labels[key]))
	}
	return strings.Join(filtered, ",")
}

// SetGPUAcceleratorLabel sets cloud.google.com/gke-accelerator=<gpuName> in kube-labels and
// in the --node-labels flag of KUBELET_ARGS in kube-env (the canonical kubelet label source).
// The NVIDIA device plugin DaemonSet uses this label as a nodeAffinity selector; without it the
// plugin will not schedule onto Karpenter-provisioned GPU nodes.
// This always replaces any existing value so that the instance type's accelerator type is
// authoritative over the base template.
// Returns an error if the kube-labels or kube-env metadata keys are absent.
func SetGPUAcceleratorLabel(metadata *compute.Metadata, gpuName string) error {
	prefix := v1alpha1.LabelGKEAccelerator + "="
	label := prefix + gpuName
	kubeLabelsFound := false
	for _, item := range metadata.Items {
		if item.Key == "kube-labels" {
			kubeLabelsFound = true
			current := lo.FromPtr(item.Value)
			parts := strings.Split(current, ",")
			filtered := parts[:0]
			for _, p := range parts {
				if !strings.HasPrefix(p, prefix) {
					filtered = append(filtered, p)
				}
			}
			item.Value = lo.ToPtr(strings.Join(append(filtered, label), ","))
		}
	}
	if !kubeLabelsFound {
		return fmt.Errorf("kube-labels metadata key not found in instance template")
	}
	return appendNodeLabelToKubeEnv(metadata, v1alpha1.LabelGKEAccelerator, gpuName)
}

// SetGPUDriverVersionLabel sets cloud.google.com/gke-gpu-driver-version=<version> in kube-labels and
// in the --node-labels flag of KUBELET_ARGS in kube-env (the canonical kubelet label source).
// GKE's GPU driver installer DaemonSet reads this label to select which driver to install.
// This always replaces any existing value so that spec.gpuDriverVersion is authoritative over
// whatever the base instance template or spec.metadata may have set.
// Returns an error if the kube-labels or kube-env metadata keys are absent.
func SetGPUDriverVersionLabel(metadata *compute.Metadata, version string) error {
	prefix := v1alpha1.LabelGKEGPUDriverVersion + "="
	label := prefix + version
	kubeLabelsFound := false
	for _, item := range metadata.Items {
		if item.Key == "kube-labels" {
			kubeLabelsFound = true
			current := lo.FromPtr(item.Value)
			parts := strings.Split(current, ",")
			filtered := parts[:0]
			for _, p := range parts {
				if !strings.HasPrefix(p, prefix) {
					filtered = append(filtered, p)
				}
			}
			item.Value = lo.ToPtr(strings.Join(append(filtered, label), ","))
		}
	}
	if !kubeLabelsFound {
		return fmt.Errorf("kube-labels metadata key not found in instance template")
	}
	return appendNodeLabelToKubeEnv(metadata, v1alpha1.LabelGKEGPUDriverVersion, version)
}

// PatchKubeEnvForOSType patches the kube-env metadata item so a single bootstrap source pool can serve nodes of either OS family.
// Patching is bidirectional: Ubuntu→COS and COS→Ubuntu.
//
// Fields modified for Ubuntu target (source is COS):
//   - gke-os-distribution=cos → gke-os-distribution=ubuntu
//   - ENABLE_NODE_BFQ_IO_SCHEDULER line removed
//   - NODE_BFQ_IO_SCHEDULER_IO_WEIGHT line removed
//
// Fields modified for ContainerOptimizedOS target (source is Ubuntu):
//   - gke-os-distribution=ubuntu → gke-os-distribution=cos
//   - ENABLE_NODE_BFQ_IO_SCHEDULER: "true" added (if absent)
//   - NODE_BFQ_IO_SCHEDULER_IO_WEIGHT: "1200" added (if absent)
func PatchKubeEnvForOSType(metaData *compute.Metadata, imageFamily string) error {
	for _, item := range metaData.Items {
		if item.Key != "kube-env" {
			continue
		}
		kubeEnv := lo.FromPtr(item.Value)
		if kubeEnv == "" {
			return fmt.Errorf("kube-env metadata is empty")
		}

		var updated string
		switch imageFamily {
		case v1alpha1.ImageFamilyUbuntu:
			updated = osDistributionCOSRegex.ReplaceAllString(kubeEnv, "gke-os-distribution=ubuntu")
			updated = removeKubeEnvLine(updated, "ENABLE_NODE_BFQ_IO_SCHEDULER")
			updated = removeKubeEnvLine(updated, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT")
		case v1alpha1.ImageFamilyContainerOptimizedOS:
			updated = osDistributionUbuntuRegex.ReplaceAllString(kubeEnv, "gke-os-distribution=cos")
			updated = ensureKubeEnvLine(updated, "ENABLE_NODE_BFQ_IO_SCHEDULER", `"true"`)
			updated = ensureKubeEnvLine(updated, "NODE_BFQ_IO_SCHEDULER_IO_WEIGHT", `"1200"`)
		default:
			continue
		}
		item.Value = lo.ToPtr(updated)
	}

	return nil
}

// ensureKubeEnvLine adds "key: value" to the kube-env string if no line with that key
// already exists. This is used when patching from an Ubuntu source template to a COS node.
func ensureKubeEnvLine(kubeEnv, key, value string) string {
	for _, line := range strings.Split(kubeEnv, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), key+":") {
			return kubeEnv
		}
	}
	return kubeEnv + "\n" + key + ": " + value
}

// removeKubeEnvLine removes any line whose key (text before the first colon) matches
// the given key. This handles the kube-env YAML-style "KEY: value" line format.
func removeKubeEnvLine(kubeEnv, key string) string {
	lines := strings.Split(kubeEnv, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, key+":") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

// ApplyCustomMetadata applies custom metadata from GCENodeClass to the instance metadata.
// User-supplied keys override any value inherited from the base instance template;
// keys not already present are appended. Empty values are ignored, so a key cannot
// be used to clear a value inherited from the base template.
func ApplyCustomMetadata(metadata *compute.Metadata, customMetadata map[string]string) {
	if len(customMetadata) == 0 {
		return
	}

	for key, value := range customMetadata {
		if value == "" {
			continue
		}

		targetEntry, index, ok := lo.FindIndexOf(metadata.Items, func(item *compute.MetadataItems) bool {
			return item.Key == key
		})

		if ok && index != -1 {
			// Key exists, override the value
			targetEntry.Value = lo.ToPtr(value)
			metadata.Items[index] = targetEntry
			continue
		}
		// Key doesn't exist, create a new metadata item
		newItem := &compute.MetadataItems{
			Key:   key,
			Value: lo.ToPtr(value),
		}
		metadata.Items = append(metadata.Items, newItem)
	}
}
