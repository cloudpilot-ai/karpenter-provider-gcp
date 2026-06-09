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
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

type KubeLabels map[string]string

func ParseKubeLabels(raw string) KubeLabels {
	labels := KubeLabels{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		key, value, _ := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		labels[key] = strings.TrimSpace(value)
	}
	return labels
}

func (l KubeLabels) Get(key string) string {
	return l[key]
}

func (l KubeLabels) Set(key, value string) {
	if key == "" || l == nil {
		return
	}
	l[key] = value
}

func (l KubeLabels) Merge(values map[string]string) {
	for key, value := range values {
		l.Set(key, value)
	}
}

func (l KubeLabels) Delete(keys ...string) {
	for _, key := range keys {
		delete(l, key)
	}
}

func (l KubeLabels) String() string {
	keys := make([]string, 0, len(l))
	for key := range l {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+l[key])
	}
	return strings.Join(parts, ",")
}

const diskTypeLabelPrefix = "disk-type.gke.io/"

// SetNodeLabels writes provider-owned bootstrap node labels to both GKE metadata
// surfaces: kube-labels for template consistency and kube-env KUBELET_ARGS
// --node-labels for kubelet registration. It intentionally does not mirror the
// full label sets between the two surfaces or promote post-registration labels
// into bootstrap metadata.
func SetNodeLabels(values MetadataValues, labels map[string]string) error {
	original := cloneMetadataValues(values)
	if err := editKubeLabels(values, func(existing KubeLabels) {
		existing.Merge(labels)
	}); err != nil {
		return err
	}
	if err := editKubeEnvNodeLabels(values, func(existing KubeLabels) {
		existing.Merge(labels)
	}); err != nil {
		for k := range values {
			delete(values, k)
		}
		for k, v := range original {
			values[k] = v
		}
		return err
	}
	return nil
}

func editKubeLabels(values MetadataValues, patch func(labels KubeLabels)) error {
	if values == nil {
		return fmt.Errorf("%s metadata key not found in instance template", KubeLabelsKey)
	}
	current, ok := values[KubeLabelsKey]
	if !ok {
		return fmt.Errorf("%s metadata key not found in instance template", KubeLabelsKey)
	}
	labels := ParseKubeLabels(current)
	patch(labels)
	values[KubeLabelsKey] = labels.String()
	return nil
}

func editKubeEnv(values MetadataValues, patch func(string) (string, error)) error {
	if values == nil {
		return fmt.Errorf("%s metadata not found", KubeEnvKey)
	}
	current, ok := values[KubeEnvKey]
	if !ok {
		return fmt.Errorf("%s metadata not found", KubeEnvKey)
	}
	updated, err := patch(current)
	if err != nil {
		return err
	}
	values[KubeEnvKey] = updated
	return nil
}

func editKubeletArgs(values MetadataValues, patch func(args *KubeletArgs)) error {
	return editKubeEnv(values, func(kubeEnv string) (string, error) {
		return PatchKubeletArgsLine(kubeEnv, patch)
	})
}

func editKubeEnvNodeLabels(values MetadataValues, patch func(labels KubeLabels)) error {
	return editKubeEnv(values, func(kubeEnv string) (string, error) {
		return patchExistingKubeEnvNodeLabels(kubeEnv, patch)
	})
}

func cloneMetadataValues(values MetadataValues) MetadataValues {
	if values == nil {
		return nil
	}
	out := make(MetadataValues, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func patchExistingKubeEnvNodeLabels(kubeEnv string, patch func(labels KubeLabels)) (string, error) {
	foundKubeletArgs, foundNodeLabels := kubeEnvHasNodeLabelsFlag(kubeEnv)
	if foundKubeletArgs && !foundNodeLabels {
		// Do not synthesize --node-labels when GKE's template did not include it;
		// all provider-owned node label edits must update an existing kubelet
		// registration surface rather than inventing a new one.
		return "", fmt.Errorf("--node-labels flag not found in kube-env KUBELET_ARGS")
	}
	return PatchKubeletArgsLine(kubeEnv, func(args *KubeletArgs) {
		patch(args.NodeLabels)
	})
}

func kubeEnvHasNodeLabelsFlag(kubeEnv string) (bool, bool) {
	lines := strings.Split(kubeEnv, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "KUBELET_ARGS:") {
			continue
		}
		rawParts := []string{strings.TrimSpace(strings.TrimPrefix(line, "KUBELET_ARGS:"))}
		for end := i + 1; end < len(lines) && isKubeletArgsContinuationLine(lines[end]); end++ {
			rawParts = append(rawParts, strings.TrimSpace(lines[end]))
		}
		for _, field := range strings.Fields(strings.Join(rawParts, " ")) {
			if strings.HasPrefix(field, "--node-labels=") {
				return true, true
			}
		}
		return true, false
	}
	return false, false
}

func ReplaceNodeLabels(values MetadataValues, labels map[string]string, shouldReplace func(string) bool) error {
	return SetNodeLabelsWithMutation(values, func(nodeLabels KubeLabels) {
		for key := range nodeLabels {
			if shouldReplace(key) {
				nodeLabels.Delete(key)
			}
		}
		nodeLabels.Merge(labels)
	})
}

func SetNodeLabelsWithMutation(values MetadataValues, patch func(KubeLabels)) error {
	original := cloneMetadataValues(values)
	if err := editKubeLabels(values, patch); err != nil {
		return err
	}
	if err := editKubeEnvNodeLabels(values, patch); err != nil {
		for k := range values {
			delete(values, k)
		}
		for k, v := range original {
			values[k] = v
		}
		return err
	}
	return nil
}

func ReplaceNodeLabelsForSurfaces(values MetadataValues, kubeLabels, kubeEnvLabels map[string]string) error {
	if values == nil {
		return fmt.Errorf("metadata must be non-nil")
	}
	if _, ok := values[KubeLabelsKey]; !ok {
		return fmt.Errorf("%s metadata key not found in instance template", KubeLabelsKey)
	}
	if _, ok := values[KubeEnvKey]; !ok {
		return fmt.Errorf("%s metadata not found", KubeEnvKey)
	}

	originalKubeLabels := values[KubeLabelsKey]
	originalKubeEnv := values[KubeEnvKey]
	values[KubeLabelsKey] = KubeLabels(kubeLabels).String()
	if err := editKubeEnvNodeLabels(values, func(existing KubeLabels) {
		for key := range existing {
			delete(existing, key)
		}
		existing.Merge(kubeEnvLabels)
	}); err != nil {
		values[KubeLabelsKey] = originalKubeLabels
		values[KubeEnvKey] = originalKubeEnv
		return err
	}
	return nil
}

func RemoveGKEBuiltinLabels(values MetadataValues) error {
	if _, ok := values[KubeLabelsKey]; ok {
		if err := editKubeLabels(values, func(labels KubeLabels) {
			labels.Delete(GKENodePoolLabel)
		}); err != nil {
			return err
		}
	}
	if _, ok := values[KubeEnvKey]; ok {
		if err := editKubeEnv(values, func(kubeEnv string) (string, error) {
			kubeEnv = removeLabelAssignment(kubeEnv, GKENodePoolLabel)
			if foundKubeletArgs, _ := kubeEnvHasNodeLabelsFlag(kubeEnv); !foundKubeletArgs {
				return kubeEnv, nil
			}
			return PatchKubeletArgsLine(kubeEnv, func(args *KubeletArgs) {
				args.NodeLabels.Delete(GKENodePoolLabel)
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

func removeLabelAssignment(raw, key string) string {
	if key == "" {
		return raw
	}
	pattern := regexp.MustCompile(`(^|[,; \t\n])` + regexp.QuoteMeta(key) + `=[^,; \t\n]*[,;]?`)
	return pattern.ReplaceAllString(raw, "$1")
}

// SetMaxPods updates every GKE metadata surface that carries max-pods.
// kube-labels and KUBELET_ARGS --node-labels keep template readers consistent;
// KUBELET_ARGS --max-pods is the kubelet input that controls node capacity.
func SetMaxPods(values MetadataValues, maxPods int32) error {
	maxPodsValue := fmt.Sprintf("%d", maxPods)
	labels := map[string]string{
		MaxPodsPerNodeLabel: maxPodsValue,
		MaxPodsLabel:        maxPodsValue,
	}
	if err := SetNodeLabels(values, labels); err != nil {
		return err
	}
	if err := editKubeletArgs(values, func(args *KubeletArgs) {
		args.MaxPods = maxPodsValue
	}); err != nil {
		return err
	}
	return editKubeEnv(values, func(kubeEnv string) (string, error) {
		return replaceKubeEnvLabelAssignments(kubeEnv, labels), nil
	})
}

func SetProvisioningModel(values MetadataValues, model string) error {
	labels := map[string]string{GKEProvisioningLabel: model}
	if err := SetNodeLabels(values, labels); err != nil {
		return err
	}
	return editKubeEnv(values, func(kubeEnv string) (string, error) {
		return replaceKubeEnvLabelAssignments(kubeEnv, labels), nil
	})
}

func PatchKubeEnvForInstanceType(values MetadataValues, instanceType *cloudprovider.InstanceType) error {
	if values == nil || instanceType == nil {
		return fmt.Errorf("metadata and instanceType must be non-nil")
	}

	arch, family := kubeEnvPatchTargets(instanceType)
	labels := map[string]string{"arch": arch}
	if family != "" {
		labels["cloud.google.com/machine-family"] = family
	}

	if family != "" {
		if _, ok := values[KubeLabelsKey]; ok {
			if err := editKubeLabels(values, func(existing KubeLabels) {
				existing.Set("cloud.google.com/machine-family", family)
			}); err != nil {
				return err
			}
		}
	}

	if _, ok := values[KubeEnvKey]; !ok {
		return nil
	}
	return editKubeEnv(values, func(kubeEnv string) (string, error) {
		if kubeEnv == "" {
			return "", fmt.Errorf("kube-env metadata is empty")
		}
		// Note: SERVER_BINARY_TAR_URL and SERVER_BINARY_TAR_HASH are left untouched.
		// They are set correctly per-architecture by GKE in the node pool template.
		// Patching the URL without the corresponding hash causes bootstrap failures.
		updated := replaceKubeEnvLabelAssignments(kubeEnv, labels)
		if family == "" {
			return updated, nil
		}
		if _, hasNodeLabels := kubeEnvHasNodeLabelsFlag(updated); !hasNodeLabels {
			return updated, nil
		}
		return patchExistingKubeEnvNodeLabels(updated, func(existing KubeLabels) {
			existing.Set("cloud.google.com/machine-family", family)
		})
	})
}

func kubeEnvPatchTargets(instanceType *cloudprovider.InstanceType) (arch string, family string) {
	arch = instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	if arch == "" {
		arch = "amd64"
	}
	family = instanceType.Requirements.Get(v1alpha1.LabelInstanceFamily).Any()
	return arch, family
}
