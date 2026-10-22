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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloud "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

type lifecycleProfile struct {
	profile sandbox.BootstrapProfile
	err     error
}

func (p lifecycleProfile) ResolveBootstrap(context.Context, string, string) (sandbox.BootstrapProfile, error) {
	return p.profile, p.err
}

func durableInstance(class *v1alpha1.GCENodeClass) *instance.Instance {
	return &instance.Instance{Name: "durable", ProjectID: "test-project", Location: "us-central1-a", Type: "n2-standard-8", ImageID: sandboxCPUImage,
		Labels:  map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool", metadata.SandboxRuntimeInstanceLabel: "gvisor"},
		Sandbox: &metadata.SandboxIdentity{Runtime: "gvisor", Installer: "gke.gcr.io/gke-gvisor-installer@sha256:" + strings.Repeat("a", 64), Profile: "gke-gvisor-20260323/native-xemu-smt-nftables-v1/amd64-core-tags", ClassUID: string(class.UID), ClassHash: v1alpha1.GCENodeClassHashVersion + "/" + class.Hash()},
	}
}

// These are drift decisions over historical facts, not artifact HTTP tests.
//
//nolint:gocyclo
func TestSandboxDriftUsesDurableVMInstallerAndPolicy(t *testing.T) {
	for _, scenario := range []string{"unchanged", "installer", "schema", "policy", "hardware", "recreated class", "ordinary VM", "disabled class", "malformed identity", "new image", "unknown boot image", "unknown consumers", "incomplete source", "claim annotation tampering"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, _ := runtimeFixtures()
			class.UID = "class-uid"
			it := runtimeType(ctx, class, "n2-standard-8", "")
			p := runtimeProvider(class, pool, it)
			profile := sandbox.BootstrapProfile{Release: "v1.35.6-gke.1250001", Schema: "gke-gvisor-20260323", Policy: "native-xemu-smt-nftables-v1"}
			inst := durableInstance(class)
			p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst}}
			claim := p.instanceToNodeClaim(inst, it)
			want := SandboxDrift
			switch scenario {
			case "unchanged":
				want = ""
			case "installer":
				class.Status.Images[0].SandboxInstaller.Digest = "sha256:" + strings.Repeat("b", 64)
			case "schema":
				profile.Schema = "new-reviewed-schema"
			case "policy":
				profile.Policy = "new-reviewed-policy"
			case "hardware":
				inst.Sandbox.Profile = "gke-gvisor-20260323/native-xemu-smt-nftables-v1/amd64-amd"
			case "recreated class":
				class.UID = "new-class-uid"
			case "ordinary VM":
				inst.Sandbox = nil
				delete(inst.Labels, metadata.SandboxRuntimeInstanceLabel)
			case "disabled class":
				class.Spec.SandboxConfig = nil
			case "malformed identity":
				inst.Sandbox.Malformed = true
			case "new image":
				class.Status.Images[0].SourceImage = strings.Replace(sandboxCPUImage, "395-109", "395-110", 1)
				want = ImageDrift
			case "unknown boot image":
				inst.ImageID = ""
				want = ImageDrift
			case "unknown consumers":
				p.bootstrapResolver = lifecycleProfile{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown consumers"}}
			case "incomplete source":
				p.sourceProvider = schedulingSource{kubeEnv: "KUBE_MANIFESTS_TAR_URL: synthetic"}
			case "claim annotation tampering":
				claim.Annotations[metadata.SandboxProfileMetadataKey] = "fake"
				claim.Annotations[metadata.SandboxInstallerMetadataKey] = "fake"
				want = ""
			}
			if scenario != "unknown consumers" {
				p.bootstrapResolver = lifecycleProfile{profile: profile}
			}
			// Sandbox comparison runs after existing static/image drift. No need
			// to fake readiness or restamp today's hash onto the launched VM.
			drift, err := p.isSandboxDrifted(ctx, claim, class)
			if scenario == "unknown consumers" || scenario == "incomplete source" || scenario == "unknown boot image" {
				require.Error(t, err)
				require.Empty(t, drift)
				return
			}
			require.NoError(t, err)
			require.Equal(t, want, drift)
		})
	}
}

func TestIsDriftedIntegratesSameImagePolicyDrift(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	class.UID = "class-uid"
	it := runtimeType(ctx, class, "n2-standard-8", "")
	p := runtimeProvider(class, pool, it)
	inst := durableInstance(class)
	p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst}}
	p.bootstrapResolver = lifecycleProfile{profile: sandbox.BootstrapProfile{Release: "v1.35.6-gke.1250001", Schema: "gke-gvisor-20260323", Policy: "policy-v2"}}
	got, err := p.IsDrifted(ctx, p.instanceToNodeClaim(inst, it))
	require.NoError(t, err)
	require.Equal(t, SandboxDrift, got)
}

func TestIsDriftedUsesActualSandboxBootImage(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		actualImage  string
		claimImage   string
		want         corecloud.DriftReason
		unknownImage bool
	}{
		{name: "unknown actual and empty claim", unknownImage: true},
		{name: "unknown actual and stale claim", claimImage: "stale-image", unknownImage: true},
		{name: "matching actual and empty claim", actualImage: sandboxCPUImage},
		{name: "matching actual and stale claim", actualImage: sandboxCPUImage, claimImage: "stale-image"},
		{name: "changed actual and matching claim", actualImage: strings.Replace(sandboxCPUImage, "395-109", "395-110", 1), claimImage: sandboxCPUImage, want: ImageDrift},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, _ := runtimeFixtures()
			class.UID = "class-uid"
			it := runtimeType(ctx, class, "n2-standard-8", "")
			p := runtimeProvider(class, pool, it)
			inst := durableInstance(class)
			inst.ImageID = scenario.actualImage
			p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst}}
			p.bootstrapResolver = lifecycleProfile{profile: sandbox.BootstrapProfile{Release: "v1.35.6-gke.1250001", Schema: "gke-gvisor-20260323", Policy: "native-xemu-smt-nftables-v1"}}
			claim := p.instanceToNodeClaim(inst, it)
			claim.Status.ImageID = scenario.claimImage
			got, err := p.IsDrifted(ctx, claim)
			if scenario.unknownImage {
				require.ErrorContains(t, err, "boot image is unavailable")
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, scenario.want, got)
		})
	}
}

func TestIsDriftedRetainsHistoricalAndPartialSandboxIndicatorsAfterDisable(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical sandbox", true: "empty owned identity"}[partial], func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, _ := runtimeFixtures()
			class.UID = "class-uid"
			inst := durableInstance(class)
			if partial {
				inst.Sandbox = &metadata.SandboxIdentity{Malformed: true}
			}
			class.Spec.SandboxConfig = nil
			p := runtimeProvider(class, pool)
			p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst}}
			p.sourceProvider = nil // Mode drift does not need a desired profile.
			claim := p.instanceToNodeClaim(inst, nil)
			if partial {
				claim.Annotations = map[string]string{metadata.SandboxProfileMetadataKey: ""}
			}
			claim.Status.ImageID = "stale-image"
			got, err := p.IsDrifted(ctx, claim)
			require.NoError(t, err)
			require.Equal(t, SandboxDrift, got)
		})
	}
}

func TestIsDriftedKeepsProvenStaticDriftBeforeSandboxReads(t *testing.T) {
	class, pool, claim := runtimeFixtures()
	class.Annotations = map[string]string{v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion, v1alpha1.AnnotationGCENodeClassHash: class.Hash()}
	claim.Annotations = map[string]string{v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion, v1alpha1.AnnotationGCENodeClassHash: "previous-configuration"}
	p := runtimeProvider(class, pool)
	p.instanceProvider = nil
	p.sourceProvider = nil
	got, err := p.IsDrifted(runtimeContext(), claim)
	require.NoError(t, err)
	require.Equal(t, NodeClassDrift, got)
}

func TestOrdinaryDriftDoesNotAcquireSandboxDependencies(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	class.Spec.SandboxConfig = nil
	p := runtimeProvider(class, pool)
	p.sourceProvider = nil
	p.instanceProvider = nil
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{karpv1.NodePoolLabelKey: "pool"}}, Status: karpv1.NodeClaimStatus{ImageID: sandboxCPUImage}}
	got, err := p.IsDrifted(ctx, claim)
	require.NoError(t, err)
	require.Empty(t, got)
	claim.Status.ImageID = "old-ordinary-image"
	got, err = p.IsDrifted(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, ImageDrift, got, "ordinary image drift retains its dependency-free fast path")
	// Static enable/disable hashing remains the first, established drift path.
	enabled := class.DeepCopy()
	enabled.Spec.SandboxConfig = &v1alpha1.SandboxConfig{Type: "GVISOR"}
	claim.Annotations = map[string]string{v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion, v1alpha1.AnnotationGCENodeClassHash: class.Hash()}
	enabled.Annotations = map[string]string{v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion, v1alpha1.AnnotationGCENodeClassHash: enabled.Hash()}
	require.Equal(t, NodeClassDrift, p.areStaticFieldsDrifted(claim, enabled))
	claim.Annotations[v1alpha1.AnnotationGCENodeClassHash] = enabled.Hash()
	class.Annotations = map[string]string{v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion, v1alpha1.AnnotationGCENodeClassHash: class.Hash()}
	require.Equal(t, NodeClassDrift, p.areStaticFieldsDrifted(claim, class))
}

func TestHistoricalRuntimeAndLaunchAnnotationsIgnoreCurrentClassAndCache(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	class.UID = "class-uid"
	it := runtimeType(ctx, class, "n2-standard-8", "")
	inst := durableInstance(class)
	before := *inst.Sandbox
	ordinary := *inst
	ordinary.Name = "ordinary"
	ordinary.Sandbox = nil
	p := runtimeProvider(class, pool, it)
	p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst, &ordinary}}
	class.Spec.SandboxConfig = nil
	class.Spec.Labels = map[string]string{"edited": "true"}
	class.Status.Images = nil
	p.sourceProvider = nil
	got, err := p.Get(ctx, "gce://test-project/us-central1-a/durable")
	require.NoError(t, err)
	require.Equal(t, before.Installer, got.Annotations[metadata.SandboxInstallerMetadataKey])
	require.Equal(t, before.Profile, got.Annotations[metadata.SandboxProfileMetadataKey])
	require.Equal(t, before.ClassUID, got.Annotations[metadata.SandboxClassUIDMetadataKey])
	require.Equal(t, before.ClassHash, got.Annotations[metadata.SandboxClassHashMetadataKey])
	require.NotEqual(t, class.Hash(), got.Annotations[v1alpha1.AnnotationGCENodeClassHash])
	listed, err := p.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	for _, claim := range listed {
		if strings.HasSuffix(claim.Status.ProviderID, "/ordinary") {
			require.NotContains(t, claim.Labels, v1alpha1.LabelSandboxRuntime)
			require.NotContains(t, claim.Annotations, metadata.SandboxProfileMetadataKey)
		}
	}
}

type returningLifecycleInstance struct {
	instance.Provider
	result       *instance.Instance
	duringCreate func()
}

func (p returningLifecycleInstance) Create(context.Context, *v1alpha1.GCENodeClass, *karpv1.NodeClaim, []*corecloud.InstanceType) (*instance.Instance, error) {
	if p.duringCreate != nil {
		p.duringCreate()
	}
	return p.result, nil
}

func TestCreatePreservesLaunchedHashAndRejectsPostProviderRefRace(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(map[bool]string{false: "genuine retry", true: "pool ref changed"}[race], func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, claim := runtimeFixtures()
			class.UID = "class-uid"
			it := runtimeType(ctx, class, "n2-standard-8", "")
			p := runtimeProvider(class, pool, it)
			inst := durableInstance(class)
			inst.Sandbox.ClassHash = "v5/durably-launched-hash"
			returning := returningLifecycleInstance{result: inst}
			if race {
				returning.duringCreate = func() {
					pool.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "other"}
				}
			}
			p.instanceProvider = returning
			got, err := p.Create(ctx, claim)
			if race {
				require.Error(t, err)
				require.True(t, corecloud.IsNodeClassNotReadyError(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, "durably-launched-hash", got.Annotations[v1alpha1.AnnotationGCENodeClassHash])
			require.Equal(t, "gvisor", got.Labels[v1alpha1.LabelSandboxRuntime])
			require.Equal(t, it.Name, got.Labels[corev1.LabelInstanceTypeStable])
		})
	}
}

func TestCreateAcceptsUnchangedBootSecondNodeClass(t *testing.T) {
	for _, mode := range []string{"ordinary", "sandbox"} {
		t.Run(mode, func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, claim := runtimeFixtures()
			class.UID = "class-uid"
			class.Spec.Disks = []v1alpha1.Disk{
				{SizeGiB: 20, Category: "pd-ssd"},
				{Boot: true, SizeGiB: 60, Category: "pd-balanced"},
			}
			if mode == "ordinary" {
				class.Spec.SandboxConfig = nil
				pool.Spec.Template.Labels = nil
				pool.Spec.Template.Spec.Taints = nil
				delete(claim.Labels, v1alpha1.LabelSandboxRuntime)
				claim.Spec.Requirements = nil
				claim.Spec.Taints = nil
			}
			it := runtimeType(ctx, class, "n2-standard-8", "")
			p := runtimeProvider(class, pool, it)
			inst := durableInstance(class)
			if mode == "ordinary" {
				inst.Sandbox = nil
				delete(inst.Labels, metadata.SandboxRuntimeInstanceLabel)
			}
			p.instanceProvider = returningLifecycleInstance{result: inst}
			before := class.DeepCopy()

			got, err := p.Create(ctx, claim)
			require.NoError(t, err)
			require.Equal(t, "gce://test-project/us-central1-a/durable", got.Status.ProviderID)
			require.Equal(t, before, class)
		})
	}
}

func TestCreateChecksGenuinePoolOwnerUIDAgainstPersistedPool(t *testing.T) {
	class, pool, claim := runtimeFixtures()
	pool.UID = "pool-uid"
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "pool", UID: "pool-uid"}}
	p := runtimeProvider(class, pool)
	require.NoError(t, p.validateRuntimeCreate(runtimeContext(), claim, class))
	claim.OwnerReferences[0].UID = "stale-pool-uid"
	require.Error(t, p.validateRuntimeCreate(runtimeContext(), claim, class))
	claim.OwnerReferences[0].UID = "pool-uid"
	claim.OwnerReferences[0].APIVersion = "other.example/v1"
	require.Error(t, p.validateRuntimeCreate(runtimeContext(), claim, class))
}

func TestHistoricalSandboxGarbageCollectionIgnoresLaunchabilityAndMalformedFacts(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	class.UID = "class-uid"
	orphan := durableInstance(class)
	orphan.Name = "sandbox-orphan"
	orphan.Type = "removed-custom-shape"
	orphan.CreationTime = time.Now().Add(-time.Hour)
	orphan.Labels[utils.LabelClusterLocationKey] = "us-central1"
	orphan.Sandbox.Malformed = true
	backed := *orphan
	backed.Name = "sandbox-backed"
	instances := &lifecycleInstances{instances: []*instance.Instance{orphan, &backed}}
	backedID := "gce://test-project/us-central1-a/sandbox-backed"
	kc := lifecycleKubeClient{reproClient: reproClient{nodePool: pool, nodeClass: class}, nodeClaims: []karpv1.NodeClaim{{Status: karpv1.NodeClaimStatus{ProviderID: backedID}}}}
	class.Spec.SandboxConfig = nil
	class.Status.Images = nil
	p := New(kc, reproEvents{}, reproTypes{}, instances, nil, nil)
	listed, err := p.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, "gvisor", listed[0].Labels[v1alpha1.LabelSandboxRuntime])
	_, err = garbagecollection.NewController(kc, p).Reconcile(ctx)
	require.NoError(t, err)
	remaining, err := p.List(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	require.Equal(t, backedID, remaining[0].Status.ProviderID)
}
