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
	"strings"
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloud "sigs.k8s.io/karpenter/pkg/cloudprovider"
	karpopts "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	gcpoptions "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/operator/options"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instance"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/instancetype"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
	sandboxcontract "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils/sandbox"
)

const sandboxCPUImage = "projects/gke-node-images/global/images/gke-1356-gke1250001-cos-125-19216-395-109-c-pre"
const sandboxGPUImage = "projects/gke-node-images/global/images/gke-1356-gke1250001-cos-125-19216-220-150-pre-gv-nvda"
const sandboxSource = "CA_CERT: synthetic-ca\nKUBERNETES_MASTER_NAME: synthetic-master\nSERVER_BINARY_TAR_HASH: synthetic-sha512\n" +
	"KUBE_MANIFESTS_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-manifests.tar.gz\n" +
	"SERVER_BINARY_TAR_URL: https://storage.googleapis.com/gke-release/kubernetes/release/v1.35.6-gke.1250001/kubernetes-server-linux-amd64.tar.gz\n"

type schedulingSource struct {
	nodepooltemplate.Provider
	kubeEnv string
}

func (s schedulingSource) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return &compute.Metadata{Items: []*compute.MetadataItems{{Key: metadata.KubeEnvKey, Value: lo.ToPtr(s.kubeEnv)}}}, nil
}

type schedulingProfile struct {
	release string
	err     error
}

func (p schedulingProfile) ResolveBootstrap(context.Context, string, string) (sandbox.BootstrapProfile, error) {
	return sandbox.BootstrapProfile{Release: p.release}, p.err
}

func runtimeFixtures() (*v1alpha1.GCENodeClass, *karpv1.NodePool, *karpv1.NodeClaim) {
	class := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Name: "sandbox"}, Spec: v1alpha1.GCENodeClassSpec{SandboxConfig: &v1alpha1.SandboxConfig{Type: "GVISOR"}, ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: "cluster"}}}}
	class.Status.Images = []v1alpha1.Image{runtimeImage(sandboxCPUImage, false)}
	class.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
	class.StatusConditions().SetTrue(v1alpha1.ConditionTypeSandboxReady)
	ref := &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: "sandbox"}
	pool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "pool"}, Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{ObjectMeta: karpv1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelSandboxRuntime: "gvisor"}}, Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: ref, Taints: []corev1.Taint{sandboxcontract.RuntimeTaint()}}}}}
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim", Labels: map[string]string{karpv1.NodePoolLabelKey: "pool", v1alpha1.LabelSandboxRuntime: "gvisor"}}, Spec: karpv1.NodeClaimSpec{NodeClassRef: ref, Taints: []corev1.Taint{sandboxcontract.RuntimeTaint()}, Requirements: []karpv1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.LabelSandboxRuntime, Operator: corev1.NodeSelectorOpIn, Values: []string{"gvisor"}}}}}
	return class, pool, claim
}

func runtimeImage(source string, gpu bool) v1alpha1.Image {
	op := corev1.NodeSelectorOpDoesNotExist
	if gpu {
		op = corev1.NodeSelectorOpExists
	}
	return v1alpha1.Image{SourceImage: source, SandboxInstaller: &v1alpha1.SandboxInstaller{Version: "20260323.0_RC03", Digest: "sha256:" + strings.Repeat("a", 64)}, Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}, {Key: v1alpha1.LabelInstanceGPUCount, Operator: op}}}
}

func runtimeContext() context.Context {
	return karpopts.ToContext(gcpoptions.ToContext(context.Background(), &gcpoptions.Options{VMMemoryOverheadPercent: 0.07}), &karpopts.Options{})
}

func runtimeType(ctx context.Context, class *v1alpha1.GCENodeClass, name, gpu string) *corecloud.InstanceType {
	mt := &computepb.MachineType{Name: lo.ToPtr(name), GuestCpus: lo.ToPtr[int32](8), MemoryMb: lo.ToPtr[int32](32768)}
	if strings.HasPrefix(name, "t2a-") {
		mt.Architecture = lo.ToPtr("ARM64")
	}
	if gpu != "" {
		mt.Accelerators = []*computepb.Accelerators{{GuestAcceleratorType: lo.ToPtr(gpu), GuestAcceleratorCount: lo.ToPtr[int32](1)}}
	}
	return instancetype.NewInstanceType(ctx, mt, class, "us-central1", corecloud.Offerings{{Available: true, Requirements: scheduling.NewRequirements(scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "us-central1-a"), scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand))}}, 0)
}

func runtimeProvider(class *v1alpha1.GCENodeClass, pool *karpv1.NodePool, types ...*corecloud.InstanceType) *CloudProvider {
	p := New(reproClient{nodeClass: class, nodePool: pool}, reproEvents{}, reproTypes{variants: types}, nil, nil, schedulingSource{kubeEnv: sandboxSource})
	p.bootstrapResolver = schedulingProfile{release: "v1.35.6-gke.1250001"}
	return p
}

func TestGetInstanceTypesWaitsForPersistedIsolationWithoutMutation(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	p := runtimeProvider(class, pool, runtimeType(ctx, class, "n2-standard-8", ""))
	pool.Spec.Template.Labels = nil
	pool.Spec.Template.Spec.Taints = nil
	before := pool.DeepCopy()
	_, err := p.GetInstanceTypes(ctx, pool)
	require.Error(t, err)
	require.Equal(t, before, pool)
	// A caller cannot bypass the gate by repairing only its scheduler snapshot.
	snapshot := pool.DeepCopy()
	snapshot.Spec.Template.Labels = map[string]string{v1alpha1.LabelSandboxRuntime: "gvisor"}
	snapshot.Spec.Template.Spec.Taints = []corev1.Taint{sandboxcontract.RuntimeTaint()}
	_, err = p.GetInstanceTypes(ctx, snapshot)
	require.ErrorContains(t, err, "stale")
	require.Equal(t, before, pool)
	pool.Spec.Template = snapshot.Spec.Template
	got, err := p.GetInstanceTypes(ctx, pool.DeepCopy())
	require.NoError(t, err)
	require.Len(t, got, 1)
	pool.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.LabelSandboxRuntime, Operator: corev1.NodeSelectorOpDoesNotExist}}
	_, err = p.GetInstanceTypes(ctx, pool.DeepCopy())
	require.ErrorContains(t, err, "contradict")
}

func TestSandboxCandidateSelectionUsesEffectiveReleaseAndORImages(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	cpu := runtimeType(ctx, class, "n2-custom-8-32768", "")
	arm := runtimeType(ctx, class, "t2a-standard-8", "")
	gpu := runtimeType(ctx, class, "a3-ultragpu-8g", "nvidia-h200-141gb")
	rejected := runtimeType(ctx, class, "n1-standard-8", "nvidia-tesla-v100")
	old := runtimeImage(strings.Replace(sandboxGPUImage, "1356-gke1250001", "1340-gke1712999", 1), true)
	class.Status.Images = append(class.Status.Images, old, runtimeImage(sandboxGPUImage, true))
	original := class.DeepCopy()
	p := runtimeProvider(class, pool, cpu, arm, gpu, rejected)
	got, err := p.GetInstanceTypes(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, []*corecloud.InstanceType{cpu, gpu}, got, "unmatched Arm and unsupported GPU must not be offered")
	require.Equal(t, original.Status.Images, class.Status.Images, "OR order must not change")
	// An old effective source blocks H200 even when the master/image is newer.
	p.bootstrapResolver = schedulingProfile{release: "v1.34.0-gke.1712999"}
	got, err = p.GetInstanceTypes(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, []*corecloud.InstanceType{cpu}, got)
	// A requirement-compatible but policy-incompatible image alone is not enough.
	p.bootstrapResolver = schedulingProfile{release: "v1.35.6-gke.1250001"}
	class.Status.Images = []v1alpha1.Image{old}
	p.instanceTypeProvider = reproTypes{variants: []*corecloud.InstanceType{gpu}}
	_, err = p.GetInstanceTypes(ctx, pool)
	require.Error(t, err)
	// CPU images cannot substitute for the missing sandbox GPU role.
	class.Status.Images = []v1alpha1.Image{runtimeImage(sandboxCPUImage, false)}
	_, err = p.GetInstanceTypes(ctx, pool)
	require.Error(t, err)
}

func TestSandboxCandidateGateValidatesSourceAndProfileWithoutHidingInventory(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	it := runtimeType(ctx, class, "n2-standard-8", "")
	p := runtimeProvider(class, pool, it)
	p.sourceProvider = schedulingSource{kubeEnv: strings.Replace(sandboxSource, "CA_CERT: synthetic-ca\n", "", 1)}
	_, err := p.GetInstanceTypes(ctx, pool)
	require.ErrorContains(t, err, "CA_CERT")
	var config *sandbox.ConfigurationError
	require.ErrorAs(t, err, &config)
	p.sourceProvider = schedulingSource{kubeEnv: sandboxSource}
	p.bootstrapResolver = schedulingProfile{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown fingerprint"}}
	_, err = p.GetInstanceTypes(ctx, pool)
	require.True(t, sandbox.IsBootstrapUnsupported(err))
	// Historical inventory bypasses launch/readiness filters entirely.
	all, err := p.getAllInstanceTypes(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, []*corecloud.InstanceType{it}, all)
}

type launchRecorder struct {
	instance.Provider
	calls      int
	candidates []*corecloud.InstanceType
}

func (p *launchRecorder) Create(_ context.Context, _ *v1alpha1.GCENodeClass, _ *karpv1.NodeClaim, types []*corecloud.InstanceType) (*instance.Instance, error) {
	p.calls++
	p.candidates = types
	return &instance.Instance{Type: types[0].Name, Sandbox: &metadata.SandboxIdentity{Runtime: "gvisor", Installer: "immutable", Profile: "reviewed"}}, nil
}

type changingTypes struct {
	reproTypes
	duringList func()
}

func (p changingTypes) List(ctx context.Context, class *v1alpha1.GCENodeClass) ([]*corecloud.InstanceType, error) {
	if p.duringList != nil {
		p.duringList()
	}
	return p.reproTypes.List(ctx, class)
}

// Each case represents a distinct scheduling-to-launch race or immutable-field violation.
//
//nolint:gocyclo
func TestCreateRejectsStaleImmutableRuntimeContractsBeforeProviderCall(t *testing.T) {
	for _, scenario := range []string{"missing label", "missing requirement", "wrong requirement", "missing taint", "disabled class", "changed pool ref", "deleted pool", "recreated pool", "deleted class", "deleting class", "pool changes during resolution", "class changes during resolution"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, claim := runtimeFixtures()
			p := runtimeProvider(class, pool, runtimeType(ctx, class, "n2-standard-8", ""))
			launched := &launchRecorder{}
			p.instanceProvider = launched
			switch scenario {
			case "missing label":
				delete(claim.Labels, v1alpha1.LabelSandboxRuntime)
			case "missing requirement":
				claim.Spec.Requirements = nil
			case "wrong requirement":
				claim.Spec.Requirements[0].Values = []string{"other"}
			case "missing taint":
				claim.Spec.Taints = nil
			case "disabled class":
				class.Spec.SandboxConfig = nil
			case "changed pool ref":
				ref := *pool.Spec.Template.Spec.NodeClassRef
				ref.Name = "other"
				pool.Spec.Template.Spec.NodeClassRef = &ref
			case "deleted pool":
				p.kubeClient = reproClient{nodeClass: class, nodePoolNotFound: true}
			case "recreated pool":
				claim.OwnerReferences = []metav1.OwnerReference{{Kind: "NodePool", Name: pool.Name, UID: "original-pool"}}
				pool.UID = "replacement-pool"
			case "deleted class":
				p.kubeClient = reproClient{nodeClassNotFound: true}
			case "deleting class":
				class.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			case "pool changes during resolution":
				p.instanceTypeProvider = changingTypes{reproTypes: reproTypes{variants: []*corecloud.InstanceType{runtimeType(ctx, class, "n2-standard-8", "")}}, duringList: func() { pool.Spec.Template.Spec.Taints = nil }}
			case "class changes during resolution":
				p.instanceTypeProvider = changingTypes{reproTypes: reproTypes{variants: []*corecloud.InstanceType{runtimeType(ctx, class, "n2-standard-8", "")}}, duringList: func() { class.Spec.Labels = map[string]string{"change": "true"} }}
			}
			_, err := p.Create(ctx, claim)
			require.Error(t, err)
			require.True(t, corecloud.IsNodeClassNotReadyError(err), "stale claims must terminate, not retry forever: %v", err)
			require.False(t, corecloud.IsInsufficientCapacityError(err), "configuration must not poison capacity")
			require.Zero(t, launched.calls)
		})
	}
}

func TestCreateUsesCandidateGateAndReturnsOnlyActualRuntimeFacts(t *testing.T) {
	ctx := runtimeContext()
	class, pool, claim := runtimeFixtures()
	cpu := runtimeType(ctx, class, "n2-standard-8", "")
	unmatched := runtimeType(ctx, class, "t2a-standard-8", "")
	p := runtimeProvider(class, pool, cpu, unmatched)
	launched := &launchRecorder{}
	p.instanceProvider = launched
	got, err := p.Create(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, []*corecloud.InstanceType{cpu}, launched.candidates)
	require.Equal(t, "gvisor", got.Labels[v1alpha1.LabelSandboxRuntime])
	// Catalog requirements must not fabricate runtime identity for old ordinary VMs.
	historical := p.instanceToNodeClaim(&instance.Instance{Type: cpu.Name}, cpu)
	require.NotContains(t, historical.Labels, v1alpha1.LabelSandboxRuntime)
	cpuCapacity := historical.Status.Capacity[corev1.ResourceCPU]
	require.EqualValues(t, 8, cpuCapacity.Value())
	p.bootstrapResolver = schedulingProfile{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown fingerprint"}}
	_, err = p.Create(ctx, claim)
	require.True(t, corecloud.IsNodeClassNotReadyError(err))
	require.Equal(t, 1, launched.calls)
}

func TestOrdinarySchedulingRejectsRuntimeIntentButNeedsNoSourceProfile(t *testing.T) {
	ctx := runtimeContext()
	class, pool, _ := runtimeFixtures()
	class.Spec.SandboxConfig = nil
	class.Status.Images = nil
	pool.Spec.Template.Labels = nil
	pool.Spec.Template.Spec.Taints = nil
	it := runtimeType(ctx, class, "n2-standard-8", "")
	p := runtimeProvider(class, pool, it)
	p.sourceProvider = nil
	p.bootstrapResolver = nil
	got, err := p.GetInstanceTypes(ctx, pool)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, corev1.NodeSelectorOpDoesNotExist, got[0].Requirements.Get(v1alpha1.LabelSandboxRuntime).Operator())
	pool.Spec.Template.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{{Key: v1alpha1.LabelSandboxRuntime, Operator: corev1.NodeSelectorOpIn, Values: []string{"gvisor"}}}
	_, err = p.GetInstanceTypes(ctx, pool)
	require.Error(t, err)
}

func TestCreateRetriesTransientStateReadErrorsWithoutCallingInstanceProvider(t *testing.T) {
	class, _, claim := runtimeFixtures()
	p := runtimeProvider(class, nil)
	apiError := errors.New("API temporarily unavailable")
	p.kubeClient = unavailableKubeClient{err: apiError}
	_, err := p.Create(runtimeContext(), claim)
	require.ErrorIs(t, err, apiError)
	require.False(t, corecloud.IsNodeClassNotReadyError(err), "transient API reads must not delete immutable claims")
	require.False(t, corecloud.IsInsufficientCapacityError(err))
}

func TestHistoricalInventoryRemainsVisibleWithoutLaunchableSandboxCapacity(t *testing.T) {
	for _, reason := range []string{"unsupported bootstrap", "deleting class", "changed to unmanaged ref"} {
		t.Run(reason, func(t *testing.T) {
			ctx := runtimeContext()
			class, pool, _ := runtimeFixtures()
			it := runtimeType(ctx, class, "n2-standard-8", "")
			p := runtimeProvider(class, pool, it)
			inst := &instance.Instance{Name: "historical", ProjectID: "test-project", Location: "us-central1-a", Type: it.Name,
				Labels:  map[string]string{utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool"},
				Sandbox: &metadata.SandboxIdentity{Runtime: "gvisor", Installer: "immutable", Profile: "reviewed"}}
			p.instanceProvider = &lifecycleInstances{instances: []*instance.Instance{inst}}
			p.bootstrapResolver = schedulingProfile{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown source"}}
			switch reason {
			case "deleting class":
				class.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			case "changed to unmanaged ref":
				pool.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "other", Kind: "Other", Name: "other"}
			}
			listed, err := p.List(ctx)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			got, err := p.Get(ctx, "gce://test-project/us-central1-a/historical")
			require.NoError(t, err)
			require.Equal(t, "gvisor", got.Labels[v1alpha1.LabelSandboxRuntime])
			require.Equal(t, inst.Type, got.Labels[corev1.LabelInstanceTypeStable])
			require.Equal(t, listed[0].Status.ProviderID, got.Status.ProviderID)
		})
	}
}
