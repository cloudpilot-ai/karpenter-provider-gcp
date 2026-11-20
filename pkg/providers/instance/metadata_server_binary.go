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

package instance

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/metadata"
	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/providers/imagefamily"
)

// launchImage keeps the selected boot image and its resolved bootstrap bundle
// together through disk and metadata construction.
type launchImage struct {
	SourceImage string
	Bootstrap   metadata.BootstrapRelease
}

func (p *DefaultProvider) resolveLaunchImage(ctx context.Context, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) (launchImage, error) {
	image, found := imagefamily.FindImage(nodeClass.Status.Images, instanceType.Requirements)
	if !found {
		return launchImage{}, fmt.Errorf("no target image found for node class %s", nodeClass.Name)
	}
	if image.KubernetesVersion == "" {
		return launchImage{}, fmt.Errorf("resolved image has no kubernetesVersion; update CRDs and wait for GCENodeClass image reconciliation")
	}
	arch := instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	if arch == "" {
		arch = "amd64"
	}
	release, err := p.bootstrapProvider.Get(ctx, image.KubernetesVersion, arch)
	if err != nil {
		return launchImage{}, fmt.Errorf("resolving bootstrap release %s/%s: %w", image.KubernetesVersion, arch, err)
	}
	return launchImage{SourceImage: image.SourceImage, Bootstrap: release}, nil
}
