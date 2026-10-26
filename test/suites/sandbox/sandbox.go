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

package sandbox

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

const (
	runtimeKey = "sandbox.gke.io/runtime"
	unameImage = "registry.k8s.io/e2e-test-images/busybox@sha256:a9155b13325b2abef48e71de77bb8ac015412a566829f621d06bfae5c699b1b9"
	cudaImage  = "nvcr.io/nvidia/k8s/cuda-sample@sha256:7c7540bdf1f942d4fb6db97069fd6c289471b54ac29e3c7fcdf914cf77af7d41"
)

var (
	env          *environment.Environment
	runtimeTaint = corev1.Taint{Key: runtimeKey, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule}
)

var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("Sandbox", Label("suite:sandbox"), func() {
	It("runs gVisor on amd64 n2-standard-2", func(ctx SpecContext) {
		runSandbox(ctx, environment.TestCase{Arch: karpv1.ArchitectureAmd64, InstanceTypes: []string{"n2-standard-2"}, CPULimit: "2"}, 60)
	}, SpecTimeout(45*time.Minute))
	It("runs gVisor on arm64 t2a-standard-2", func(ctx SpecContext) {
		runSandbox(ctx, environment.TestCase{Arch: karpv1.ArchitectureArm64, InstanceTypes: []string{"t2a-standard-2"}, CPULimit: "2"}, 60)
	}, SpecTimeout(45*time.Minute))
	It("runs CUDA vector-add under gVisor on g2-standard-4 with one L4", Label("suite:gpu"), func(ctx SpecContext) {
		runSandbox(ctx, environment.TestCase{Arch: karpv1.ArchitectureAmd64, InstanceTypes: []string{"g2-standard-4"}, CPULimit: "4", GPUCount: "1"}, 100)
	}, SpecTimeout(45*time.Minute))
})

func runSandbox(ctx SpecContext, tc environment.TestCase, diskGiB int64) {
	class := sandboxRuntimeClass(ctx)
	name := environment.TestPrefix(tc.Arch, karpv1.CapacityTypeOnDemand, "sandbox") + "-" + environment.UniqueSuffix()
	var nodeName string
	DeferCleanup(func(cleanupCtx SpecContext) {
		// Initialization may fail before the workload poll captures its assigned node.
		pod, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(cleanupCtx, name, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
			nodeName = pod.Spec.NodeName
		}
		err = env.KubeClient.CoreV1().Pods(environment.TestNamespace).Delete(cleanupCtx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		Eventually(func() bool {
			_, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(cleanupCtx, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}).WithTimeout(environment.NodeCleanupTimeout).WithPolling(environment.DefaultPollInterval).Should(BeTrue())
		env.DeleteNodePool(cleanupCtx, name)
		env.DeleteNodeClass(cleanupCtx, name)
		if nodeName != "" {
			Expect(env.WaitForNodeRemoval(cleanupCtx, nodeName)).To(Succeed())
		}
	}, NodeTimeout(2*environment.NodeCleanupTimeout))

	tc.CapacityType = karpv1.CapacityTypeOnDemand
	tc.ConsolidateAfter = "Never"
	tc.DisruptionBudgets = []map[string]any{{"nodes": "0"}}
	env.CreateSandboxNodeClass(ctx, name, diskGiB)
	env.WaitForNodeClassReady(ctx, name)
	env.CreateNodePoolWithExpiry(ctx, name, name, tc, "Never")
	env.WaitForNodePoolReady(ctx, name)
	initialNodes := env.AllNodeNames(ctx)

	pod, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Create(ctx, sandboxPod(name, class, tc), metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "sandbox Pod admission; check the existing RuntimeClass")
	Expect(pod.Spec.NodeSelector).To(HaveKeyWithValue(runtimeKey, "gvisor"))
	env.WaitForNodeClaimLaunched(ctx, name)
	env.WaitForNodeClaimInitialized(ctx, name)
	// Both uname and CUDA are one-shot commands; whole-Pod Ready can be missed.
	Eventually(func(g Gomega) {
		pod, err = env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		nodeName = pod.Spec.NodeName
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodSucceeded), "sandbox workload must complete successfully")
		g.Expect(pod.Status.ContainerStatuses).To(HaveEach(HaveField("RestartCount", int32(0))))
	}).WithTimeout(environment.GPUProvisioningTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())

	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(initialNodes).NotTo(HaveKey(nodeName), "expected a newly provisioned node")
	Expect(environment.IsNodeReady(node)).To(BeTrue())
	Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, tc.InstanceTypes[0]))
	Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelArchStable, tc.Arch))
	Expect(node.Spec.Taints).To(ContainElement(runtimeTaint))
	for key, value := range pod.Spec.NodeSelector {
		Expect(node.Labels).To(HaveKeyWithValue(key, value), "admitted selector must match the assigned node")
	}
	env.WaitForDatapathAgentRunning(ctx, nodeName)
	Expect(node.Status.NodeInfo.KernelVersion).NotTo(BeEmpty())
	Eventually(func(g Gomega) {
		logs, err := containerLogs(ctx, name, "kernel")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(logs)).NotTo(BeEmpty())
		g.Expect(strings.TrimSpace(logs)).NotTo(Equal(node.Status.NodeInfo.KernelVersion), "gVisor must not report the host kernel")
		if tc.GPUCount != "" {
			logs, err = containerLogs(ctx, name, "cuda")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("Test PASSED"), "CUDA vector-add must execute successfully")
		}
	}).WithTimeout(time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	if tc.GPUCount != "" {
		capacity, allocatable := node.Status.Capacity["nvidia.com/gpu"], node.Status.Allocatable["nvidia.com/gpu"]
		Expect(capacity.Value()).To(Equal(int64(1)))
		Expect(allocatable.Value()).To(Equal(int64(1)))
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-accelerator", "nvidia-l4"))
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-gpu-driver-version", "default"))
	}
}

func sandboxRuntimeClass(ctx context.Context) string {
	classes, err := env.KubeClient.NodeV1().RuntimeClasses().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "read existing RuntimeClasses")
	for _, class := range classes.Items {
		if class.Handler != "gvisor" || class.Scheduling == nil || len(class.Scheduling.NodeSelector) != 1 || class.Scheduling.NodeSelector[runtimeKey] != "gvisor" {
			continue
		}
		for _, toleration := range class.Scheduling.Tolerations {
			if toleration.ToleratesTaint(log.FromContext(ctx), &runtimeTaint, false) {
				return class.Name
			}
		}
	}
	Fail("sandbox prerequisite: provide an existing RuntimeClass with handler gvisor, only sandbox.gke.io/runtime=gvisor selector and a matching toleration; see test/suites/sandbox/README.md")
	return ""
}

func sandboxPod(name, class string, tc environment.TestCase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: environment.TestNamespace},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(class), RestartPolicy: corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 map[string]string{karpv1.NodePoolLabelKey: name, corev1.LabelArchStable: tc.Arch},
			Tolerations:                  []corev1.Toleration{{Key: "karpenter-e2e/nodepool", Operator: corev1.TolerationOpEqual, Value: name, Effect: corev1.TaintEffectNoSchedule}},
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{
				Name: "kernel", Image: unameImage, Command: []string{"/bin/uname", "-r"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				},
			}},
		},
	}
	if tc.Arch == karpv1.ArchitectureArm64 {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: corev1.LabelArchStable, Operator: corev1.TolerationOpEqual, Value: tc.Arch, Effect: corev1.TaintEffectNoSchedule})
	}
	if tc.GPUCount != "" {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			// Preserve the official sample's /cuda-samples/sample entrypoint.
			Name: "cuda", Image: cudaImage,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		})
	}
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].SecurityContext = &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		}
	}
	return pod
}

func containerLogs(ctx context.Context, name, container string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	logs, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).GetLogs(name, &corev1.PodLogOptions{
		Container: container, LimitBytes: ptr.To[int64](16384), TailLines: ptr.To[int64](30),
	}).DoRaw(ctx)
	return string(logs), err
}
