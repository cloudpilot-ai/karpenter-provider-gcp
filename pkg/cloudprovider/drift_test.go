/*
Copyright 2025 The CloudPilot AI Authors.

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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// ptr returns a pointer to the given value (generic helper for tests).
func ptr[T any](v T) *T { return &v }

// nodeClaim returns a NodeClaim with the given hash annotation. When hash is
// non-empty, the current GCENodeClassHashVersion is also set. Callers that
// need to test with an absent version annotation should construct the
// NodeClaim inline.
func nodeClaim(hash string) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{}
	if hash != "" {
		nc.Annotations = map[string]string{
			v1alpha1.AnnotationGCENodeClassHash:        hash,
			v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion,
		}
	}
	return nc
}

func nodeClass(hash string) *v1alpha1.GCENodeClass {
	nc := &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		v1alpha1.AnnotationGCENodeClassHashVersion: v1alpha1.GCENodeClassHashVersion,
	}}}
	if hash != "" {
		nc.Annotations[v1alpha1.AnnotationGCENodeClassHash] = hash
	}
	return nc
}

func TestAreStaticFieldsDrifted(t *testing.T) {
	t.Parallel()
	cp := &CloudProvider{}

	tests := []struct {
		name      string
		nodeClass *v1alpha1.GCENodeClass
		nodeClaim *karpv1.NodeClaim
		want      karpcloudprovider.DriftReason
	}{
		{
			name:      "no drift when hashes match",
			nodeClass: nodeClass("abc123"),
			nodeClaim: nodeClaim("abc123"),
			want:      "",
		},
		{
			name:      "NodeClassDrift when hashes differ",
			nodeClass: nodeClass("abc123"),
			nodeClaim: nodeClaim("def456"),
			want:      NodeClassDrift,
		},
		{
			name:      "no drift when NodeClass hash annotation absent",
			nodeClass: nodeClass(""),
			nodeClaim: nodeClaim("abc123"),
			want:      "",
		},
		{
			name:      "no drift when NodeClaim hash annotation absent",
			nodeClass: nodeClass("abc123"),
			nodeClaim: nodeClaim(""),
			want:      "",
		},
		{
			name:      "no drift when NodeClass version annotation absent",
			nodeClass: &v1alpha1.GCENodeClass{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1alpha1.AnnotationGCENodeClassHash: "abc123"}}},
			nodeClaim: nodeClaim("abc123"),
			want:      "",
		},
		{
			name:      "no drift when NodeClaim version annotation absent",
			nodeClass: nodeClass("abc123"),
			nodeClaim: &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1alpha1.AnnotationGCENodeClassHash: "abc123"}}},
			want:      "",
		},
		{
			name:      "no drift when NodeClaim has old hash version",
			nodeClass: nodeClass("abc123"),
			nodeClaim: &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				v1alpha1.AnnotationGCENodeClassHash:        "def456",
				v1alpha1.AnnotationGCENodeClassHashVersion: "v4",
			}}},
			want: "",
		},
		{
			name:      "no drift when all annotations absent",
			nodeClass: &v1alpha1.GCENodeClass{},
			nodeClaim: &karpv1.NodeClaim{},
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cp.areStaticFieldsDrifted(tt.nodeClaim, tt.nodeClass)
			require.Equal(t, tt.want, got)
		})
	}
}

func imageWithRequirements(source string, requirements ...corev1.NodeSelectorRequirement) v1alpha1.Image {
	return v1alpha1.Image{SourceImage: source, Requirements: requirements}
}

func cpuImageRequirements(arch string) []corev1.NodeSelectorRequirement {
	return []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{arch}},
		{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpDoesNotExist},
	}
}

func gpuImageRequirements() []corev1.NodeSelectorRequirement {
	return []corev1.NodeSelectorRequirement{
		{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}},
		{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpExists},
	}
}

// TestIsImageDrifted protects image-only drift: a retained node only drifts when the published
// images contain a usable replacement for the variant it actually runs.
func TestIsImageDrifted(t *testing.T) {
	t.Parallel()
	cp := &CloudProvider{}

	const (
		amd64Image    = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-pre"
		newAMD64Image = "projects/gke-node-images/global/images/gke-1351-gke1396005-cos-125-19216-105-127-c-pre"
		arm64Image    = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-arm64-125-19216-104-126-c-pre"
		newARM64Image = "projects/gke-node-images/global/images/gke-1351-gke1396005-cos-arm64-125-19216-105-127-c-pre"
		nvidiaImage   = "projects/gke-node-images/global/images/gke-1351-gke1396004-cos-125-19216-104-126-c-nvda"
	)

	amd64CPUImage := imageWithRequirements(amd64Image, cpuImageRequirements("amd64")...)
	newAMD64CPUImage := imageWithRequirements(newAMD64Image, cpuImageRequirements("amd64")...)
	arm64CPUImage := imageWithRequirements(arm64Image, cpuImageRequirements("arm64")...)
	newARM64CPUImage := imageWithRequirements(newARM64Image, cpuImageRequirements("arm64")...)
	nvidiaGPUImage := imageWithRequirements(nvidiaImage, gpuImageRequirements()...)

	claim := func(imageID string, labels map[string]string, specRequirements ...karpv1.NodeSelectorRequirementWithMinValues) *karpv1.NodeClaim {
		nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
		nc.Status.ImageID = imageID
		nc.Spec.Requirements = specRequirements
		return nc
	}
	archRequirement := func(arches ...string) karpv1.NodeSelectorRequirementWithMinValues {
		return karpv1.NodeSelectorRequirementWithMinValues{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: arches}
	}
	class := func(images ...v1alpha1.Image) *v1alpha1.GCENodeClass {
		nc := &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: images}}
		nc.StatusConditions().SetTrue(v1alpha1.ConditionTypeImagesReady)
		return nc
	}

	tests := []struct {
		name      string
		nodeClaim *karpv1.NodeClaim
		nodeClass *v1alpha1.GCENodeClass
		want      karpcloudprovider.DriftReason
	}{
		{
			name:      "no drift when the node still runs a published image",
			nodeClaim: claim(amd64Image, map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: class(amd64CPUImage, arm64CPUImage),
			want:      "",
		},
		{
			name:      "drift when the amd64 image the node runs was replaced",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: class(newAMD64CPUImage),
			want:      ImageDrift,
		},
		{
			// Deeper insight: a newer amd64 build must not rotate every arm64 node.
			name:      "no drift for a retained arm64 node when only amd64 advanced",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "arm64"}),
			nodeClass: class(newAMD64CPUImage),
			want:      "",
		},
		{
			name:      "drift when a usable arm64 replacement is published",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "arm64"}),
			nodeClass: class(newAMD64CPUImage, newARM64CPUImage),
			want:      ImageDrift,
		},
		{
			name:      "no drift while images are not ready",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: func() *v1alpha1.GCENodeClass {
				nc := class(newAMD64CPUImage)
				nc.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageUnavailable", "publication in progress")
				return nc
			}(),
			want: "",
		},
		{
			name:      "no drift when image readiness is unknown",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: &v1alpha1.GCENodeClass{Status: v1alpha1.GCENodeClassStatus{Images: []v1alpha1.Image{newAMD64CPUImage}}},
			want:      "",
		},
		{
			name:      "no drift when no image is published",
			nodeClaim: claim(amd64Image, map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: class(),
			want:      "",
		},
		{
			// Deeper insight: the arm64 and NVIDIA siblings are published independently, so a GPU
			// node has no usable replacement while only CPU images exist.
			name:      "no drift for a GPU node without a GPU image",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-nvda", map[string]string{corev1.LabelArchStable: "amd64", v1alpha1.LabelInstanceGPUCount: "1"}),
			nodeClass: class(newAMD64CPUImage, newARM64CPUImage),
			want:      "",
		},
		{
			name:      "drift for a GPU node once its GPU image is replaced",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-nvda", map[string]string{corev1.LabelArchStable: "amd64", v1alpha1.LabelInstanceGPUCount: "1"}),
			nodeClass: class(newAMD64CPUImage, nvidiaGPUImage),
			want:      ImageDrift,
		},
		{
			// Deeper insight: a CPU node's labels carry no GPU count, which must not be read as
			// "any GPU shape" and drift the node onto a GPU-only image.
			name:      "no drift for a CPU node toward a GPU-only image",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "amd64"}),
			nodeClass: class(nvidiaGPUImage),
			want:      "",
		},
		{
			// Deeper insight: a broad GPU spec requirement admits a GPU shape, but the labels still
			// prove this is a CPU node, so the GPU-only image is not a replacement.
			name: "broad GPU spec requirement cannot override the actual CPU node",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre",
				map[string]string{corev1.LabelArchStable: "amd64"},
				karpv1.NodeSelectorRequirementWithMinValues{Key: v1alpha1.LabelInstanceGPUCount, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"8"}}),
			nodeClass: class(nvidiaGPUImage),
			want:      "",
		},
		{
			name:      "no drift when only the spec requirements name another architecture",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", nil, archRequirement("arm64")),
			nodeClass: class(newAMD64CPUImage),
			want:      "",
		},
		{
			name:      "drift when the spec requirements are the only architecture source and match",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", nil, archRequirement("arm64")),
			nodeClass: class(newARM64CPUImage),
			want:      ImageDrift,
		},
		{
			name:      "broad spec requirements cannot override the actual arm64 node",
			nodeClaim: claim("projects/gke-node-images/global/images/gke-1351-gke1390000-cos-arm64-125-19216-100-100-c-pre", map[string]string{corev1.LabelArchStable: "arm64"}, archRequirement("amd64", "arm64")),
			nodeClass: class(newAMD64CPUImage),
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, cp.isImageDrifted(tt.nodeClaim, tt.nodeClass))
		})
	}
}

// TestStaticDriftIsReportedWhileImagesAreNotReady protects the boundary of the image-only guard.
func TestStaticDriftIsReportedWhileImagesAreNotReady(t *testing.T) {
	t.Parallel()
	cp := &CloudProvider{}

	nodeClass := nodeClass("newhash")
	nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeImagesReady, "ImageUnavailable", "publication in progress")
	claim := nodeClaim("oldhash")
	claim.Status.ImageID = "projects/gke-node-images/global/images/gke-1351-gke1390000-cos-125-19216-100-100-c-pre"

	require.Equal(t, NodeClassDrift, cp.isNodeClassDrifted(context.Background(), claim, nil, nodeClass))
}

// TestNodeClassDriftFieldCoverage verifies that each GCENodeClassSpec field either
// triggers NodeClassDrift (hashed fields) or produces no drift (hash:"ignore" fields).
// One row per field — when per-field drift reasons are added in the future (e.g.
// ServiceAccountDrift, NetworkTagsDrift), update the want column for that row.
func TestNodeClassDriftFieldCoverage(t *testing.T) {
	t.Parallel()
	cp := &CloudProvider{}

	// baseline returns a GCENodeClass with all required fields populated.
	// ImageFamily is set explicitly so that ImageSelectorTerms changes do not
	// affect the hash via in.ImageFamily().
	baseline := func() *v1alpha1.GCENodeClass {
		cos := v1alpha1.ImageFamilyContainerOptimizedOS
		return &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				ServiceAccount: "sa@project.iam.gserviceaccount.com",
				ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{
					{Alias: "ContainerOptimizedOS@latest"},
				},
				ImageFamily:  &cos,
				LocalSsdMode: v1alpha1.LocalSSDModeRawBlock,
			},
		}
	}

	// stampHash stamps both the NodeClaim (old hash) and NodeClass (new hash)
	// with the current hash version so that areStaticFieldsDrifted can compare them.
	stampHash := func(nc *karpv1.NodeClaim, class *v1alpha1.GCENodeClass) {
		h := class.Hash()
		if class.Annotations == nil {
			class.Annotations = make(map[string]string)
		}
		class.Annotations[v1alpha1.AnnotationGCENodeClassHash] = h
		class.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = v1alpha1.GCENodeClassHashVersion
		if nc.Annotations == nil {
			nc.Annotations = make(map[string]string)
		}
		nc.Annotations[v1alpha1.AnnotationGCENodeClassHash] = h
		nc.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = v1alpha1.GCENodeClassHashVersion
	}

	tests := []struct {
		name   string
		mutate func(*v1alpha1.GCENodeClass)
		want   karpcloudprovider.DriftReason
	}{
		{
			name:   "ServiceAccount",
			mutate: func(nc *v1alpha1.GCENodeClass) { nc.Spec.ServiceAccount = "other@project.iam.gserviceaccount.com" },
			want:   NodeClassDrift,
		},
		{
			name: "Disks",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.Disks = append(nc.Spec.Disks, v1alpha1.Disk{SizeGiB: 100, Boot: true})
			},
			want: NodeClassDrift,
		},
		{
			// ImageSelectorTerms is tagged hash:"ignore". With an explicit ImageFamily set,
			// changing ImageSelectorTerms also does not affect in.ImageFamily(), so no drift.
			name: "ImageSelectorTerms (hash:ignore)",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.ImageSelectorTerms = append(nc.Spec.ImageSelectorTerms, v1alpha1.ImageSelectorTerm{ID: "extra-image-id"})
			},
			want: "",
		},
		{
			name: "ImageFamily",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				ubuntu := v1alpha1.ImageFamilyUbuntu
				nc.Spec.ImageFamily = &ubuntu
			},
			want: NodeClassDrift,
		},
		{
			name: "KubeletConfiguration",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				maxPods := int32(50)
				nc.Spec.KubeletConfiguration = &v1alpha1.KubeletConfiguration{MaxPods: &maxPods}
			},
			want: NodeClassDrift,
		},
		{
			name:   "Labels",
			mutate: func(nc *v1alpha1.GCENodeClass) { nc.Spec.Labels = map[string]string{"env": "test"} },
			want:   NodeClassDrift,
		},
		{
			name:   "Metadata",
			mutate: func(nc *v1alpha1.GCENodeClass) { nc.Spec.Metadata = map[string]string{"key": "value"} },
			want:   NodeClassDrift,
		},
		{
			name: "NetworkTags",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.NetworkTags = append(nc.Spec.NetworkTags, "drift-tag")
			},
			want: NodeClassDrift,
		},
		{
			name: "ResourceManagerTags",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.ResourceManagerTags = map[string]v1alpha1.ResourceManagerTagValue{"tagKeys/123": "tagValues/456"}
			},
			want: NodeClassDrift,
		},
		{
			name: "ShieldedInstanceConfig",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.ShieldedInstanceConfig = &v1alpha1.ShieldedInstanceConfig{EnableSecureBoot: ptr(true)}
			},
			want: NodeClassDrift,
		},
		{
			name: "AdvancedMachineFeatures",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.AdvancedMachineFeatures = &v1alpha1.AdvancedMachineFeatures{EnableNestedVirtualization: ptr(true)}
			},
			want: NodeClassDrift,
		},
		{
			name: "NetworkConfig",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.NetworkConfig = &v1alpha1.NetworkConfig{
					Subnetwork: "regions/us-central1/subnetworks/custom",
				}
			},
			want: NodeClassDrift,
		},
		{
			name: "SubnetRangeNames",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.SubnetRangeNames = []string{"pods-a", "pods-b"}
			},
			want: NodeClassDrift,
		},
		{
			name: "LocalSsdMode",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.LocalSsdMode = v1alpha1.LocalSSDModeEphemeral
			},
			want: NodeClassDrift,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := baseline()
			nc := &karpv1.NodeClaim{}
			stampHash(nc, before)

			after := baseline()
			tt.mutate(after)
			if after.Annotations == nil {
				after.Annotations = make(map[string]string)
			}
			after.Annotations[v1alpha1.AnnotationGCENodeClassHash] = after.Hash()
			after.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = v1alpha1.GCENodeClassHashVersion

			got := cp.areStaticFieldsDrifted(nc, after)
			require.Equal(t, tt.want, got, "field %q: unexpected drift reason", tt.name)
		})
	}
}
