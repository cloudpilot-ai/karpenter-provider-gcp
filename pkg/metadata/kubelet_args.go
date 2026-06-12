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

package metadata

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const kubeletMaxPodsFlagName = "--max-pods"

// ParseKubeletArgs parses KUBELET_ARGS from the GKE source template so unknown
// kubelet args can be preserved while provider-owned args are replaced.
func ParseKubeletArgs(raw string) KubeletArgs {
	args := KubeletArgs{nodeLabels: Labels{}}
	for _, field := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(field, kubeletMaxPodsFlagName+"="):
			value := strings.TrimPrefix(field, kubeletMaxPodsFlagName+"=")
			if value == "" {
				continue
			}
			maxPods, err := strconv.ParseInt(value, 10, 32)
			if err != nil {
				args.other = append(args.other, field)
				continue
			}
			v := int32(maxPods)
			args.maxPods = &v
		case strings.HasPrefix(field, "--node-labels="):
			labels := strings.TrimPrefix(field, "--node-labels=")
			if labels == "" {
				continue
			}
			mergeLabels(args.nodeLabels, ParseLabels(labels))
		case strings.HasPrefix(field, "--register-with-taints="):
			// Karpenter owns register-with-taints, so source-template taints are
			// intentionally discarded instead of preserved or parsed.
			continue
		default:
			args.other = append(args.other, field)
		}
	}
	return args
}

func (a KubeletArgs) String() string {
	parts := append([]string{}, a.other...)
	if a.maxPods != nil {
		parts = append(parts, fmt.Sprintf("%s=%d", kubeletMaxPodsFlagName, *a.maxPods))
	}
	if labels := a.nodeLabels.String(); labels != "" {
		parts = append(parts, "--node-labels="+labels)
	}
	if taints := a.nodeTaints.String(); taints != "" {
		parts = append(parts, "--register-with-taints="+taints)
	}
	return strings.Join(parts, " ")
}

// SetNodeTaints replaces the node taints this metadata model will apply when
// the instance registers as a Kubernetes node. The storage/rendering mechanism
// is intentionally hidden from callers.
func (m *InstanceMetadata) SetNodeTaints(taints ...corev1.Taint) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.SetNodeTaints(taints...)
}

// AddNodeTaints adds node taints this metadata model will apply when the
// instance registers as a Kubernetes node. Existing key/effect pairs win.
func (m *InstanceMetadata) AddNodeTaints(taints ...corev1.Taint) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.AddNodeTaints(taints...)
}

func (a *KubeletArgs) SetNodeTaints(taints ...corev1.Taint) {
	if a == nil {
		return
	}
	a.nodeTaints = nil
	a.AddNodeTaints(taints...)
}

func (a *KubeletArgs) AddNodeTaints(taints ...corev1.Taint) {
	if a == nil {
		return
	}
	a.nodeTaints = mergeNodeTaints(a.nodeTaints, taints)
}

func mergeNodeTaints(existing Taints, added []corev1.Taint) Taints {
	out := make(Taints, 0, len(existing)+len(added))
	for _, taint := range append(append([]corev1.Taint{}, existing...), added...) {
		if taint.Key == "" || taint.Effect == "" {
			continue
		}
		if containsNodeTaint(out, taint) {
			continue
		}
		out = append(out, taint)
	}
	return out
}

func containsNodeTaint(taints []corev1.Taint, taint corev1.Taint) bool {
	for i := range taints {
		if taint.MatchTaint(&taints[i]) {
			return true
		}
	}
	return false
}

func (t Taints) String() string {
	parts := make([]string, 0, len(t))
	for _, taint := range mergeNodeTaints(nil, t) {
		parts = append(parts, taint.ToString())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
