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

package cloudprovider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
)

const SandboxDrift cloudprovider.DriftReason = "SandboxDrift"

func mapSandboxIdentity(annotations map[string]string, identity *metadata.SandboxIdentity) {
	if identity == nil {
		return
	}
	annotations[metadata.SandboxRuntimeMetadataKey] = identity.Runtime
	annotations[metadata.SandboxInstallerMetadataKey] = identity.Installer
	annotations[metadata.SandboxProfileMetadataKey] = identity.Profile
	annotations[metadata.SandboxClassUIDMetadataKey] = identity.ClassUID
	annotations[metadata.SandboxClassHashMetadataKey] = identity.ClassHash
	if identity.PoolUID != "" {
		annotations[metadata.SandboxPoolUIDMetadataKey] = identity.PoolUID
	}
	if version, hash, ok := strings.Cut(identity.ClassHash, "/"); ok && version != "" && hash != "" {
		annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = version
		annotations[v1alpha1.AnnotationGCENodeClassHash] = hash
	}
}

// Presence, including an empty owned key, selects factual comparison after a
// class is disabled. These hints never establish a VM's actual runtime mode.
func requiresSandboxDrift(claim *karpv1.NodeClaim, class *v1alpha1.GCENodeClass) bool {
	if class.SandboxEnabled() {
		return true
	}
	if _, present := claim.Labels[v1alpha1.LabelSandboxRuntime]; present {
		return true
	}
	for _, key := range []string{metadata.SandboxRuntimeMetadataKey, metadata.SandboxInstallerMetadataKey, metadata.SandboxProfileMetadataKey, metadata.SandboxClassUIDMetadataKey, metadata.SandboxClassHashMetadataKey, metadata.SandboxPoolUIDMetadataKey} {
		if _, present := claim.Annotations[key]; present {
			return true
		}
	}
	return false
}

// Compare durably launched VM facts, not today's catalog labels or a mutable
// NodeClaim annotation. Ordinary nodes acquire no source/artifact dependency.
func (c *CloudProvider) isSandboxDrifted(ctx context.Context, claim *karpv1.NodeClaim, class *v1alpha1.GCENodeClass) (cloudprovider.DriftReason, error) {
	if !requiresSandboxDrift(claim, class) {
		return "", nil
	}
	actual, err := c.instanceProvider.Get(ctx, claim.Status.ProviderID)
	if err != nil {
		return "", cloudprovider.IgnoreNodeClaimNotFoundError(err)
	}
	if sandbox.ValidateIdentityMode(actual.Sandbox, actual.Labels, class.SandboxEnabled()) != nil {
		return SandboxDrift, nil //nolint:nilerr // Invalid durable facts are a drift decision, not a failed provider read.
	}
	if !class.SandboxEnabled() {
		return "", nil
	}
	if actual.Sandbox.ClassUID != string(class.UID) || actual.Sandbox.ClassHash != v1alpha1.GCENodeClassHashVersion+"/"+class.Hash() {
		return SandboxDrift, nil
	}
	if actual.ImageID == "" {
		return "", &sandbox.ConfigurationError{Reason: "VM boot image is unavailable for sandbox drift comparison"}
	}
	return c.sandboxArtifactDrift(ctx, actual, class)
}

func (c *CloudProvider) sandboxArtifactDrift(ctx context.Context, actual *instance.Instance, class *v1alpha1.GCENodeClass) (cloudprovider.DriftReason, error) {
	// Inventory is unfiltered. An absent historical type prevents comparison,
	// but never removes the instance from Get/List/garbage collection.
	types, err := c.instanceTypeProvider.List(ctx, class)
	if err != nil {
		return "", err
	}
	it, _ := matchVariantForInstance(types, actual)
	if it == nil {
		return "", &sandbox.ConfigurationError{Reason: "historical VM type is unavailable for sandbox drift comparison"}
	}
	selected := class.DeepCopy()
	selected.Status.Images = nil
	for _, image := range class.Status.Images {
		if sandbox.ImageMatches(actual.ImageID, image.SourceImage) {
			selected.Status.Images = append(selected.Status.Images, image)
		}
	}
	if len(selected.Status.Images) == 0 {
		return ImageDrift, nil
	}
	profile, err := c.sandboxBootstrapProfile(ctx, class)
	if err != nil {
		return "", err
	}
	image, err := imagefamily.SelectLaunchImage(selected, it, profile.Release)
	if err != nil {
		return "", err
	}
	expected, err := sandbox.LaunchIdentity(class, *image.SandboxInstaller, profile, it.Name, it.Requirements.Get(corev1.LabelArchStable).Any())
	if err != nil {
		return "", err
	}
	if actual.Sandbox.Installer != expected.Installer || actual.Sandbox.Profile != expected.Profile {
		return SandboxDrift, nil
	}
	return "", nil
}

func (c *CloudProvider) createRuntimeInstance(ctx context.Context, class *v1alpha1.GCENodeClass, claim *karpv1.NodeClaim, types []*cloudprovider.InstanceType) (*instance.Instance, error) {
	inst, err := c.instanceProvider.Create(ctx, class, claim, types)
	if err != nil {
		var configuration *sandbox.ConfigurationError
		if errors.As(err, &configuration) || sandbox.IsBootstrapUnsupported(err) {
			return nil, runtimeCreateError(err)
		}
		return nil, fmt.Errorf("creating instance, %w", err)
	}
	if err := c.validateRuntimeCreate(ctx, claim, class); err != nil {
		return nil, runtimeCreateError(err)
	}
	return inst, nil
}

func (c *CloudProvider) createdNodeClaim(inst *instance.Instance, it *cloudprovider.InstanceType, class *v1alpha1.GCENodeClass) *karpv1.NodeClaim {
	claim := c.instanceToNodeClaim(inst, it)
	if inst.Sandbox == nil {
		claim.Annotations[v1alpha1.AnnotationGCENodeClassHash] = class.Hash()
		claim.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = v1alpha1.GCENodeClassHashVersion
	}
	return claim
}
