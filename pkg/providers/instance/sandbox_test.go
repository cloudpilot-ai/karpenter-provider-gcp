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
	"net/http"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/imagefamily"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/offerings/unavailableofferings"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const sandboxCPUImage = "projects/gke-node-images/global/images/gke-1356-gke1250001-cos-125-19216-395-109-c-pre"
const sandboxGPUImage = "projects/gke-node-images/global/images/gke-1356-gke1250001-cos-125-19216-220-150-pre-gv-nvda"

type fixedBootstrap struct {
	err     error
	release string
}

func (r fixedBootstrap) ResolveBootstrap(context.Context, string, string) (sandbox.BootstrapProfile, error) {
	if r.err != nil {
		return sandbox.BootstrapProfile{}, r.err
	}
	release := r.release
	if release == "" {
		release = "v1.35.6-gke.1250001"
	}
	return sandbox.BootstrapProfile{Release: release, Schema: "gke-gvisor-20260323", Policy: "native-xemu-smt-nftables-v1"}, nil
}

func sandboxNodeClass() *v1alpha1.GCENodeClass {
	nc := bootDiskNodeClass("pd-balanced", nil, nil)
	nc.UID = "sandbox-class-uid"
	nc.Spec.SandboxConfig = &v1alpha1.SandboxConfig{Type: "GVISOR"}
	nc.Spec.ImageSelectorTerms = []v1alpha1.ImageSelectorTerm{{Family: v1alpha1.ImageFamilyContainerOptimizedOS, Channel: "cluster"}}
	nc.Status.Images = []v1alpha1.Image{{SourceImage: sandboxCPUImage, Requirements: []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
		{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpDoesNotExist},
	}, SandboxInstaller: &v1alpha1.SandboxInstaller{Version: "20260323.0_RC03", Digest: "sha256:" + strings.Repeat("a", 64)}}}
	return nc
}

func TestBuildInstanceNativeSandboxProfilesBindActualImageAndDurableIdentity(t *testing.T) {
	for _, profile := range []struct {
		name, arch, variant string
		gpu                 bool
	}{
		{"n2-standard-2", "amd64", "amd64-core-tags", false}, {"e2-standard-2", "amd64", "amd64-core-tags", false},
		{"c4-standard-2", "amd64", "amd64-core-tags", false}, {"n2d-standard-2", "amd64", "amd64-amd", false},
		{"t2a-standard-2", "arm64", "arm64", false}, {"g2-standard-4", "amd64", "amd64-core-tags", true}, {"a2-highgpu-1g", "amd64", "amd64-core-tags", true},
	} {
		t.Run(profile.name, func(t *testing.T) {
			p := makeProvider()
			p.bootstrapResolver = fixedBootstrap{}
			nc := sandboxNodeClass()
			it := makeNonGPUIT()
			it.Name = profile.name
			it.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
			delete(it.Requirements, corev1.LabelArchStable)
			it.Requirements.Add(scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, profile.arch))
			actualImage := nc.Status.Images[0]
			if profile.arch == "arm64" {
				actualImage.SourceImage = strings.Replace(actualImage.SourceImage, "-cos-", "-cos-arm64-", 1)
				actualImage.Requirements[0].Values = []string{"arm64"}
			}
			if profile.gpu {
				actualImage.SourceImage = sandboxGPUImage
				actualImage.Requirements[1].Operator = corev1.NodeSelectorOpExists
				model := "nvidia-l4"
				if strings.HasPrefix(profile.name, "a2-") {
					model = "nvidia-tesla-a100"
				}
				delete(it.Requirements, v1alpha1.LabelInstanceGPUCount)
				it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, "1"), scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpIn, model))
				it.Capacity[v1alpha1.ResourceNVIDIAGPU] = resource.MustParse("1")
			}
			// An incompatible first image must not donate its installer to the selected one.
			wrong := actualImage
			wrong.Requirements = []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"other"}}}
			wrong.SandboxInstaller = &v1alpha1.SandboxInstaller{Version: "wrong", Digest: "sha256:" + strings.Repeat("b", 64)}
			nc.Status.Images = []v1alpha1.Image{wrong, actualImage}
			source := strings.ReplaceAll(requiredSourceKubeEnv, "v1.30.1-gke.123", "v1.35.6-gke.1250001")
			source = strings.ReplaceAll(source, "linux-amd64", "linux-"+profile.arch)
			vm, err := p.buildInstance(context.Background(), onDemandNodeClaim(), nc, it, computeMetadataValues(map[string]string{metadata.KubeEnvKey: source + "GVISOR_CORE_TAGS: \"true\"\n"}), makeCluster("projects/p/global/networks/vpc", "regions/us-central1/subnetworks/subnet", "pods", false), "us-central1-a", "sandbox-test", karpv1.CapacityTypeOnDemand, nil, 0, "pods")
			require.NoError(t, err)
			require.Equal(t, actualImage.SourceImage, vm.Disks[0].InitializeParams.SourceImage)
			require.Equal(t, "gvisor", vm.Labels[metadata.SandboxRuntimeInstanceLabel])
			for _, value := range vm.Labels {
				require.LessOrEqual(t, len(value), 63)
			}
			env := kubeEnvFrom(t, vm)
			require.Contains(t, env, `GVISOR_ENABLE_SMT: "true"`)
			require.Contains(t, env, `GVISOR_PLATFORM: "xemu"`)
			require.Contains(t, env, `GVISOR_REPRODUCE_NFTABLES: "true"`)
			if profile.variant == "amd64-core-tags" {
				require.Contains(t, env, `GVISOR_CORE_TAGS: "true"`)
			} else {
				require.NotContains(t, env, "GVISOR_CORE_TAGS:")
			}
			require.Equal(t, int64(4), it.Capacity.Cpu().Value()) // no SMT capacity halving
			identity := sandboxIdentityFromVM(vm)
			require.Equal(t, &metadata.SandboxIdentity{Runtime: "gvisor", Installer: "gke.gcr.io/gke-gvisor-installer@sha256:" + strings.Repeat("a", 64), Profile: "gke-gvisor-20260323/native-xemu-smt-nftables-v1/" + profile.variant, ClassUID: "sandbox-class-uid", ClassHash: v1alpha1.GCENodeClassHashVersion + "/" + nc.Hash()}, identity)
			nc.Spec.SandboxConfig = nil
			nc.Status.Images = nil
			adopted := p.adoptExistingInstance(context.Background(), vm)
			require.Equal(t, identity, adopted.Sandbox)
		})
	}
}

func TestSandboxLaunchGateRejectsUnsupportedSourceAndImageWithoutCapacityError(t *testing.T) {
	nc := sandboxNodeClass()
	it := makeNonGPUIT()
	target := metadata.NewInstanceMetadata()
	p := &DefaultProvider{bootstrapResolver: fixedBootstrap{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown complete consumer fingerprint"}}}
	_, err := p.prepareSandbox(context.Background(), target, nc, it)
	require.True(t, sandbox.IsBootstrapUnsupported(err))
	require.False(t, cloudprovider.IsInsufficientCapacityError(err))
	p.bootstrapResolver = fixedBootstrap{}
	nc.Status.Images[0].SandboxInstaller = nil
	_, err = p.prepareSandbox(context.Background(), target, nc, it)
	var configuration *sandbox.ConfigurationError
	require.ErrorAs(t, err, &configuration)
	nc = sandboxNodeClass()
	nc.Status.Images[0].SourceImage = "projects/custom/global/images/copied-cos"
	_, err = p.prepareSandbox(context.Background(), target, nc, it)
	require.ErrorAs(t, err, &configuration)
}

func TestSandboxIdentitySurvivesInventoryAndCacheMappings(t *testing.T) {
	vm := &compute.Instance{Name: "historical", MachineType: "zones/us-central1-a/machineTypes/n2-standard-2", Labels: map[string]string{
		utils.SanitizeGCELabelValue(utils.LabelClusterNameKey): "cluster", utils.SanitizeGCELabelValue(utils.LabelClusterLocationKey): "us-central1",
	}, Metadata: computeMetadataValues(map[string]string{metadata.SandboxRuntimeMetadataKey: "gvisor", metadata.SandboxInstallerMetadataKey: "old-installer@sha256:" + strings.Repeat("c", 64), metadata.SandboxProfileMetadataKey: "old-schema/old-policy/old-hardware"})}
	p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/regions/") {
			writeJSON(w, &compute.Region{Zones: []string{"zones/us-central1-a"}})
			return
		}
		require.True(t, strings.HasSuffix(r.URL.Path, "/instances"))
		writeJSON(w, &compute.InstanceList{Items: []*compute.Instance{vm}})
	}))
	p.clusterName = "cluster"
	p.clusterLocation = "us-central1"
	instances, err := p.List(context.Background())
	require.NoError(t, err)
	require.Len(t, instances, 1)
	require.Equal(t, "old-schema/old-policy/old-hardware", instances[0].Sandbox.Profile)
	cached, err := p.Get(context.Background(), "gce://test-project/us-central1-a/historical")
	require.NoError(t, err)
	require.Equal(t, instances[0].Sandbox, cached.Sandbox)
	require.Nil(t, sandboxIdentityFromVM(&compute.Instance{}))
}

func TestSandboxReservedVMIdentityCannotBeOverridden(t *testing.T) {
	for _, key := range []string{metadata.SandboxRuntimeMetadataKey, metadata.SandboxInstallerMetadataKey, metadata.SandboxProfileMetadataKey, metadata.SandboxClassUIDMetadataKey, metadata.SandboxClassHashMetadataKey, metadata.SandboxPoolUIDMetadataKey} {
		nc := sandboxNodeClass()
		nc.Spec.Metadata = map[string]string{key: "spoof"}
		require.Error(t, sandbox.ValidateConfiguration(nc))
	}
	nc := sandboxNodeClass()
	nc.Spec.Labels = map[string]string{metadata.SandboxRuntimeInstanceLabel: "spoof"}
	require.Error(t, sandbox.ValidateConfiguration(nc))
	p := &DefaultProvider{}
	require.Error(t, p.CreateTags(context.Background(), "unused", map[string]string{metadata.SandboxRuntimeInstanceLabel: "spoof"}))
}

func TestSandboxGPUChecksEffectiveImageAndBootstrapAndFractionalCapacity(t *testing.T) {
	p := &DefaultProvider{bootstrapResolver: fixedBootstrap{release: "v1.30.11-gke.1092999"}}
	nc := sandboxNodeClass()
	nc.Status.Images[0].SourceImage = sandboxGPUImage
	nc.Status.Images[0].Requirements[1].Operator = corev1.NodeSelectorOpExists
	it := makeGPUIT()
	it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpIn, "nvidia-l4"))
	it.Capacity[v1alpha1.ResourceNVIDIAGPU] = resource.MustParse("1")
	target := metadata.NewInstanceMetadata()
	_, err := p.prepareSandbox(context.Background(), target, nc, it)
	require.ErrorContains(t, err, "1.30.11-gke.1092999")
	p.bootstrapResolver = fixedBootstrap{}
	nc.Status.Images[0].SourceImage = strings.Replace(sandboxGPUImage, "1356-gke1250001", "13011-gke1092999", 1)
	_, err = p.prepareSandbox(context.Background(), target, nc, it)
	require.ErrorContains(t, err, "1.30.11-gke.1092999")
	nc.Status.Images[0].SourceImage = sandboxGPUImage
	it.Capacity[v1alpha1.ResourceNVIDIAGPU] = resource.MustParse("0.5")
	_, err = p.prepareSandbox(context.Background(), target, nc, it)
	require.ErrorContains(t, err, "fractional")
}

func TestSandboxBootstrapFailurePreventsInsertWithoutPoisoningOffering(t *testing.T) {
	p := makeProvider()
	p.bootstrapResolver = fixedBootstrap{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown full consumer fingerprints"}}
	p.unavailableOfferings = unavailableofferings.NewUnavailableOfferings()
	nc := sandboxNodeClass()
	it := makeNonGPUIT()
	source := computeMetadataValues(map[string]string{metadata.KubeEnvKey: strings.ReplaceAll(requiredSourceKubeEnv, "v1.30.1-gke.123", "v1.35.6-gke.1250001")})
	// No compute client: reaching Instances.Insert would panic instead of returning
	// a typed configuration failure. Only the local launch preparation may run.
	_, _, retryable, err := p.createInstanceWithPodRanges(context.Background(), onDemandNodeClaim(), nc, it, source, makeCluster("projects/p/global/networks/vpc", "regions/us-central1/subnetworks/subnet", "pods", false), "us-central1-a", "sandbox-test", karpv1.CapacityTypeOnDemand, nil, 0, nil)
	require.True(t, sandbox.IsBootstrapUnsupported(err))
	require.False(t, retryable)
	require.False(t, p.unavailableOfferings.IsUnavailable(it.Name, "us-central1-a", karpv1.CapacityTypeOnDemand))
	require.False(t, cloudprovider.IsInsufficientCapacityError(err))
}

func TestSandboxIdentityPresenceIsNotCollapsedIntoLegacyAbsence(t *testing.T) {
	require.Nil(t, sandboxIdentityFromVM(&compute.Instance{}))
	require.Nil(t, sandboxIdentityFromVM(&compute.Instance{Metadata: &compute.Metadata{Items: []*compute.MetadataItems{nil, {Key: "unrelated", Value: nil}}}}))
	for _, key := range []string{metadata.SandboxRuntimeMetadataKey, metadata.SandboxInstallerMetadataKey, metadata.SandboxProfileMetadataKey, metadata.SandboxClassUIDMetadataKey, metadata.SandboxClassHashMetadataKey, metadata.SandboxPoolUIDMetadataKey} {
		for _, value := range []*string{nil, lo.ToPtr("")} {
			identity := sandboxIdentityFromVM(&compute.Instance{Metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: key, Value: value}}}})
			require.Equal(t, &metadata.SandboxIdentity{Malformed: true}, identity, key)
		}
	}
	partial := sandboxIdentityFromVM(&compute.Instance{Metadata: computeMetadataValues(map[string]string{metadata.SandboxRuntimeMetadataKey: "gvisor"})})
	require.Equal(t, &metadata.SandboxIdentity{Runtime: "gvisor"}, partial)
}

func TestBuildInstanceSelectsPolicyCompatibleORImageAndBindsInstaller(t *testing.T) {
	p := makeProvider()
	p.bootstrapResolver = fixedBootstrap{}
	nc := sandboxNodeClass()
	supported := nc.Status.Images[0]
	supported.SourceImage = sandboxGPUImage
	supported.Requirements[1].Operator = corev1.NodeSelectorOpExists
	unsupported := *supported.DeepCopy()
	unsupported.SourceImage = strings.Replace(sandboxGPUImage, "1356-gke1250001", "1340-gke1712999", 1)
	unsupported.SandboxInstaller = &v1alpha1.SandboxInstaller{Version: "20250811.0_RC05", Digest: "sha256:" + strings.Repeat("b", 64)}
	nc.Status.Images = []v1alpha1.Image{unsupported, supported}
	before := nc.DeepCopy()
	it := makeGPUIT()
	it.Name = "a3-ultragpu-8g"
	delete(it.Requirements, v1alpha1.LabelInstanceGPUCount)
	delete(it.Requirements, v1alpha1.LabelGKEAccelerator)
	it.Requirements.Add(scheduling.NewRequirement(v1alpha1.LabelInstanceGPUName, corev1.NodeSelectorOpIn, "nvidia-h200-141gb"), scheduling.NewRequirement(v1alpha1.LabelGKEAccelerator, corev1.NodeSelectorOpIn, "nvidia-h200-141gb"), scheduling.NewRequirement(v1alpha1.LabelInstanceGPUCount, corev1.NodeSelectorOpIn, "8"))
	it.Capacity[v1alpha1.ResourceNVIDIAGPU] = resource.MustParse("8")
	it.Capacity[corev1.ResourceCPU] = resource.MustParse("224")
	source := computeMetadataValues(map[string]string{metadata.KubeEnvKey: strings.ReplaceAll(requiredSourceKubeEnv, "v1.30.1-gke.123", "v1.35.6-gke.1250001")})
	vm, err := p.buildInstance(context.Background(), onDemandNodeClaim(), nc, it, source, makeCluster("projects/p/global/networks/vpc", "regions/us-central1/subnetworks/subnet", "pods", false), "us-central1-a", "sandbox-or-test", karpv1.CapacityTypeOnDemand, nil, 0, "pods")
	require.NoError(t, err)
	require.Equal(t, sandboxGPUImage, vm.Disks[0].InitializeParams.SourceImage)
	require.Equal(t, "gke.gcr.io/gke-gvisor-installer@sha256:"+strings.Repeat("a", 64), sandboxIdentityFromVM(vm).Installer)
	selected, err := imagefamily.SelectLaunchImage(nc, it, "v1.35.6-gke.1250001")
	require.NoError(t, err)
	require.Equal(t, selected.SourceImage, vm.Disks[0].InitializeParams.SourceImage)
	require.Equal(t, before.Status.Images, nc.Status.Images)
	// A usable image cannot compensate for an incompatible effective bootstrap.
	_, err = imagefamily.SelectLaunchImage(nc, it, "v1.34.0-gke.1712999")
	require.Error(t, err)
	nc.Status.Images = []v1alpha1.Image{unsupported}
	_, err = imagefamily.SelectLaunchImage(nc, it, "v1.35.6-gke.1250001")
	require.Error(t, err)
}
