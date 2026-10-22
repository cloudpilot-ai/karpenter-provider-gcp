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
	"errors"
	"regexp"
	"strings"

	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

// GCE instance reads carry a disk resource, not InitializeParams. Resolve only
// the sandbox boot disk, never arbitrary metadata URLs or cross-project disks.
var bootDiskResource = regexp.MustCompile(`^(?:https://(?:www[.]googleapis[.]com|compute[.]googleapis[.]com)/compute/v1/)?projects/([a-z0-9-]+)/zones/([a-z0-9-]+)/disks/([a-z0-9-]+)$`)

func (p *DefaultProvider) sandboxBootImage(ctx context.Context, vm *compute.Instance, zone string) (string, error) {
	boot, err := sandboxBootDisk(vm)
	if err != nil {
		return "", err
	}

	if boot.InitializeParams != nil && boot.InitializeParams.SourceImage != "" {
		return boot.InitializeParams.SourceImage, nil
	}
	parts := bootDiskResource.FindStringSubmatch(boot.Source)
	if len(parts) != 4 || parts[1] != p.projectID || parts[2] != zone || !strings.HasPrefix(zone, p.region+"-") {
		return "", &sandbox.ConfigurationError{Reason: "VM boot disk is not in the provider project and zone"}
	}
	disk, err := p.computeService.Disks.Get(p.projectID, zone, parts[3]).Context(ctx).Do()
	if err != nil {
		return "", cloudprovider.NewCreateError(errors.New("VM boot disk image lookup failed"), "SandboxBootImageResolutionFailed", "Unable to resolve VM boot image")
	}
	if disk.SourceImage == "" {
		return "", &sandbox.ConfigurationError{Reason: "VM boot disk has no source image"}
	}
	// Keep the VM response shape usable by the existing factual mapping.
	boot.InitializeParams = &compute.AttachedDiskInitializeParams{SourceImage: disk.SourceImage}
	return disk.SourceImage, nil
}

func (p *DefaultProvider) validateAdoption(ctx context.Context, vm *compute.Instance, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim, types []*cloudprovider.InstanceType, source *compute.Metadata) error {
	enabled := class != nil && class.SandboxEnabled()
	actual := sandboxIdentityFromVM(vm)
	if err := sandbox.ValidateIdentityMode(actual, vm.Labels, enabled); err != nil {
		return err
	}
	if err := validateAdoptionOwnership(vm, class, claim, enabled); err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	if err := sandbox.ValidateLaunchBinding(actual, class, claim); err != nil {
		return err
	}
	if err := sandbox.ValidateConfiguration(class); err != nil {
		return err
	}
	it, err := p.resolveAdoptionType(ctx, vm, class, types)
	if err != nil {
		return err
	}
	if source == nil {
		if p.nodePoolTemplateProvider == nil {
			return &sandbox.ConfigurationError{Reason: "bootstrap source provider is unavailable for sandbox adoption"}
		}
		source, err = p.nodePoolTemplateProvider.GetSourceTemplateMetadata(ctx)
		if err != nil {
			return &sandbox.ConfigurationError{Reason: "bootstrap source is unavailable for sandbox adoption"}
		}
	}
	return p.validateAdoptionArtifacts(ctx, vm, class, it, source, actual)
}

func (p *DefaultProvider) validateAdoptionArtifacts(ctx context.Context, vm *compute.Instance, class *v1alpha1.GCENodeClass, it *cloudprovider.InstanceType, source *compute.Metadata, actual *metadata.SandboxIdentity) error {
	profile, err := p.adoptionBootstrapProfile(ctx, source)
	if err != nil {
		return err
	}

	imageID, err := p.sandboxBootImage(ctx, vm, lastPathSegment(vm.Zone))
	if err != nil {
		return err
	}
	// A still-selected alternative is a legitimate retry; selector reordering
	// must not replace it. The same shared requirements/policy predicate runs.
	selected := class.DeepCopy()
	selected.Status.Images = nil
	for _, image := range class.Status.Images {
		if sandbox.ImageMatches(imageID, image.SourceImage) {
			selected.Status.Images = append(selected.Status.Images, image)
		}
	}

	image, err := imagefamily.SelectLaunchImage(selected, it, profile.Release)
	if err != nil {
		return err
	}
	expected, err := sandbox.LaunchIdentity(class, *image.SandboxInstaller, profile, it.Name, it.Requirements.Get(corev1.LabelArchStable).Any())
	if err != nil {
		return err
	}
	if actual.Runtime != expected.Runtime || actual.Installer != expected.Installer || actual.Profile != expected.Profile {
		return &sandbox.ConfigurationError{Reason: "VM sandbox installer or bootstrap policy differs from the launch request"}
	}
	return nil
}

func claimClassName(class *v1alpha1.GCENodeClass) string {
	if class == nil {
		return ""
	}
	return class.Name
}

func adoptionInstanceType(vm *compute.Instance, types []*cloudprovider.InstanceType) *cloudprovider.InstanceType {
	for _, it := range types {
		if it != nil && it.Name == lastPathSegment(vm.MachineType) {
			return it
		}
	}
	return nil
}

func sandboxBootDisk(vm *compute.Instance) (*compute.AttachedDisk, error) {
	var boot *compute.AttachedDisk
	for _, disk := range vm.Disks {
		if disk != nil && disk.Boot {
			if boot != nil {
				return nil, &sandbox.ConfigurationError{Reason: "VM has multiple boot disks"}
			}
			boot = disk
		}
	}
	if boot == nil {
		return nil, &sandbox.ConfigurationError{Reason: "VM boot image is unavailable"}
	}
	return boot, nil
}

func validateAdoptionOwnership(vm *compute.Instance, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim, enabled bool) error {
	// Existing ownership labels are binding even on legacy ordinary VMs. Older
	// ordinary VMs without the new runtime metadata remain valid retries.
	for key, want := range map[string]string{
		utils.SanitizeGCELabelValue(utils.LabelNodePoolKey):     claim.Labels[karpv1.NodePoolLabelKey],
		utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey): claimClassName(class),
	} {
		got, present := vm.Labels[key]
		if (enabled || present) && got != want {
			return &sandbox.ConfigurationError{Reason: "VM launch ownership reference changed"}
		}
	}
	return nil
}

func (p *DefaultProvider) resolveAdoptionType(ctx context.Context, vm *compute.Instance, class *v1alpha1.GCENodeClass, types []*cloudprovider.InstanceType) (*cloudprovider.InstanceType, error) {
	it := adoptionInstanceType(vm, types)
	if it == nil && p.instanceTypeProvider != nil {
		catalog, err := p.instanceTypeProvider.List(ctx, class)
		if err != nil {
			return nil, err
		}
		it = adoptionInstanceType(vm, catalog)
	}
	if it == nil {
		return nil, &sandbox.ConfigurationError{Reason: "VM machine type facts are unavailable for sandbox adoption"}
	}
	return it, nil
}

func (p *DefaultProvider) adoptForClaim(ctx context.Context, vm *compute.Instance, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim, types []*cloudprovider.InstanceType) (*Instance, error) {
	if err := p.validateAdoption(ctx, vm, class, claim, types, nil); err != nil {
		return nil, adoptionError(err)
	}
	return p.adoptExistingInstance(ctx, vm), nil
}

func bindSandboxPool(target *metadata.InstanceMetadata, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim) error {
	if !class.SandboxEnabled() {
		return nil
	}
	uid, err := sandbox.NodePoolOwnerUID(claim)
	if err != nil {
		return err
	}
	target.SetSandboxPoolUID(uid)
	return nil
}

func (p *DefaultProvider) adoptionBootstrapProfile(ctx context.Context, source *compute.Metadata) (sandbox.BootstrapProfile, error) {
	target, err := metadata.FromSourceTemplate(source)
	if err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: "bootstrap source has an invalid typed configuration"}
	}
	if err := metadata.ValidateSourceKubeEnv(target); err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: err.Error()}
	}
	if err := target.ApplySandbox(nil); err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: err.Error()}
	}
	manifests, _ := target.GetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL")
	server, _ := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	resolver := p.bootstrapResolver
	if resolver == nil {
		resolver = sandbox.SharedResolver()
	}
	return resolver.ResolveBootstrap(ctx, manifests, server)
}

func renderLaunchMetadata(target *metadata.InstanceMetadata, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim) (*compute.Metadata, error) {
	if err := bindSandboxPool(target, class, claim); err != nil {
		return nil, err
	}
	return target.ToComputeMetadata()
}

func adoptionError(err error) error {
	var configuration *sandbox.ConfigurationError
	if errors.As(err, &configuration) || sandbox.IsBootstrapUnsupported(err) {
		return cloudprovider.NewNodeClassNotReadyError(err)
	}
	return cloudprovider.NewCreateError(err, "SandboxIdentityResolutionFailed", "Unable to resolve VM launch identity")
}
