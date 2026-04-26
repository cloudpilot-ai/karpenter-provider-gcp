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
	"reflect"
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

// setNonZeroPrimitive handles scalar kinds for setNonZero.
func setNonZeroPrimitive(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("non-zero")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	}
}

// setNonZero sets v to a non-zero value of its type. Used by
// TestGCENodeClassHashCoversAllMutableFields to verify every GCENodeClassSpec
// field participates in the hash without manually enumerating each field.
func setNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		setNonZero(v.Elem())
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		setNonZero(elem)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		key := reflect.New(v.Type().Key()).Elem()
		setNonZero(key)
		val := reflect.New(v.Type().Elem()).Elem()
		setNonZero(val)
		v.SetMapIndex(key, val)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				setNonZero(v.Field(i))
				return
			}
		}
	default:
		setNonZeroPrimitive(v)
	}
}

// TestGCENodeClassHashCoversAllMutableFields uses reflection to iterate every
// exported GCENodeClassSpec field. For each field it verifies that setting a
// non-zero value produces a different hash (fields tagged hash:"ignore" must
// NOT change the hash). New fields added to GCENodeClassSpec are tested
// automatically without requiring manual updates here.
func TestGCENodeClassHashCoversAllMutableFields(t *testing.T) {
	t.Parallel()

	baseline := func() *v1alpha1.GCENodeClass {
		return &v1alpha1.GCENodeClass{
			Spec: v1alpha1.GCENodeClassSpec{
				ServiceAccount: "sa@project.iam.gserviceaccount.com",
				ImageSelectorTerms: []v1alpha1.ImageSelectorTerm{
					{Alias: "ContainerOptimizedOS@latest"},
				},
				ImageFamily: ptr(v1alpha1.ImageFamilyContainerOptimizedOS),
			},
		}
	}

	specType := reflect.TypeOf(v1alpha1.GCENodeClassSpec{})
	for i := 0; i < specType.NumField(); i++ {
		field := specType.Field(i)
		idx := i
		isIgnored := field.Tag.Get("hash") == "ignore"

		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()

			before := baseline()
			after := baseline()
			setNonZero(reflect.ValueOf(&after.Spec).Elem().Field(idx))

			if isIgnored {
				require.Equal(t, before.Hash(), after.Hash(),
					"field %s is hash:ignore — mutating it must NOT change the hash", field.Name)
			} else {
				require.NotEqual(t, before.Hash(), after.Hash(),
					"field %s must change the hash when mutated", field.Name)
			}
		})
	}
}
