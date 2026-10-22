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

package sandbox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
)

// ConfigurationError represents an unsupported target configuration, never a
// capacity shortage. Callers must not feed it to unavailable-offerings caches.
type ConfigurationError struct{ Reason string }

func (e *ConfigurationError) Error() string { return "SandboxConfigurationUnsupported: " + e.Reason }

// HardwarePolicy is independently maintained native policy, not discovered by
// hashing bootstrap consumers. The revision is included in BootstrapProfile.
type HardwarePolicy struct {
	Variant  string
	CoreTags bool
}

func ResolveHardwarePolicy(machineType, arch string) (HardwarePolicy, error) {
	if arch == "arm64" {
		return HardwarePolicy{Variant: "arm64"}, nil
	}
	if arch != "amd64" || machineType == "" {
		return HardwarePolicy{}, &ConfigurationError{Reason: "sandbox requires a known machine type and amd64 or arm64 architecture"}
	}
	family, _, _ := strings.Cut(machineType, "-")
	// Documented AMD-only series (Compute Engine machine-resource catalog); prefixes also cover their custom variants.
	// Intel and mixed-vendor x86 series use conservative core scheduling, including
	// E2 and legacy custom/N1. Do not limit support to experimentally sampled CPUs.
	switch family {
	case "n2d", "n4d", "c2d", "c3d", "c4d", "t2d", "h4d", "z4d", "g4":
		return HardwarePolicy{Variant: "amd64-amd"}, nil
	default:
		return HardwarePolicy{Variant: "amd64-core-tags", CoreTags: true}, nil
	}
}

var gkeVersion = regexp.MustCompile(`^v?1[.]([0-9]+)[.]([0-9]+)-gke[.]([0-9]+)$`)

func versionParts(version string) ([3]int, bool) {
	var parts [3]int
	m := gkeVersion.FindStringSubmatch(version)
	if m == nil {
		return parts, false
	}
	for i := range parts {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// GPUCompatible checks BOTH the selected image and effective bootstrap release.
// Model floors follow https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods#gpu_model_support.
// Only built-in whole GPUs and default/latest drivers are in this contract.
func GPUCompatible(model, driver string, count int64, sharing bool, imageRelease, bootstrapRelease string) error {
	if sharing {
		return &ConfigurationError{Reason: "GPU time-sharing is not supported"}
	}
	if driver != "" && driver != "default" && driver != "latest" {
		return &ConfigurationError{Reason: "sandbox GPUs require default or latest drivers"}
	}
	if count < 1 {
		return &ConfigurationError{Reason: "fractional GPUs are not supported"}
	}
	floor, err := gpuModelFloor(model)
	if err != nil {
		return err
	}
	for _, release := range []string{imageRelease, bootstrapRelease} {
		if err := compatibleGPURelease(floor, release); err != nil {
			return err
		}
	}
	return nil
}

func gpuModelFloor(model string) ([3]int, error) {
	switch model {
	case "nvidia-tesla-t4", "nvidia-l4", "nvidia-tesla-a100", "nvidia-a100-80gb", "nvidia-h100-80gb", "nvidia-h100-mega-80gb":
		return [3]int{29, 15, 1134000}, nil
	case "nvidia-h200-141gb", "nvidia-b200", "nvidia-gb200":
		return [3]int{34, 0, 1713000}, nil
	case "nvidia-rtx-pro-6000":
		return [3]int{34, 1, 2037001}, nil
	default:
		return [3]int{}, &ConfigurationError{Reason: fmt.Sprintf("GPU model %q is not supported by GKE Sandbox", model)}
	}
}

func compatibleGPURelease(floor [3]int, release string) error {
	v, ok := versionParts(release)
	if !ok {
		return &ConfigurationError{Reason: "GPU compatibility requires exact selected-image and effective bootstrap GKE releases"}
	}
	required := floor
	if floor[0] == 29 {
		switch v[0] {
		case 30:
			required = [3]int{30, 11, 1093000}
		case 31:
			required = [3]int{31, 7, 1149000}
		case 32:
			required = [3]int{32, 2, 1182003}
		}
	}
	if lessVersion(v, required) {
		return &ConfigurationError{Reason: fmt.Sprintf("GPU model is unsupported at GKE release %s", release)}
	}
	return nil
}

func lessVersion(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func ValidateConfiguration(nodeClass *v1alpha1.GCENodeClass) error {
	for _, key := range []string{metadata.SandboxRuntimeMetadataKey, metadata.SandboxInstallerMetadataKey, metadata.SandboxProfileMetadataKey, metadata.SandboxClassUIDMetadataKey, metadata.SandboxClassHashMetadataKey, metadata.SandboxPoolUIDMetadataKey} {
		if _, ok := nodeClass.Spec.Metadata[key]; ok {
			return &ConfigurationError{Reason: fmt.Sprintf("metadata key %s is provider-owned", key)}
		}
	}
	if _, ok := nodeClass.Spec.Labels[metadata.SandboxRuntimeInstanceLabel]; ok {
		return &ConfigurationError{Reason: "sandbox runtime instance label is provider-owned"}
	}
	if err := nodeClass.ValidateSandbox(); err != nil {
		return &ConfigurationError{Reason: err.Error()}
	}
	return nil
}
