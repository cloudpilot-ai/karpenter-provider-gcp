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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	sandboxcontract "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/sandbox"
)

func managedNodeClassRef(ref *karpv1.NodeClassReference) bool {
	return ref != nil && ref.Group == apis.Group && ref.Kind == "GCENodeClass" && ref.Name != ""
}

func sameSchedulingPool(snapshot, persisted *karpv1.NodePool) bool {
	return snapshot.UID == persisted.UID && persisted.DeletionTimestamp.IsZero() && equality.Semantic.DeepEqual(snapshot.Spec.Template, persisted.Spec.Template)
}

func validateRuntimePool(pool *karpv1.NodePool, class *v1alpha1.GCENodeClass) error {
	if !pool.DeletionTimestamp.IsZero() {
		return &sandbox.ConfigurationError{Reason: "NodePool is terminating"}
	}
	if err := sandbox.ValidateConfiguration(class); err != nil {
		return err
	}
	if err := sandboxcontract.ValidatePool(pool, class.SandboxEnabled()); err != nil {
		return &sandbox.ConfigurationError{Reason: err.Error()}
	}
	return nil
}

// NodeClassNotReadyError is terminal for an immutable stale claim in upstream
// Karpenter, but does not mark any zonal offering unavailable.
func (c *CloudProvider) validateRuntimeCreate(ctx context.Context, claim *karpv1.NodeClaim, scheduledClass *v1alpha1.GCENodeClass) error {
	if !managedNodeClassRef(claim.Spec.NodeClassRef) {
		return &sandbox.ConfigurationError{Reason: "NodeClaim has no managed GCENodeClass reference"}
	}
	poolName := claim.Labels[karpv1.NodePoolLabelKey]
	if poolName == "" {
		return &sandbox.ConfigurationError{Reason: "NodeClaim has no NodePool identity"}
	}
	pool := &karpv1.NodePool{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: poolName}, pool); err != nil {
		return fmt.Errorf("resolving launch NodePool: %w", err)
	}
	if err := validateClaimPoolIdentity(claim, pool); err != nil {
		return err
	}
	class, err := c.resolveNodeClassFromNodePool(ctx, pool)
	if err != nil {
		return err
	}
	if class.UID != scheduledClass.UID || !equality.Semantic.DeepEqual(class.Spec, scheduledClass.Spec) || !equality.Semantic.DeepEqual(class.Status.Images, scheduledClass.Status.Images) {
		return &sandbox.ConfigurationError{Reason: "GCENodeClass changed during launch preparation"}
	}
	if err := validateRuntimePool(pool, class); err != nil {
		return err
	}
	if err := sandboxcontract.ValidateClaim(claim, class.SandboxEnabled()); err != nil {
		return &sandbox.ConfigurationError{Reason: err.Error()}
	}
	return nil
}

// Only new scheduling/launch candidates are filtered. Provider List remains the
// complete historical catalog used by Get/List/adoption/garbage collection.
func (c *CloudProvider) sandboxCandidates(ctx context.Context, class *v1alpha1.GCENodeClass, types []*cloudprovider.InstanceType) ([]*cloudprovider.InstanceType, error) {
	if !class.SandboxEnabled() {
		return types, nil
	}
	release, err := c.sandboxBootstrapRelease(ctx, class)
	if err != nil {
		return nil, err
	}
	var candidates []*cloudprovider.InstanceType
	var firstError error
	for _, it := range types {
		if err := sandboxCandidate(class, it, release); err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		candidates = append(candidates, it)
	}
	if len(candidates) == 0 {
		if firstError != nil {
			return nil, firstError
		}
		return nil, &sandbox.ConfigurationError{Reason: "no compatible sandbox instance types"}
	}
	return candidates, nil
}

func sandboxCandidate(class *v1alpha1.GCENodeClass, it *cloudprovider.InstanceType, release string) error {
	arch := it.Requirements.Get(corev1.LabelArchStable)
	if arch.Operator() != corev1.NodeSelectorOpIn || arch.Len() != 1 {
		return &sandbox.ConfigurationError{Reason: "sandbox candidate has no factual architecture"}
	}
	if _, err := sandbox.ResolveHardwarePolicy(it.Name, arch.Any()); err != nil {
		return err
	}
	_, err := imagefamily.SelectLaunchImage(class, it, release)
	return err
}

func (c *CloudProvider) sandboxBootstrapRelease(ctx context.Context, class *v1alpha1.GCENodeClass) (string, error) {
	profile, err := c.sandboxBootstrapProfile(ctx, class)
	return profile.Release, err
}

func (c *CloudProvider) sandboxBootstrapProfile(ctx context.Context, class *v1alpha1.GCENodeClass) (sandbox.BootstrapProfile, error) {
	if err := sandbox.ValidateConfiguration(class); err != nil {
		return sandbox.BootstrapProfile{}, err
	}
	if c.sourceProvider == nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: "bootstrap source provider is unavailable"}
	}
	source, err := c.sourceProvider.GetSourceTemplateMetadata(ctx)
	if err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: "bootstrap source template is unavailable"}
	}
	target, err := metadata.FromSourceTemplate(source)
	if err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: "bootstrap source metadata has an invalid typed configuration"}
	}
	if err := metadata.ValidateSourceKubeEnv(target); err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: err.Error()}
	}
	if err := target.ApplySandbox(nil); err != nil {
		return sandbox.BootstrapProfile{}, &sandbox.ConfigurationError{Reason: err.Error()}
	}
	manifests, _ := target.GetKubeEnvEntry("KUBE_MANIFESTS_TAR_URL")
	server, _ := target.GetKubeEnvEntry("SERVER_BINARY_TAR_URL")
	profile, err := c.bootstrapResolver.ResolveBootstrap(ctx, manifests, server)
	return profile, err
}

func validateClaimPoolIdentity(claim *karpv1.NodeClaim, pool *karpv1.NodePool) error {
	if !equality.Semantic.DeepEqual(pool.Spec.Template.Spec.NodeClassRef, claim.Spec.NodeClassRef) {
		return &sandbox.ConfigurationError{Reason: "NodePool GCENodeClass reference changed"}
	}
	uid, err := sandbox.NodePoolOwnerUID(claim)
	if err != nil {
		return err
	}
	if uid != "" && uid != string(pool.UID) {
		return &sandbox.ConfigurationError{Reason: "NodePool identity changed"}
	}
	return nil
}

func runtimeCreateError(err error) error {
	var configuration *sandbox.ConfigurationError
	if errors.As(err, &configuration) || sandbox.IsBootstrapUnsupported(err) || apierrors.IsNotFound(err) || errors.Is(err, errNodeClassTerminating) {
		return cloudprovider.NewNodeClassNotReadyError(err)
	}
	return cloudprovider.NewCreateError(err, "SandboxStateResolutionFailed", "Unable to resolve current runtime state")
}
