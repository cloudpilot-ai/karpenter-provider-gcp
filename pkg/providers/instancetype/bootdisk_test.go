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

package instancetype

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

func swapEnabledNodeClass(disks ...v1alpha1.Disk) *v1alpha1.GCENodeClass {
	return &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		Disks:           disks,
		LinuxNodeConfig: &v1alpha1.LinuxNodeConfig{SwapConfig: &v1alpha1.SwapConfig{Enabled: true}},
	}}
}

func TestResolveBootDiskSwapCreatesDefaultBootDisk(t *testing.T) {
	for name, tc := range map[string]struct {
		instanceType string
		wantType     v1alpha1.DiskCategory
	}{
		"pd-standard where supported":                    {instanceType: "n2-standard-4", wantType: "pd-standard"},
		"pd-balanced when pd-standard is not":            {instanceType: "c3-standard-4", wantType: "pd-balanced"},
		"hyperdisk-balanced for hyperdisk-only families": {instanceType: "n4-standard-4", wantType: "hyperdisk-balanced"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveBootDiskSwap(swapEnabledNodeClass(), tc.instanceType, 16384)
			require.NoError(t, err)
			require.NotNil(t, got.DefaultBootDisk, "a swap-enabled node class without a boot disk gets one from the provider")
			require.True(t, got.DefaultBootDisk.Boot)
			require.Equal(t, int32(100), got.DefaultBootDisk.SizeGiB)
			require.Equal(t, tc.wantType, got.DefaultBootDisk.Category)
			require.Equal(t, int64(100), got.BootDiskGiB)
			require.Equal(t, int64(24), got.SwapGiB, "16GiB of memory doubles to 24GiB on a 100GiB disk")
		})
	}
}

func TestResolveBootDiskSwapRejectsUnknownDiskCompatibility(t *testing.T) {
	_, err := ResolveBootDiskSwap(swapEnabledNodeClass(), "madeup-standard-4", 16384)
	require.ErrorContains(t, err, "no disk type compatibility is known for machine family \"madeup\"")
}

func TestResolveBootDiskSwapKeepsExplicitBootDisk(t *testing.T) {
	nodeClass := swapEnabledNodeClass(v1alpha1.Disk{Boot: true, SizeGiB: 40, Category: "pd-ssd"})

	got, err := ResolveBootDiskSwap(nodeClass, "n2-standard-4", 2048)
	require.NoError(t, err)
	require.Nil(t, got.DefaultBootDisk, "the node class boot disk is rendered unchanged")
	require.Equal(t, int64(40), got.BootDiskGiB)
	require.Equal(t, int64(4), got.SwapGiB)

	// 30GiB of memory doubles to 60GiB, which the 30GiB disk caps at 15GiB.
	nodeClass = swapEnabledNodeClass(v1alpha1.Disk{Boot: true, SizeGiB: 30})
	got, err = ResolveBootDiskSwap(nodeClass, "n2-standard-4", 30720)
	require.NoError(t, err)
	require.Equal(t, int64(15), got.SwapGiB, "the half-disk limit applies to the explicit boot disk")
}

func TestResolveBootDiskSwapRejectsAmbiguousBootDisks(t *testing.T) {
	nodeClass := swapEnabledNodeClass(
		v1alpha1.Disk{Boot: true, SizeGiB: 40},
		v1alpha1.Disk{Boot: true, SizeGiB: 60},
	)
	_, err := ResolveBootDiskSwap(nodeClass, "n2-standard-4", 2048)
	require.ErrorContains(t, err, "requires exactly one boot disk")

	nodeClass = swapEnabledNodeClass(v1alpha1.Disk{Boot: true})
	_, err = ResolveBootDiskSwap(nodeClass, "n2-standard-4", 2048)
	require.ErrorContains(t, err, "requires a boot disk with an explicit sizeGiB")
}

func TestResolveBootDiskSwapRejectsOversizeRequest(t *testing.T) {
	nodeClass := swapEnabledNodeClass(v1alpha1.Disk{Boot: true, SizeGiB: 100})
	nodeClass.Spec.LinuxNodeConfig.SwapConfig.BootDiskProfile = &v1alpha1.SwapBootDiskProfile{SwapSizeGib: lo.ToPtr(int64(60))}

	_, err := ResolveBootDiskSwap(nodeClass, "n2-standard-4", 16384)
	require.ErrorContains(t, err, "more than half of the 100GiB boot disk")
}

func TestResolveBootDiskSwapWithoutSwapKeepsExistingDisks(t *testing.T) {
	nodeClass := &v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		Disks: []v1alpha1.Disk{{Boot: true, SizeGiB: 60}},
	}}
	got, err := ResolveBootDiskSwap(nodeClass, "n2-standard-4", 16384)
	require.NoError(t, err)
	require.Nil(t, got.DefaultBootDisk)
	require.Equal(t, int64(60), got.BootDiskGiB)
	require.Zero(t, got.SwapGiB)

	got, err = ResolveBootDiskSwap(&v1alpha1.GCENodeClass{}, "n2-standard-4", 16384)
	require.NoError(t, err)
	require.Nil(t, got.DefaultBootDisk, "nodes without swap keep launching with only the disks the node class configures")
	require.Equal(t, DefaultBootDiskGiB, got.BootDiskGiB, "the estimate keeps its 100GiB fallback")

	// Without swap a boot disk whose size GCE resolves is still accepted.
	got, err = ResolveBootDiskSwap(&v1alpha1.GCENodeClass{Spec: v1alpha1.GCENodeClassSpec{
		Disks: []v1alpha1.Disk{{Boot: true}},
	}}, "n2-standard-4", 16384)
	require.NoError(t, err)
	require.Equal(t, DefaultBootDiskGiB, got.BootDiskGiB)
}
