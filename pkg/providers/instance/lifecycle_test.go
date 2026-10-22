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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/metadata"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/nodepooltemplate"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/sandbox"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

func adoptionFixtures() (*v1alpha1.GCENodeClass, *karpv1.NodeClaim, *compute.Instance, *compute.Metadata) {
	class := sandboxNodeClass()
	class.Name = "sandbox"
	claim := onDemandNodeClaim()
	claim.Name = "retry"
	claim.Labels = map[string]string{karpv1.NodePoolLabelKey: "pool"}
	claim.Spec.NodeClassRef = &karpv1.NodeClassReference{Group: "karpenter.k8s.gcp", Kind: "GCENodeClass", Name: class.Name}
	claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "karpenter.sh/v1", Kind: "NodePool", Name: "pool", UID: "pool-uid"}}
	vm := &compute.Instance{Name: "karpenter-retry", Zone: "zones/us-central1-a", MachineType: makeNonGPUIT().Name,
		Labels: map[string]string{metadata.SandboxRuntimeInstanceLabel: "gvisor", utils.SanitizeGCELabelValue(utils.LabelGCENodeClassKey): "sandbox", utils.SanitizeGCELabelValue(utils.LabelNodePoolKey): "pool"},
		Metadata: computeMetadataValues(map[string]string{
			metadata.SandboxRuntimeMetadataKey: "gvisor", metadata.SandboxInstallerMetadataKey: "gke.gcr.io/gke-gvisor-installer@sha256:" + strings.Repeat("a", 64),
			metadata.SandboxProfileMetadataKey:  "gke-gvisor-20260323/native-xemu-smt-nftables-v1/amd64-core-tags",
			metadata.SandboxClassUIDMetadataKey: "sandbox-class-uid", metadata.SandboxClassHashMetadataKey: v1alpha1.GCENodeClassHashVersion + "/" + class.Hash(), metadata.SandboxPoolUIDMetadataKey: "pool-uid",
		}), Disks: []*compute.AttachedDisk{{Boot: true, InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: sandboxCPUImage}}},
	}
	source := computeMetadataValues(map[string]string{metadata.KubeEnvKey: strings.ReplaceAll(requiredSourceKubeEnv, "v1.30.1-gke.123", "v1.35.6-gke.1250001")})
	return class, claim, vm, source
}

func setVMMetadata(vm *compute.Instance, key, value string) {
	for _, item := range vm.Metadata.Items {
		if item.Key == key {
			item.Value = lo.ToPtr(value)
			return
		}
	}
	vm.Metadata.Items = append(vm.Metadata.Items, &compute.MetadataItems{Key: key, Value: lo.ToPtr(value)})
}

// Each change protects a distinct false-adoption or stale retry boundary.
//
//nolint:gocyclo
func TestSandboxAdoptionValidatesDurableFactsAndLaunchBindings(t *testing.T) {
	for _, scenario := range []string{"genuine retry", "ordinary VM", "disabled class", "wrong installer", "changed policy", "changed hardware", "recreated class", "changed spec", "changed class ref", "changed pool label", "changed pool owner", "missing owner", "foreign owner", "duplicate owner", "missing class UID", "empty pool UID", "nil identity value", "duplicate identity key", "label disagreement", "label-only identity", "unselected image", "unknown source", "missing source field", "OR reorder", "canonical image URL", "foreign image URL"} {
		t.Run(scenario, func(t *testing.T) {
			class, claim, vm, source := adoptionFixtures()
			p := &DefaultProvider{bootstrapResolver: fixedBootstrap{}}
			switch scenario {
			case "ordinary VM":
				vm.Metadata = nil
				vm.Labels = nil
			case "disabled class":
				class.Spec.SandboxConfig = nil
			case "wrong installer":
				setVMMetadata(vm, metadata.SandboxInstallerMetadataKey, "other@sha256:"+strings.Repeat("b", 64))
			case "changed policy":
				setVMMetadata(vm, metadata.SandboxProfileMetadataKey, "gke-gvisor-20260323/old-policy/amd64-core-tags")
			case "changed hardware":
				setVMMetadata(vm, metadata.SandboxProfileMetadataKey, "gke-gvisor-20260323/native-xemu-smt-nftables-v1/amd64-amd")
			case "recreated class":
				class.UID = "new-class-uid"
			case "changed spec":
				class.Spec.Labels = map[string]string{"change": "true"}
			case "changed class ref":
				claim.Spec.NodeClassRef.Name = "other"
			case "changed pool label":
				vm.Labels[utils.SanitizeGCELabelValue(utils.LabelNodePoolKey)] = "other"
			case "changed pool owner":
				claim.OwnerReferences[0].UID = "new-pool-uid"
			case "missing owner":
				claim.OwnerReferences = nil
			case "foreign owner":
				claim.OwnerReferences[0].APIVersion = "other.example/v1"
			case "duplicate owner":
				claim.OwnerReferences = append(claim.OwnerReferences, claim.OwnerReferences[0])
			case "missing class UID":
				setVMMetadata(vm, metadata.SandboxClassUIDMetadataKey, "")
			case "empty pool UID":
				setVMMetadata(vm, metadata.SandboxPoolUIDMetadataKey, "")
			case "nil identity value":
				vm.Metadata.Items[0].Value = nil
			case "duplicate identity key":
				vm.Metadata.Items = append(vm.Metadata.Items, vm.Metadata.Items[0])
			case "label disagreement":
				vm.Labels[metadata.SandboxRuntimeInstanceLabel] = "other"
			case "label-only identity":
				vm.Metadata = nil
			case "canonical image URL":
				vm.Disks[0].InitializeParams.SourceImage = "https://www.googleapis.com/compute/v1/" + sandboxCPUImage
			case "foreign image URL":
				vm.Disks[0].InitializeParams.SourceImage = "https://untrusted.example/" + sandboxCPUImage
			case "unselected image":
				vm.Disks[0].InitializeParams.SourceImage = "projects/other/global/images/copied"
			case "unknown source":
				p.bootstrapResolver = fixedBootstrap{err: &sandbox.SandboxBootstrapUnsupported{Reason: "unknown consumers"}}
			case "missing source field":
				source = computeMetadataValues(map[string]string{metadata.KubeEnvKey: "KUBE_MANIFESTS_TAR_URL: synthetic"})
			case "OR reorder":
				first := *class.Status.Images[0].DeepCopy()
				first.SourceImage = strings.Replace(sandboxCPUImage, "395-109", "395-110", 1)
				class.Status.Images = append([]v1alpha1.Image{first}, class.Status.Images...)
			}
			err := p.validateAdoption(context.Background(), vm, class, claim, []*cloudprovider.InstanceType{makeNonGPUIT()}, source)
			if scenario == "genuine retry" || scenario == "OR reorder" || scenario == "canonical image URL" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.False(t, cloudprovider.IsInsufficientCapacityError(err))
		})
	}
}

func TestLegacyOrdinaryAdoptionRemainsCompatibleButRejectsOwnedKeyPresence(t *testing.T) {
	p := &DefaultProvider{}
	claim := onDemandNodeClaim()
	require.NoError(t, p.validateAdoption(context.Background(), &compute.Instance{}, &v1alpha1.GCENodeClass{}, claim, nil, nil))
	for _, key := range []string{metadata.SandboxRuntimeMetadataKey, metadata.SandboxInstallerMetadataKey, metadata.SandboxProfileMetadataKey, metadata.SandboxClassUIDMetadataKey, metadata.SandboxClassHashMetadataKey, metadata.SandboxPoolUIDMetadataKey} {
		vm := &compute.Instance{Metadata: &compute.Metadata{Items: []*compute.MetadataItems{{Key: key}}}}
		require.NotNil(t, sandboxIdentityFromVM(vm))
		require.Error(t, p.validateAdoption(context.Background(), vm, &v1alpha1.GCENodeClass{}, claim, nil, nil))
	}
	vm := &compute.Instance{Labels: map[string]string{metadata.SandboxRuntimeInstanceLabel: ""}}
	require.Error(t, p.validateAdoption(context.Background(), vm, &v1alpha1.GCENodeClass{}, claim, nil, nil))
}

type adoptionSource struct {
	nodepooltemplate.Provider
	source *compute.Metadata
}

func (s adoptionSource) GetSourceTemplateMetadata(context.Context) (*compute.Metadata, error) {
	return s.source, nil
}

func TestCreateAndCrossZoneRetryValidateAdoptionWithoutInsert(t *testing.T) {
	for _, boundary := range []string{"regional lookup", "zonal retry"} {
		for _, valid := range []bool{true, false} {
			t.Run(boundary+map[bool]string{true: " genuine retry", false: " stale installer"}[valid], func(t *testing.T) {
				class, claim, vm, source := adoptionFixtures()
				if !valid {
					setVMMetadata(vm, metadata.SandboxInstallerMetadataKey, "stale")
				}
				p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, http.MethodGet, r.Method, "adoption must not create or mutate a VM")
					if strings.HasSuffix(r.URL.Path, "/aggregated/instances") {
						writeJSON(w, &compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{"zones/us-central1-a": {Instances: []*compute.Instance{vm}}}})
						return
					}
					writeJSON(w, vm)
				}))
				p.bootstrapResolver = fixedBootstrap{}
				p.nodePoolTemplateProvider = adoptionSource{source: source}
				var err error
				if boundary == "regional lookup" {
					adopted, e := p.Create(context.Background(), class, claim, []*cloudprovider.InstanceType{makeNonGPUIT()})
					err = e
					if valid {
						require.NotNil(t, adopted)
						require.Equal(t, "pool-uid", adopted.Sandbox.PoolUID)
					}
				} else {
					existing, zone, retryable, e := p.getOrCreateInstance(context.Background(), claim, class, makeNonGPUIT(), source, nil, "us-central1-a", karpv1.CapacityTypeOnDemand, nil, nil, 0, nil)
					require.False(t, retryable)
					err = e
					if valid {
						require.NotNil(t, existing)
						require.Equal(t, "us-central1-a", zone)
					}
				}
				if valid {
					require.NoError(t, err)
					return
				}
				require.Error(t, err)
				require.True(t, cloudprovider.IsNodeClassNotReadyError(err), "impossible adoption must not retry forever")
				require.False(t, cloudprovider.IsInsufficientCapacityError(err))
			})
		}
	}
}

// Each adapter case changes the GCE boot disk shape or lookup trust boundary.
//
//nolint:gocyclo
func TestSandboxBootDiskAdapterAndHistoricalInventory(t *testing.T) {
	for _, scenario := range []string{"GCE disk resource", "lookup fails", "foreign project", "foreign zone", "untrusted URL", "nonboot disk first", "only data disk", "ambiguous boot disks", "disk has no source image"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, vm, _ := adoptionFixtures()
			vm.Disks = []*compute.AttachedDisk{{Boot: true, Source: "https://www.googleapis.com/compute/v1/projects/test-project/zones/us-central1-a/disks/boot"}}
			switch scenario {
			case "foreign project":
				vm.Disks[0].Source = strings.Replace(vm.Disks[0].Source, "test-project", "foreign-project", 1)
			case "foreign zone":
				vm.Disks[0].Source = strings.Replace(vm.Disks[0].Source, "us-central1-a", "us-central1-b", 1)
			case "untrusted URL":
				vm.Disks[0].Source = strings.Replace(vm.Disks[0].Source, "www.googleapis.com", "untrusted.example", 1)
			case "only data disk":
				vm.Disks = []*compute.AttachedDisk{{InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: "wrong-data-image"}}}
			case "ambiguous boot disks":
				vm.Disks = append(vm.Disks, vm.Disks[0])
			case "nonboot disk first":
				vm.Disks = append([]*compute.AttachedDisk{{Source: "untrusted data disk", InitializeParams: &compute.AttachedDiskInitializeParams{SourceImage: "wrong"}}}, vm.Disks...)
			}
			diskReads := 0
			p := newFakeComputeProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				switch {
				case strings.Contains(r.URL.Path, "/regions/"):
					writeJSON(w, &compute.Region{Zones: []string{"zones/us-central1-a"}})
				case strings.Contains(r.URL.Path, "/disks/"):
					diskReads++
					require.True(t, strings.HasSuffix(r.URL.Path, "/zones/us-central1-a/disks/boot"))
					if scenario == "lookup fails" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					if scenario == "disk has no source image" {
						writeJSON(w, &compute.Disk{})
						return
					}
					writeJSON(w, &compute.Disk{SourceImage: sandboxCPUImage})
				default:
					writeJSON(w, &compute.InstanceList{Items: []*compute.Instance{vm}})
				}
			}))
			p.region = "us-central1"
			listed, err := p.List(context.Background())
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.NotNil(t, listed[0].Sandbox)
			cached, err := p.Get(context.Background(), "gce://test-project/us-central1-a/karpenter-retry")
			require.NoError(t, err)
			require.Equal(t, listed[0].Sandbox, cached.Sandbox)
			if scenario == "GCE disk resource" || scenario == "nonboot disk first" {
				require.Equal(t, sandboxCPUImage, cached.ImageID)
				require.Equal(t, 1, diskReads)
			} else {
				require.Empty(t, cached.ImageID)
				if scenario != "lookup fails" && scenario != "disk has no source image" {
					require.Zero(t, diskReads)
				}
			}
		})
	}
}

func TestNewSandboxVMLaunchBindsClassAndGenuinePoolOwner(t *testing.T) {
	class, claim, _, source := adoptionFixtures()
	p := makeProvider()
	p.bootstrapResolver = fixedBootstrap{}
	vm, err := p.buildInstance(context.Background(), claim, class, makeNonGPUIT(), source, makeCluster("projects/p/global/networks/vpc", "regions/us-central1/subnetworks/subnet", "pods", false), "us-central1-a", "karpenter-retry", karpv1.CapacityTypeOnDemand, nil, 0, "pods")
	require.NoError(t, err)
	actual := sandboxIdentityFromVM(vm)
	require.Equal(t, "sandbox-class-uid", actual.ClassUID)
	require.Equal(t, v1alpha1.GCENodeClassHashVersion+"/"+class.Hash(), actual.ClassHash)
	require.Equal(t, "pool-uid", actual.PoolUID)
	require.False(t, actual.Malformed)
	require.NoError(t, p.validateAdoption(context.Background(), vm, class, claim, []*cloudprovider.InstanceType{makeNonGPUIT()}, source))
}
