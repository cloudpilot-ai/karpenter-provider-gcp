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
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const kubeletMaxPodsFlagName = "--max-pods"

// ParseSourceKubeletArgs preserves only unknown kubelet args from a GKE source
// template. Bootstrap labels, taints, and max pods are target-owned and should
// be set explicitly by provider orchestration before rendering.
func ParseSourceKubeletArgs(raw string) KubeletArgs {
	args := KubeletArgs{nodeLabels: Labels{}}
	for _, field := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(field, kubeletMaxPodsFlagName+"="),
			strings.HasPrefix(field, "--node-labels="),
			strings.HasPrefix(field, "--register-with-taints="):
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

func (m *InstanceMetadata) SetRegistrationNodeLabels(labels Labels) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.nodeLabels = maps.Clone(labels)
}

func (m *InstanceMetadata) SetRegistrationNodeLabel(key, value string) {
	if m == nil || key == "" {
		return
	}
	if m.kubeEnv.kubeletArgs.nodeLabels == nil {
		m.kubeEnv.kubeletArgs.nodeLabels = Labels{}
	}
	m.kubeEnv.kubeletArgs.nodeLabels[key] = value
}

func (m *InstanceMetadata) SetKubeletMaxPods(maxPods int32) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.maxPods = &maxPods
}

// SetNodeTaints replaces the node taints this metadata model will render when
// the instance registers as a Kubernetes node.
func (m *InstanceMetadata) SetNodeTaints(taints Taints) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.SetNodeTaints(taints)
}

func (m *InstanceMetadata) AppendNodeTaint(taint corev1.Taint) {
	if m == nil {
		return
	}
	m.kubeEnv.kubeletArgs.AppendNodeTaint(taint)
}

func (a *KubeletArgs) SetNodeTaints(taints Taints) {
	if a == nil {
		return
	}
	a.nodeTaints = slices.Clone(taints)
}

func (a *KubeletArgs) AppendNodeTaint(taint corev1.Taint) {
	if a == nil {
		return
	}
	a.nodeTaints = append(a.nodeTaints, taint)
}

func (t Taints) String() string {
	parts := make([]string, 0, len(t))
	for _, taint := range t {
		parts = append(parts, taint.ToString())
	}
	return strings.Join(parts, ",")
}
