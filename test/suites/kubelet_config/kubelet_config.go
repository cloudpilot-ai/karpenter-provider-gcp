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

package kubelet_config

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

// kubeletConfigCase describes one e2e scenario: provision a node with
// kubeletConfig attached to the GCENodeClass, then assert against the
// resulting node's status. Mirrors environment.TestCase fields for the
// provisioning side; assertions live in assert.
type kubeletConfigCase struct {
	tc              environment.TestCase
	kubeletConfig   map[string]any
	linuxNodeConfig map[string]any
	// omitBootDisk omits spec.disks so the provider has to create the boot disk
	// a swap file needs.
	omitBootDisk bool
	// bootDiskGiB overrides the per-family test boot disk size.
	bootDiskGiB int64
	assert      func(node *corev1.Node)
	// assertOnNode, when set, runs after assert and may create workloads on
	// the node.
	assertOnNode func(ctx context.Context, node *corev1.Node)
	wantShutdown *shutdownPeriods
}

type shutdownPeriods struct {
	total, critical time.Duration
}

var env *environment.Environment
var _ = BeforeEach(func() { env = environment.Current() })

var _ = DescribeTable("kubeletConfiguration honored on provisioned nodes (#398)", Label("suite:kubelet_config"),
	func(ctx SpecContext, c kubeletConfigCase) {
		runKubeletConfigTest(ctx, c)
	},

	// systemReserved reduces allocatable: the literal assertion from
	// issue #398. n2-standard-4 has 4 vCPU / 16 GiB. With systemReserved
	// cpu=1 / memory=1Gi, the kubelet must report allocatable < capacity
	// by at least those amounts.
	Entry("COS: systemReserved reduces allocatable", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"systemReserved": map[string]any{
				"cpu":    "1",
				"memory": "1Gi",
			},
		},
		assert: assertSystemReservedReducesAllocatable,
	}, SpecTimeout(15*time.Minute)),

	Entry("Ubuntu: systemReserved reduces allocatable", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		},
		kubeletConfig: map[string]any{
			"systemReserved": map[string]any{
				"cpu":    "1",
				"memory": "1Gi",
			},
		},
		assert: assertSystemReservedReducesAllocatable,
	}, SpecTimeout(15*time.Minute)),

	// Partial kubeReserved override: issue #220 regression guard at the
	// integration level. The user sets only cpu; the kubelet must still
	// receive memory and ephemeral-storage from provider-computed
	// defaults so the node comes up Ready.
	Entry("COS: partial kubeReserved override keeps node Ready (#220)", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"kubeReserved": map[string]any{
				"cpu": "500m",
			},
		},
		assert: assertKubeReservedCPUOverride,
	}, SpecTimeout(15*time.Minute)),

	// podsPerCore caps capacity[pods]. n2-standard-4 = 4 vCPU; with
	// podsPerCore=8 expect exactly 32.
	Entry("COS: podsPerCore caps capacity[pods]", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"podsPerCore": int64(8),
		},
		assert: assertPodsPerCoreCapsCapacity(32),
	}, SpecTimeout(15*time.Minute)),

	// maxPods is reflected in capacity (scheduler / kubelet agreement,
	// issue #133). maxPods=50 is a value clearly distinct from common
	// defaults, so checking node.status.capacity[pods] proves the setting
	// was propagated to kubelet.
	Entry("COS: maxPods reflected in capacity (#133)", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: map[string]any{
			"maxPods": int64(50),
		},
		assert: assertMaxPodsCapacity(50),
	}, SpecTimeout(15*time.Minute)),

	Entry("COS: on-demand shutdown defaults", shutdownConfigCase(karpv1.CapacityTypeOnDemand, nil, 0, 0), SpecTimeout(15*time.Minute)),
	Entry("COS: Spot shutdown defaults", shutdownConfigCase(karpv1.CapacityTypeSpot, nil, 30*time.Second, 15*time.Second), SpecTimeout(15*time.Minute)),
	Entry("COS: on-demand shutdown overrides", shutdownConfigCase(karpv1.CapacityTypeOnDemand, map[string]any{
		"shutdownGracePeriod": "40s", "shutdownGracePeriodCriticalPods": "10s",
	}, 40*time.Second, 10*time.Second), SpecTimeout(15*time.Minute)),
	Entry("COS: Spot shutdown overrides", shutdownConfigCase(karpv1.CapacityTypeSpot, map[string]any{
		"shutdownGracePeriod": "20s", "shutdownGracePeriodCriticalPods": "5s",
	}, 20*time.Second, 5*time.Second), SpecTimeout(15*time.Minute)),
	Entry("COS: Spot shorter total clamps critical shutdown default", shutdownConfigCase(karpv1.CapacityTypeSpot, map[string]any{
		"shutdownGracePeriod": "10s",
	}, 10*time.Second, 10*time.Second), SpecTimeout(15*time.Minute)),
	Entry("COS: Spot zero disables shutdown grace periods", shutdownConfigCase(karpv1.CapacityTypeSpot, map[string]any{
		"shutdownGracePeriod": "0s",
	}, 0, 0), SpecTimeout(15*time.Minute)),
	// Boot-disk swap: the GKE bootstrap creates the swap file before the
	// kubelet starts, and the kubelet then reports it as active swap with swap
	// enabled for pods. COS and Ubuntu each cover encryption on and off; the
	// Ubuntu unencrypted case also carries an explicit swap size and an
	// explicit kubeReserved value.
	Entry("COS: boot-disk swap materializes the default boot disk", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		// No spec.disks and no spec.kubeletConfiguration: the provider has to
		// supply both the 100 GiB boot disk and the kubelet swap defaults.
		omitBootDisk:    true,
		linuxNodeConfig: map[string]any{"swapConfig": map[string]any{"enabled": true}},
		// A 16 GiB machine gets the automatic 24 GiB swap file.
		assertOnNode: swapNodeAssertion{encrypted: true, wantSwapGiB: 24, wantBootDiskGiB: 100}.assert,
	}, SpecTimeout(15*time.Minute)),

	Entry("COS: boot-disk swap without encryption", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		bootDiskGiB: 100,
		linuxNodeConfig: map[string]any{"swapConfig": map[string]any{
			"enabled":          true,
			"encryptionConfig": map[string]any{"disabled": true},
		}},
		assertOnNode: swapNodeAssertion{encrypted: false, wantSwapGiB: 24, wantBootDiskGiB: 100}.assert,
	}, SpecTimeout(15*time.Minute)),

	Entry("Ubuntu: boot-disk swap defaults", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		},
		bootDiskGiB:     50,
		linuxNodeConfig: map[string]any{"swapConfig": map[string]any{"enabled": true}},
		assertOnNode:    swapNodeAssertion{encrypted: true, wantSwapGiB: 24, wantBootDiskGiB: 50}.assert,
	}, SpecTimeout(15*time.Minute)),

	Entry("Ubuntu: boot-disk swap with explicit size and reservation", kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-4"},
			ImageFamily:   gcpv1alpha1.ImageFamilyUbuntu,
		},
		bootDiskGiB: 100,
		kubeletConfig: map[string]any{
			"kubeReserved": map[string]any{"ephemeral-storage": "20Gi"},
		},
		linuxNodeConfig: map[string]any{"swapConfig": map[string]any{
			"enabled":          true,
			"encryptionConfig": map[string]any{"disabled": true},
			"bootDiskProfile":  map[string]any{"swapSizeGib": int64(4)},
		}},
		// The explicit 20Gi reservation plus the 4Gi swap file must survive the
		// node class overlay as one 24Gi reservation.
		assertOnNode: swapNodeAssertion{encrypted: false, wantSwapGiB: 4, wantBootDiskGiB: 100, wantKubeReserved: "24Gi"}.assert,
	}, SpecTimeout(15*time.Minute)),
)

func shutdownConfigCase(capacityType string, config map[string]any, total, critical time.Duration) kubeletConfigCase {
	return kubeletConfigCase{
		tc: environment.TestCase{
			CapacityType:  capacityType,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2"},
			ImageFamily:   gcpv1alpha1.ImageFamilyContainerOptimizedOS,
		},
		kubeletConfig: config,
		wantShutdown:  &shutdownPeriods{total: total, critical: critical},
	}
}

// runKubeletConfigTest provisions a node with the given kubeletConfig and
// invokes c.assert against the resulting Node. Mirrors
// provisioning_test.runProvisioningTest but uses
// CreateNodeClassWithKubeletConfig.
func runKubeletConfigTest(ctx context.Context, c kubeletConfigCase) {
	prefix := environment.TestPrefix(c.tc.Arch, c.tc.CapacityType, osSlug(c.tc.ImageFamily), "kubeletconfig")
	suffix := environment.UniqueSuffix()
	name := prefix + "-" + suffix

	GinkgoWriter.Printf("[setup] arch=%s capacityType=%s os=%s nodePool=%s kubeletConfig=%v\n",
		c.tc.Arch, c.tc.CapacityType, c.tc.ImageFamily, name, c.kubeletConfig)

	initialNodes := env.AllNodeNames(ctx)

	var provisionedNodeName string
	DeferCleanup(func(ctx context.Context) {
		env.DeleteDeployment(ctx, name)
		env.DeleteNodePool(ctx, name)
		env.DeleteNodeClass(ctx, name)
		if provisionedNodeName != "" {
			Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
		}
	})

	imageFamily := c.tc.ImageFamily
	if imageFamily == "" {
		imageFamily = gcpv1alpha1.ImageFamilyContainerOptimizedOS
	}
	bootDiskGiB := int64(environment.DefaultE2EDiskGiB)
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		bootDiskGiB = 50
	}
	if c.bootDiskGiB != 0 {
		bootDiskGiB = c.bootDiskGiB
	}
	if c.omitBootDisk {
		bootDiskGiB = 0
	}
	env.CreateNodeClassWithKubeletConfig(ctx, name, imageFamily, bootDiskGiB, c.kubeletConfig, c.linuxNodeConfig)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePool(ctx, name, name, c.tc)
	env.WaitForNodePoolReady(ctx, name)
	env.CreateDeployment(ctx, name, name, name, c.tc.Arch)

	env.WaitForNodeClaimLaunched(ctx, name)
	pod := env.WaitForRunningPod(ctx, name)
	Expect(pod.Spec.NodeName).NotTo(BeEmpty())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	provisionedNodeName = node.Name

	_, existedBefore := initialNodes[node.Name]
	Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
	Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)
	Expect(c.tc.InstanceTypes).To(ContainElement(node.Labels[corev1.LabelInstanceTypeStable]))

	if c.assert != nil {
		c.assert(node)
	}
	if c.wantShutdown != nil {
		assertShutdownConfig(ctx, node.Name, *c.wantShutdown)
	}
	if c.assertOnNode != nil {
		c.assertOnNode(ctx, node)
	}
}

// assertShutdownConfig reads the running kubelet, not the instance metadata:
// GKE bootstrap must deliver the settings all the way to the effective config.
func assertShutdownConfig(ctx context.Context, nodeName string, want shutdownPeriods) {
	var config struct {
		ShutdownGracePeriod              *metav1.Duration `json:"shutdownGracePeriod"`
		ShutdownGracePeriodCriticalPods  *metav1.Duration `json:"shutdownGracePeriodCriticalPods"`
		ShutdownGracePeriodByPodPriority []any            `json:"shutdownGracePeriodByPodPriority"`
		FeatureGates                     map[string]bool  `json:"featureGates"`
	}
	Expect(json.Unmarshal(kubeletConfigz(ctx, nodeName), &config)).To(Succeed())
	Expect(config.ShutdownGracePeriod).NotTo(BeNil(), "configz must include shutdownGracePeriod")
	Expect(config.ShutdownGracePeriodCriticalPods).NotTo(BeNil(), "configz must include shutdownGracePeriodCriticalPods")
	Expect(config.ShutdownGracePeriod.Duration).To(Equal(want.total))
	Expect(config.ShutdownGracePeriodCriticalPods.Duration).To(Equal(want.critical))
	Expect(config.ShutdownGracePeriodByPodPriority).To(BeEmpty())
	if want.total > 0 {
		Expect(config.FeatureGates).To(HaveKeyWithValue("GracefulNodeShutdown", true))
	}
}

// assertSystemReservedReducesAllocatable verifies that the user-configured
// systemReserved (cpu=1, memory=1Gi) is reflected in node.status.allocatable.
// Memory is asserted leniently: allocatable.memory < capacity.memory - 1Gi,
// because eviction and provider-computed kubeReserved also reduce
// allocatable. CPU is asserted strictly: allocatable.cpu ≤ capacity.cpu - 1.
func assertSystemReservedReducesAllocatable(node *corev1.Node) {
	capCPU := node.Status.Capacity[corev1.ResourceCPU]
	allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
	oneCPU := resource.MustParse("1")
	// expected upper bound on allocatable: capacity - 1 CPU
	capMinusOne := capCPU.DeepCopy()
	capMinusOne.Sub(oneCPU)
	Expect(allocCPU.Cmp(capMinusOne) <= 0).To(BeTrue(),
		"allocatable cpu %s must be <= capacity %s - 1 CPU (= %s) when systemReserved.cpu=1",
		allocCPU.String(), capCPU.String(), capMinusOne.String())

	capMem := node.Status.Capacity[corev1.ResourceMemory]
	allocMem := node.Status.Allocatable[corev1.ResourceMemory]
	oneGi := resource.MustParse("1Gi")
	capMinusOneGi := capMem.DeepCopy()
	capMinusOneGi.Sub(oneGi)
	Expect(allocMem.Cmp(capMinusOneGi) < 0).To(BeTrue(),
		"allocatable memory %s must be strictly less than capacity %s - 1Gi (= %s) when systemReserved.memory=1Gi",
		allocMem.String(), capMem.String(), capMinusOneGi.String())
}

// assertKubeReservedCPUOverride verifies that a partial kubeReserved (only
// cpu=500m set by the user) keeps the node healthy and reduces allocatable
// cpu by at least 500m. The "node is Ready" check inside runKubeletConfigTest
// is the #220 integration regression guard — proves the kubelet did not
// receive a partial kubeReserved that crashed startup.
func assertKubeReservedCPUOverride(node *corev1.Node) {
	capCPU := node.Status.Capacity[corev1.ResourceCPU]
	allocCPU := node.Status.Allocatable[corev1.ResourceCPU]
	halfCPU := resource.MustParse("500m")
	capMinusHalf := capCPU.DeepCopy()
	capMinusHalf.Sub(halfCPU)
	Expect(allocCPU.Cmp(capMinusHalf) <= 0).To(BeTrue(),
		"allocatable cpu %s must be <= capacity %s - 500m (= %s) when kubeReserved.cpu=500m",
		allocCPU.String(), capCPU.String(), capMinusHalf.String())
}

// assertPodsPerCoreCapsCapacity returns an assertion that
// node.status.capacity[pods] equals the given count.
func assertPodsPerCoreCapsCapacity(want int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		pods := node.Status.Capacity[corev1.ResourcePods]
		Expect(pods.Value()).To(Equal(want),
			"capacity.pods %s must equal podsPerCore × cpus", pods.String())
	}
}

// assertMaxPodsCapacity returns an assertion that node.status.capacity[pods]
// equals the user-configured maxPods. Pinning the value is the #133 regression
// guard against kubelet / scheduler disagreement.
func assertMaxPodsCapacity(want int64) func(*corev1.Node) {
	return func(node *corev1.Node) {
		pods := node.Status.Capacity[corev1.ResourcePods]
		Expect(pods.Value()).To(Equal(want),
			"capacity.pods %s must equal kubeletConfiguration.maxPods", pods.String())
	}
}

// assertBootDiskSize checks the size of the boot disk GCE reports for the node.
func assertBootDiskSize(ctx context.Context, node *corev1.Node, sizeGiB int64) {
	disk, err := env.GetGCEBootDisk(ctx, node.Spec.ProviderID)
	Expect(err).NotTo(HaveOccurred())
	Expect(disk.SizeGb).To(Equal(sizeGiB), "boot disk of node %s", node.Name)
}

// kubeletConfigz returns the running kubelet's effective configuration. Reading
// it through the node proxy proves the bootstrap metadata reached the kubelet.
func kubeletConfigz(ctx context.Context, nodeName string) json.RawMessage {
	requestCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	raw, err := env.KubeClient.CoreV1().RESTClient().Get().
		Resource("nodes").Name(nodeName).SubResource("proxy").Suffix("configz").
		DoRaw(requestCtx)
	Expect(err).NotTo(HaveOccurred(), "reading kubelet configz requires node proxy access")

	var response struct {
		KubeletConfig *json.RawMessage `json:"kubeletconfig"`
	}
	Expect(json.Unmarshal(raw, &response)).To(Succeed())
	Expect(response.KubeletConfig).NotTo(BeNil(), "configz must include kubeletconfig")
	return *response.KubeletConfig
}

// assertKubeletSwapDefaults checks the running kubelet's effective swap policy.
func assertKubeletSwapDefaults(config *kubeletconfig.KubeletConfiguration) {
	Expect(config.FailSwapOn).NotTo(BeNil(), "configz must include failSwapOn")
	Expect(*config.FailSwapOn).To(BeFalse(), "the kubelet refuses to start with swap active unless failSwapOn is false")
	Expect(config.MemorySwap.SwapBehavior).To(Equal("LimitedSwap"), "the kubelet must enable pod swap")
}

// swapProbeImage is the shell image Kubernetes' own e2e tests use for node
// probes. It only reads host files: the probe is unprivileged and read-only.
const swapProbeImage = "registry.k8s.io/e2e-test-images/busybox:1.37.0-2"

//go:embed testdata/swap_probe.sh
var swapProbeScript string

// swapNodeAssertion is the set of checks every boot-disk swap node must pass:
// the effective kubelet defaults, the active swap backing device, the storage
// reservation the node actually applies, and optionally the boot disk size.
type swapNodeAssertion struct {
	// encrypted requires the active swap to be the dm-crypt device named
	// encswap. Otherwise the active swap must be GKE's plain swap file and no
	// encswap device may exist.
	encrypted bool
	// wantSwapGiB checks the active entry in the host kernel's /proc/swaps.
	wantSwapGiB int64
	// wantBootDiskGiB, when non-zero, checks the size of the boot disk GCE
	// reports for the node.
	wantBootDiskGiB int64
	// wantKubeReserved, when set, checks the serialized ephemeral-storage
	// kubeReserved value.
	wantKubeReserved string
}

func (a swapNodeAssertion) assert(ctx context.Context, node *corev1.Node) {
	var config kubeletconfig.KubeletConfiguration
	Expect(json.Unmarshal(kubeletConfigz(ctx, node.Name), &config)).To(Succeed())
	assertKubeletSwapDefaults(&config)
	assertSwapBacking(ctx, node, a.encrypted, a.wantSwapGiB)
	current, err := env.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	assertNodeStorageReservation(current, &config, a.wantKubeReserved)
	if a.wantBootDiskGiB > 0 {
		assertBootDiskSize(ctx, node, a.wantBootDiskGiB)
	}
}

// assertSwapBacking runs a short node-bound probe that reads the node's
// /proc/swaps and device-mapper names, and succeeds only when the swap the node
// runs is backed by the expected device. A /dev/mapper link existing is not
// enough: it does not show that the device is the active swap, nor that a node
// with encryption disabled is actually unencrypted.
func assertSwapBacking(ctx context.Context, node *corev1.Node, encrypted bool, sizeGiB int64) {
	name := node.Name + "-swap-probe"

	volumes := []corev1.Volume{
		hostFileVolume("proc-swaps", "/proc/swaps"),
		// /sys/block/dm-* are relative symlinks into ../devices. Mount the
		// target directory so those links cannot escape the read-only mount.
		hostDirectoryVolume("sys-block", "/sys/devices/virtual/block"),
	}
	mounts := []corev1.VolumeMount{
		{Name: "proc-swaps", MountPath: "/host/proc/swaps", ReadOnly: true},
		{Name: "sys-block", MountPath: "/host/sys/block", ReadOnly: true},
	}

	if !encrypted {
		volumes = append(volumes, hostDirectoryVolume("swap-dir", "/mnt/stateful_partition/swap"))
		mounts = append(mounts, corev1.VolumeMount{Name: "swap-dir", MountPath: "/host/swap", ReadOnly: true})
	}

	runAsRoot, noGrace := int64(0), int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: environment.TestNamespace,
			Labels:    map[string]string{environment.E2EOwnerLabel: "true"},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node.Name,
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &noGrace,
			// The node carries the pool and unregistered taints while bootstrap runs.
			Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			SecurityContext: &corev1.PodSecurityContext{
				// Reading the stateful partition and device-mapper metadata needs
				// the node's root user, but not privileges or extra capabilities.
				RunAsUser: &runAsRoot,
			},
			Containers: []corev1.Container{{
				Name:    "probe",
				Image:   swapProbeImage,
				Command: []string{"sh", "-c", swapProbeScript, "swap-probe", strconv.FormatBool(encrypted), strconv.FormatInt(sizeGiB*1024*1024, 10)},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: lo.ToPtr(false),
					ReadOnlyRootFilesystem:   lo.ToPtr(true),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: mounts,
			}},
			Volumes: volumes,
		},
	}

	pods := env.KubeClient.CoreV1().Pods(environment.TestNamespace)
	_, err := pods.Create(ctx, pod, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating swap probe pod %s", name)
	DeferCleanup(func(ctx context.Context) {
		_ = pods.Delete(ctx, name, metav1.DeleteOptions{})
	})

	Eventually(func(g Gomega) corev1.PodPhase {
		current, err := pods.Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		return current.Status.Phase
	}).WithContext(ctx).WithTimeout(5 * time.Minute).WithPolling(environment.DefaultPollInterval).
		Should(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)))

	current, err := pods.Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	if current.Status.Phase == corev1.PodSucceeded {
		return
	}
	logs, logErr := pods.GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if logErr != nil {
		logs = []byte(logErr.Error())
	}
	Fail(fmt.Sprintf("swap backing probe on node %s failed (encrypted=%t):\n%s", node.Name, encrypted, logs))
}

func hostFileVolume(name, path string) corev1.Volume {
	file := corev1.HostPathFile
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &file},
	}}
}

func hostDirectoryVolume(name, path string) corev1.Volume {
	directory := corev1.HostPathDirectory
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
		HostPath: &corev1.HostPathVolumeSource{Path: path, Type: &directory},
	}}
}

// assertNodeStorageReservation checks that the node does not advertise the space
// the swap file and the configured storage reservations take. It combines the
// reservation the kubelet reports with the node's own capacity and allocatable,
// so it does not assume the nominal disk size: a filesystem is smaller than the
// disk that holds it.
func assertNodeStorageReservation(node *corev1.Node, config *kubeletconfig.KubeletConfiguration, wantKubeReserved string) {
	kubeReserved := kubeletEphemeralQuantity(config.KubeReserved)
	if wantKubeReserved != "" {
		expected := resource.MustParse(wantKubeReserved)
		Expect(kubeReserved.Value()).To(Equal(expected.Value()),
			"kubeReserved.ephemeral-storage of node %s must keep the swap file", node.Name)
	}
	systemReserved := kubeletEphemeralQuantity(config.SystemReserved)

	capacity := node.Status.Capacity[corev1.ResourceEphemeralStorage]
	allocatable := node.Status.Allocatable[corev1.ResourceEphemeralStorage]
	Expect(capacity.Value()).To(BeNumerically(">", 0), "node %s reports no ephemeral storage capacity", node.Name)

	reserved := capacity.Value() - allocatable.Value()
	eviction := evictionHardEphemeralFloor(config.EvictionHard["nodefs.available"], capacity)
	Expect(reserved).To(BeNumerically(">=", kubeReserved.Value()+systemReserved.Value()+eviction),
		"node %s must subtract the kubelet reservation and eviction threshold from ephemeral storage: capacity %s, allocatable %s, kubeReserved %s, systemReserved %s, eviction %d",
		node.Name, capacity.String(), allocatable.String(), kubeReserved.String(), systemReserved.String(), eviction)
}

// kubeletEphemeralQuantity returns the ephemeral-storage value of a kubelet
// reservation map, or zero when the map or key is absent.
func kubeletEphemeralQuantity(values map[string]string) resource.Quantity {
	if value, ok := values["ephemeral-storage"]; ok {
		return resource.MustParse(value)
	}
	return resource.Quantity{}
}

// evictionHardEphemeralFloor resolves the nodefs eviction threshold the kubelet
// applies, rounding a percentage down so the check stays a lower bound.
func evictionHardEphemeralFloor(signal string, capacity resource.Quantity) int64 {
	if signal == "" {
		return 0
	}
	if strings.HasSuffix(signal, "%") {
		percent, err := strconv.ParseFloat(strings.TrimSuffix(signal, "%"), 64)
		Expect(err).NotTo(HaveOccurred(), "eviction signal %q", signal)
		return int64(float64(capacity.Value()) * percent / 100)
	}
	quantity := resource.MustParse(signal)
	return quantity.Value()
}

func osSlug(imageFamily string) string {
	if imageFamily == gcpv1alpha1.ImageFamilyUbuntu {
		return "ubuntu"
	}
	return "cos"
}
