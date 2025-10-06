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

package utils

import (
	"context"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

const (
	LabelNodePoolKey     string = "karpenter.sh/nodepool"
	LabelGCENodeClassKey string = "karpenter.k8s.gcp/gcenodeclass"
	LabelClusterNameKey  string = "goog-k8s-cluster-name"
)

func GetAllSingleValuedRequirementLabels(instanceType *cloudprovider.InstanceType) map[string]string {
	labels := map[string]string{}
	if instanceType == nil {
		return labels
	}
	for key, req := range instanceType.Requirements {
		if req.Len() == 1 {
			labels[key] = req.Values()[0]
		}
	}
	return labels
}

func SanitizeGCELabelValue(s string) string {
	re := regexp.MustCompile("[^a-zA-Z0-9]+")
	sanitized := re.ReplaceAllString(s, "-")

	sanitized = strings.Trim(sanitized, "-")
	return strings.ToLower(sanitized)
}

func ResolveReservedResource(instanceType string, cpuMCore, memoryMiB, bootDiskGiB, totalSSDGiB, localSSDCount int64) (int64, int64, int64, int64, int64) {
	// referring to https://cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes
	// order: cpu, memory, eviction memory, ephemeral eviction threshold, ephemeral system reservation
	evictionThreshold, systemReservation := ResolveReservedEphemeralStorage(bootDiskGiB, totalSSDGiB, localSSDCount)
	return ResolveReservedCPUMCore(instanceType, cpuMCore), ResolveReservedMemoryMiB(instanceType, memoryMiB), 100, evictionThreshold, systemReservation
}

func ResolveReservedMemoryMiB(instanceType string, memoryMiB int64) int64 {
	memory := int64(0)

	// 255 MiB of memory for machines with less than 1 GiB of memory
	// 25% of the first 4 GiB of memory
	// 20% of the next 4 GiB of memory (up to 8 GiB)
	// 10% of the next 8 GiB of memory (up to 16 GiB)
	// 6% of the next 112 GiB of memory (up to 128 GiB)
	// 2% of any memory above 128 GiB
	if memoryMiB <= 1024 {
		memory += 255
		return memory
	}

	memory += int64(math.Min(float64(memoryMiB), 4096) * 0.25)
	memoryMiB -= 4096
	if memoryMiB <= 0 {
		return memory
	}

	memory += int64(math.Min(float64(memoryMiB), 4096) * 0.20)
	memoryMiB -= 4096
	if memoryMiB <= 0 {
		return memory
	}

	memory += int64(math.Min(float64(memoryMiB), 8192) * 0.10)
	memoryMiB -= 8192
	if memoryMiB <= 0 {
		return memory
	}

	memory += int64(math.Min(float64(memoryMiB), 114688) * 0.06)
	memoryMiB -= 114688
	if memoryMiB <= 0 {
		return memory
	}

	memory += int64(float64(memoryMiB) * 0.02)
	return memory
}

func ResolveReservedCPUMCore(instanceType string, cpuMCore int64) int64 {
	cpu := int64(0)

	isSharedE2 := instanceType == "e2-micro" ||
		instanceType == "e2-small" ||
		instanceType == "e2-medium"
	if isSharedE2 {
		// For shared-core E2 machine types, GKE reserves a total of 1060 millicores.
		return 1060
	} else {
		// 6% of the first core
		// 1% of the next core (up to 2 cores)
		// 0.5% of the next 2 cores (up to 4 cores)
		// 0.25% of any cores above 4 cores
		cpu += int64(math.Min(float64(cpuMCore), 1000) * 0.06)
		cpuMCore -= 1000
		if cpuMCore <= 0 {
			return cpu
		}

		cpu += int64(math.Min(float64(cpuMCore), 1000) * 0.01)
		cpuMCore -= 1000
		if cpuMCore <= 0 {
			return cpu
		}

		cpu += int64(math.Min(float64(cpuMCore), 2000) * 0.005)
		cpuMCore -= 2000
		if cpuMCore <= 0 {
			return cpu
		}

		cpu += int64(float64(cpuMCore) * 0.0025)
	}

	return cpu
}

func ResolveReservedEphemeralStorage(bootDiskGiB, totalSSDGiB, localSSDCount int64) (int64, int64) {
	// referring to https://cloud.google.com/kubernetes-engine/docs/concepts/plan-node-sizes
	// order: cpu, memory, eviction memory, ephemeral eviction threshold, ephemeral system reservation
	var evictionThreshold, systemReservation int64

	if localSSDCount > 0 && totalSSDGiB > 0 {
		// Ephemeral storage backed by local SSDs
		// Eviction threshold is 10% of total SSD capacity
		evictionThreshold = int64(float64(totalSSDGiB) * 0.10)

		// System reservation based on number of SSDs
		switch {
		case localSSDCount == 1:
			systemReservation = 50
		case localSSDCount == 2:
			systemReservation = 75
		default: // 3 or more
			systemReservation = 100
		}
		return evictionThreshold, systemReservation
	}
	// Ephemeral storage backed by boot disk
	// Eviction threshold is 10% of boot disk capacity
	evictionThreshold = int64(float64(bootDiskGiB) * 0.10)

	// System reservation is minimum of:
	// - 50% of boot disk capacity
	// - 35% of boot disk capacity + 6 GiB
	// - 100 GiB
	option1 := int64(float64(bootDiskGiB) * 0.50)
	option2 := int64(float64(bootDiskGiB)*0.35) + 6
	option3 := int64(100)

	systemReservation = option1
	if option2 < systemReservation {
		systemReservation = option2
	}
	if option3 < systemReservation {
		systemReservation = option3
	}

	return evictionThreshold, systemReservation
}

// WithDefaultFloat64 returns the float64 value of the supplied environment variable or, if not present,
// the supplied default value. If the float64 conversion fails, returns the default
func WithDefaultFloat64(key string, def float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}

func ResolveNodePoolFromNodeClaim(ctx context.Context, kubeClient client.Client, nodeClaim *karpv1.NodeClaim) (*karpv1.NodePool, error) {
	if nodePoolName, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]; ok {
		nodePool := &karpv1.NodePool{}
		if err := kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
			return nil, err
		}
		return nodePool, nil
	}
	// There will be no nodePool referenced inside the nodeClaim in case of standalone nodeClaims
	return nil, nil
}
