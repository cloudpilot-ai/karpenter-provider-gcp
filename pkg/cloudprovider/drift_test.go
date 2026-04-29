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
	"testing"

	"github.com/stretchr/testify/require"
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

func nodeClass(hash, hashVersion string) *v1alpha1.GCENodeClass {
	nc := &v1alpha1.GCENodeClass{}
	if hash != "" || hashVersion != "" {
		nc.Annotations = make(map[string]string)
	}
	if hash != "" {
		nc.Annotations[v1alpha1.AnnotationGCENodeClassHash] = hash
	}
	if hashVersion != "" {
		nc.Annotations[v1alpha1.AnnotationGCENodeClassHashVersion] = hashVersion
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
			nodeClass: nodeClass("abc123", v1alpha1.GCENodeClassHashVersion),
			nodeClaim: nodeClaim("abc123"),
			want:      "",
		},
		{
			name:      "NodeClassDrift when hashes differ",
			nodeClass: nodeClass("abc123", v1alpha1.GCENodeClassHashVersion),
			nodeClaim: nodeClaim("def456"),
			want:      NodeClassDrift,
		},
		{
			name:      "no drift when NodeClass hash annotation absent",
			nodeClass: nodeClass("", v1alpha1.GCENodeClassHashVersion),
			nodeClaim: nodeClaim("abc123"),
			want:      "",
		},
		{
			name:      "no drift when NodeClaim hash annotation absent",
			nodeClass: nodeClass("abc123", v1alpha1.GCENodeClassHashVersion),
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
			nodeClass: nodeClass("abc123", v1alpha1.GCENodeClassHashVersion),
			nodeClaim: &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{v1alpha1.AnnotationGCENodeClassHash: "abc123"}}},
			want:      "",
		},
		{
			name:      "no drift when hash versions mismatch",
			nodeClass: nodeClass("abc123", "v99"),
			nodeClaim: nodeClaim("abc123"),
			want:      "",
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

func TestIsImageDrifted(t *testing.T) {
	t.Parallel()
	cp := &CloudProvider{}

	nodeClaimWithImage := func(imageID string) *karpv1.NodeClaim {
		nc := &karpv1.NodeClaim{}
		nc.Status.ImageID = imageID
		return nc
	}

	nodeClassWithImages := func(sources ...string) *v1alpha1.GCENodeClass {
		nc := &v1alpha1.GCENodeClass{}
		for _, s := range sources {
			nc.Status.Images = append(nc.Status.Images, v1alpha1.Image{SourceImage: s})
		}
		return nc
	}

	tests := []struct {
		name      string
		nodeClass *v1alpha1.GCENodeClass
		nodeClaim *karpv1.NodeClaim
		want      karpcloudprovider.DriftReason
	}{
		{
			name:      "no drift when ImageID suffix matches SourceImage",
			nodeClaim: nodeClaimWithImage("projects/gke-node-images/global/images/cos-113-123"),
			nodeClass: nodeClassWithImages("cos-113-123"),
			want:      "",
		},
		{
			name:      "no drift when one of multiple images matches",
			nodeClaim: nodeClaimWithImage("projects/gke-node-images/global/images/cos-113-123"),
			nodeClass: nodeClassWithImages("ubuntu-2004-456", "cos-113-123"),
			want:      "",
		},
		{
			name:      "ImageDrift when no image matches",
			nodeClaim: nodeClaimWithImage("projects/gke-node-images/global/images/cos-113-999"),
			nodeClass: nodeClassWithImages("cos-113-123"),
			want:      ImageDrift,
		},
		{
			name:      "ImageDrift when Status.Images is empty",
			nodeClaim: nodeClaimWithImage("projects/gke-node-images/global/images/cos-113-123"),
			nodeClass: &v1alpha1.GCENodeClass{},
			want:      ImageDrift,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cp.isImageDrifted(tt.nodeClaim, tt.nodeClass)
			require.Equal(t, tt.want, got)
		})
	}
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
				ImageFamily: &cos,
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
			name: "ShieldedInstanceConfig",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.ShieldedInstanceConfig = &v1alpha1.ShieldedInstanceConfig{EnableSecureBoot: ptr(true)}
			},
			want: NodeClassDrift,
		},
		{
			name: "NetworkConfig",
			mutate: func(nc *v1alpha1.GCENodeClass) {
				nc.Spec.NetworkConfig = &v1alpha1.NetworkConfig{
					NetworkInterfaces: []v1alpha1.NetworkInterface{{Subnetwork: "regions/us-central1/subnetworks/custom"}},
				}
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
