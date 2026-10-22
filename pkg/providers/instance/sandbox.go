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

	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

func (p *DefaultProvider) prepareSandbox(ctx context.Context, target *metadata.InstanceMetadata, nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType) (*v1alpha1.Image, error) {
	if !nodeClass.SandboxEnabled() {
		if err := target.ApplySandbox(nil); err != nil {
			return nil, &sandbox.ConfigurationError{Reason: err.Error()}
		}
		return nil, nil
	}
	manifests, _ := target.GetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL")
	server, _ := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	resolver := p.bootstrapResolver
	if resolver == nil {
		resolver = sandbox.SharedResolver()
	}
	profile, err := resolver.ResolveBootstrap(ctx, manifests, server)
	if err != nil {
		return nil, err
	}
	arch := instanceType.Requirements.Get(corev1.LabelArchStable).Any()
	hardware, err := sandbox.ResolveHardwarePolicy(instanceType.Name, arch)
	if err != nil {
		return nil, err
	}
	image, err := imagefamily.SelectLaunchImage(nodeClass, instanceType, profile.Release)
	if err != nil {
		return nil, err
	}
	if err := target.ApplySandbox(&metadata.SandboxConfig{Version: image.SandboxInstaller.Version, Digest: image.SandboxInstaller.Digest, CoreTags: hardware.CoreTags}); err != nil {
		return nil, &sandbox.ConfigurationError{Reason: err.Error()}
	}
	identity, err := sandbox.LaunchIdentity(nodeClass, *image.SandboxInstaller, profile, instanceType.Name, arch)
	if err != nil {
		return nil, err
	}
	target.SetSandboxIdentity(identity)
	return &image, nil
}

func sandboxIdentityFromVM(vm *compute.Instance) *metadata.SandboxIdentity {
	identity := &metadata.SandboxIdentity{}
	fields := map[string]*string{
		metadata.SandboxRuntimeMetadataKey:   &identity.Runtime,
		metadata.SandboxInstallerMetadataKey: &identity.Installer,
		metadata.SandboxProfileMetadataKey:   &identity.Profile,
		metadata.SandboxClassUIDMetadataKey:  &identity.ClassUID,
		metadata.SandboxClassHashMetadataKey: &identity.ClassHash,
		metadata.SandboxPoolUIDMetadataKey:   &identity.PoolUID,
	}
	seen := map[string]bool{}
	if vm.Metadata != nil {
		for _, item := range vm.Metadata.Items {
			if item == nil {
				continue
			}
			field, owned := fields[item.Key]
			if !owned {
				continue
			}
			if seen[item.Key] || item.Value == nil {
				identity.Malformed = true
			}
			seen[item.Key] = true
			if item.Value != nil {
				*field = *item.Value
			}
			if *field == "" {
				identity.Malformed = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	return identity
}

func bootImageSource(nodeClass *v1alpha1.GCENodeClass, instanceType *cloudprovider.InstanceType, selectedImage *v1alpha1.Image) (string, error) {
	if selectedImage != nil {
		return selectedImage.SourceImage, nil
	}
	if nodeClass.SandboxEnabled() {
		return "", &sandbox.ConfigurationError{Reason: "sandbox disks require a prepared launch image"}
	}
	image, err := imagefamily.SelectLaunchImage(nodeClass, instanceType, "")
	return image.SourceImage, err
}
