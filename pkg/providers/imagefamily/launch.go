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

package imagefamily

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

// SelectLaunchImage returns the first requirements- and policy-compatible image
// in selector order without modifying status. Sandbox callers must first resolve
// the effective bootstrap profile and pass its Release. Candidate filtering and
// launch must use this same selection contract, not just requirement matching.
func SelectLaunchImage(nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, bootstrapRelease string) (v1alpha1.Image, error) {
	var firstError error
	for _, image := range nodeClass.Status.Images {
		if scheduling.NewNodeSelectorRequirements(image.Requirements...).Compatible(instanceType.Requirements, scheduling.AllowUndefinedWellKnownLabels) != nil {
			continue
		}
		if nodeClass.SandboxEnabled() {
			if err := sandboxImageCompatible(nodeClass, instanceType, image, bootstrapRelease); err != nil {
				if firstError == nil {
					firstError = err
				}
				continue
			}
		}
		return image, nil
	}
	if firstError != nil {
		return v1alpha1.Image{}, firstError
	}
	if nodeClass.SandboxEnabled() {
		return v1alpha1.Image{}, &sandbox.ConfigurationError{Reason: "no compatible sandbox launch image"}
	}
	return v1alpha1.Image{}, fmt.Errorf("no target image found for node class %s", nodeClass.Name)
}

func sandboxImageCompatible(nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, image v1alpha1.Image, bootstrapRelease string) error {
	if image.SandboxInstaller == nil {
		return &sandbox.ConfigurationError{Reason: "selected launch image has no resolved sandbox installer"}
	}
	if bootstrapRelease == "" {
		return &sandbox.ConfigurationError{Reason: "sandbox image selection requires the effective bootstrap release"}
	}
	release, err := SandboxImageRelease(image.SourceImage)
	if err != nil {
		return &sandbox.ConfigurationError{Reason: err.Error()}
	}
	return sandboxGPUCompatible(nodeClass, instanceType, release, bootstrapRelease)
}

func sandboxGPUCompatible(nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, imageRelease, bootstrapRelease string) error {
	req := instanceType.Requirements[v1alpha1.LabelInstanceGPUCount]
	if req != nil && req.Operator() == corev1.NodeSelectorOpIn && req.Len() > 0 {
		gpu := instanceType.Requirements.Get(v1alpha1.LabelInstanceGPUName)
		if gpu.Len() != 1 || gpu.Operator() != corev1.NodeSelectorOpIn {
			return &sandbox.ConfigurationError{Reason: "GPU launch requires one factual GPU model"}
		}
		quantity := instanceType.Capacity[v1alpha1.ResourceNVIDIAGPU]
		count, whole := quantity.AsInt64()
		if !whole {
			return &sandbox.ConfigurationError{Reason: "fractional GPUs are not supported"}
		}
		if err := sandbox.GPUCompatible(gpu.Any(), nodeClass.Spec.GPUDriverVersion, count, nodeClass.Spec.GPUSharingConfig != nil, imageRelease, bootstrapRelease); err != nil {
			return err
		}
	}
	return nil
}
