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
	"sort"
	"strings"

	"github.com/samber/lo"
)

// KubeletArgs is a targeted editor for the GKE-generated KUBELET_ARGS line,
// not a complete shell parser. It normalizes only flags owned by this provider
// and preserves all other whitespace-delimited tokens best-effort in Other.
type KubeletArgs struct {
	Other              []string
	MaxPods            string
	NodeLabels         KubeLabels
	RegisterWithTaints []string
}

func ParseKubeletArgs(raw string) *KubeletArgs {
	args := &KubeletArgs{NodeLabels: KubeLabels{}}
	for _, field := range strings.Fields(raw) {
		switch {
		case strings.HasPrefix(field, KubeletMaxPodsFlagName+"="):
			args.MaxPods = strings.TrimPrefix(field, KubeletMaxPodsFlagName+"=")
		case strings.HasPrefix(field, "--node-labels="):
			args.NodeLabels.Merge(ParseKubeLabels(strings.TrimPrefix(field, "--node-labels=")))
		case strings.HasPrefix(field, "--register-with-taints="):
			args.AddRegisterWithTaints(strings.Split(strings.TrimPrefix(field, "--register-with-taints="), ",")...)
		default:
			args.Other = append(args.Other, field)
		}
	}
	return args
}

func (a *KubeletArgs) AddRegisterWithTaints(taints ...string) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a.RegisterWithTaints)+len(taints))
	for _, taint := range append(a.RegisterWithTaints, taints...) {
		taint = strings.TrimSpace(taint)
		if taint == "" {
			continue
		}
		if _, ok := seen[taint]; ok {
			continue
		}
		seen[taint] = struct{}{}
		out = append(out, taint)
	}
	a.RegisterWithTaints = out
}

func (a *KubeletArgs) String() string {
	parts := append([]string{}, a.Other...)
	if a.MaxPods != "" {
		parts = append(parts, KubeletMaxPodsFlagName+"="+a.MaxPods)
	}
	if labels := a.NodeLabels.String(); labels != "" {
		parts = append(parts, "--node-labels="+labels)
	}
	if len(a.RegisterWithTaints) > 0 {
		taints := append([]string{}, a.RegisterWithTaints...)
		sort.Strings(taints)
		parts = append(parts, "--register-with-taints="+strings.Join(taints, ","))
	}
	return strings.Join(parts, " ")
}

func PatchKubeletArgsLine(kubeEnv string, patch func(args *KubeletArgs)) (string, error) {
	lines := strings.Split(kubeEnv, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "KUBELET_ARGS:") {
			continue
		}

		rawParts := []string{strings.TrimSpace(strings.TrimPrefix(line, "KUBELET_ARGS:"))}
		end := i + 1
		for end < len(lines) && isKubeletArgsContinuationLine(lines[end]) {
			rawParts = append(rawParts, strings.TrimSpace(lines[end]))
			end++
		}

		args := ParseKubeletArgs(strings.Join(rawParts, " "))
		patch(args)
		lines[i] = "KUBELET_ARGS: " + args.String()
		lines = append(lines[:i+1], lines[end:]...)
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("KUBELET_ARGS not found in kube-env")
}

func isKubeletArgsContinuationLine(line string) bool {
	return len(line) > 0 && strings.TrimLeft(line, " \t") != line && strings.HasPrefix(strings.TrimSpace(line), "--")
}

func PatchKubeEnvTaints(values MetadataValues, taints []string) error {
	requested := lo.FilterMap(taints, func(taint string, _ int) (string, bool) {
		taint = strings.TrimSpace(taint)
		return taint, taint != ""
	})
	if len(requested) == 0 {
		return nil
	}

	return editKubeletArgs(values, func(args *KubeletArgs) {
		args.AddRegisterWithTaints(requested...)
	})
}

func SetKubeEnvTaints(values MetadataValues, taints []string) error {
	requested := lo.FilterMap(taints, func(taint string, _ int) (string, bool) {
		taint = strings.TrimSpace(taint)
		return taint, taint != ""
	})
	return editKubeletArgs(values, func(args *KubeletArgs) {
		args.RegisterWithTaints = nil
		args.AddRegisterWithTaints(requested...)
	})
}

func AppendGPUTaint(values MetadataValues) error {
	return PatchKubeEnvTaints(values, []string{GPUTaintArg})
}
