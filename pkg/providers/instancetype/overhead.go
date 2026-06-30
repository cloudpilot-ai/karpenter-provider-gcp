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

package instancetype

import (
	"math"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

// Eviction signal keys recognized for overhead modeling. imagefs.available
// and pid.available are accepted by the CRD but not factored into the
// scheduler's view of allocatable — matches aws/karpenter-provider-aws.
const (
	signalMemoryAvailable = "memory.available"
	signalNodeFSAvailable = "nodefs.available"
)

// kcSystemReserved returns Overhead.SystemReserved derived from computed and
// user-provided reservations. CRD pattern validation guarantees every user
// value is a parseable resource.Quantity, so MustParse is safe here. User
// values are additive because computed reservations model provider overhead
// that exists independently of explicit kubelet systemReserved settings.
func kcSystemReserved(computed corev1.ResourceList, kc *v1alpha1.KubeletConfiguration) corev1.ResourceList {
	out := computed.DeepCopy()
	if kc == nil {
		return out
	}
	for k, v := range kc.SystemReserved {
		name := corev1.ResourceName(k)
		q := resource.MustParse(string(v))
		if existing, ok := out[name]; ok {
			existing.Add(q)
			out[name] = existing
			continue
		}
		out[name] = q
	}
	return out
}

// mergeKubeReserved returns computed with user keys overlaid. Mirrors AWS's
// lo.Assign(computed, user) semantics: user keys win per-key, computed
// sub-keys for sub-keys the user did not set survive.
func mergeKubeReserved(computed corev1.ResourceList, kc *v1alpha1.KubeletConfiguration) corev1.ResourceList {
	out := computed.DeepCopy()
	if kc == nil {
		return out
	}
	for k, v := range kc.KubeReserved {
		out[corev1.ResourceName(k)] = resource.MustParse(string(v))
	}
	return out
}

// evictionThreshold returns the eviction overhead used by the scheduler.
// Only evictionHard is factored into overhead; evictionSoft is a warning
// threshold with a grace period and is not modeled here, matching
// aws/karpenter-provider-aws and the Kubernetes node-pressure-eviction docs:
// https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#eviction-thresholds
// https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#eviction-signals
//
// Only memory.available and nodefs.available are modeled; imagefs.available
// and pid.available are out of scope for scheduler bin-packing.
func evictionThreshold(memory, storage *resource.Quantity, computed corev1.ResourceList, kc *v1alpha1.KubeletConfiguration) corev1.ResourceList {
	out := computed.DeepCopy()
	if kc == nil || len(kc.EvictionHard) == 0 {
		return out
	}
	if v, ok := kc.EvictionHard[signalMemoryAvailable]; ok {
		out[corev1.ResourceMemory] = computeEvictionSignal(*memory, string(v))
	}
	if v, ok := kc.EvictionHard[signalNodeFSAvailable]; ok {
		out[corev1.ResourceEphemeralStorage] = computeEvictionSignal(*storage, string(v))
	}
	return out
}

// computeEvictionSignal resolves an eviction signal value against a capacity.
// CRD pattern restricts inputs to either a percentage like "5%" or a
// resource.Quantity. MustParse is safe on the non-percentage branch.
func computeEvictionSignal(capacity resource.Quantity, signal string) resource.Quantity {
	if strings.HasSuffix(signal, "%") {
		pct, err := strconv.ParseFloat(strings.TrimSuffix(signal, "%"), 64)
		if err != nil {
			// CRD pattern validates this; defensive fallback to zero.
			return resource.Quantity{}
		}
		return *resource.NewQuantity(int64(math.Ceil(float64(capacity.Value())*pct/100.0)), capacity.Format)
	}
	return resource.MustParse(signal)
}

// kcMaxPods returns the effective max pods for an instance type, honoring
// nodeClass.GetMaxPods() and PodsPerCore (cap pods at podsPerCore × cpus).
// Mirrors AWS's pods() helper.
func kcMaxPods(nodeClassMaxPods int32, kc *v1alpha1.KubeletConfiguration, cpus int64) int64 {
	maxPods := int64(nodeClassMaxPods)
	if kc != nil && kc.PodsPerCore != nil && *kc.PodsPerCore > 0 {
		byCores := int64(*kc.PodsPerCore) * cpus
		if byCores < maxPods {
			return byCores
		}
	}
	return maxPods
}
