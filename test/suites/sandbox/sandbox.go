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
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/test/pkg/environment"
)

const (
	runtimeKey   = "sandbox.gke.io/runtime"
	poolTaintKey = "karpenter-e2e/nodepool"
	pythonImage  = "docker.io/library/python@sha256:bf12ecc0bea3f171d55145dd02c78b5eeb09f30ee2ac9cc2b4c9e4e8f5e30741"
	cudaImage    = "nvcr.io/nvidia/k8s/cuda-sample@sha256:7c7540bdf1f942d4fb6db97069fd6c289471b54ac29e3c7fcdf914cf77af7d41"
)

var (
	env      *environment.Environment
	poolGVR  = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	classGVR = schema.GroupVersionResource{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"}
)

var _ = BeforeEach(func() { env = environment.Current() })

var _ = Describe("Sandbox", Label("suite:sandbox"), func() {
	It("runs amd64 n2-standard-2 probes and preserves the node through disable/drift", func(ctx SpecContext) {
		runSandbox(ctx, "n2-standard-2", karpv1.ArchitectureAmd64, "2", 60, false, true)
	}, SpecTimeout(45*time.Minute))
	It("runs arm64 t2a-standard-2 probes", func(ctx SpecContext) {
		runSandbox(ctx, "t2a-standard-2", karpv1.ArchitectureArm64, "2", 60, false, false)
	}, SpecTimeout(45*time.Minute))
	It("runs CUDA vector-add on g2-standard-4 with one L4", Label("suite:gpu"), func(ctx SpecContext) {
		runSandbox(ctx, "g2-standard-4", karpv1.ArchitectureAmd64, "4", 100, true, false)
	}, SpecTimeout(45*time.Minute))
})

func runSandbox(ctx SpecContext, instanceType, arch, cpuLimit string, diskGiB int64, gpu, disable bool) {
	runtimeClass := safeRuntimeClass(ctx)
	name := environment.TestPrefix(arch, karpv1.CapacityTypeOnDemand, "sandbox") + "-" + environment.UniqueSuffix()
	ordinaryName := name + "-ordinary"
	var nodeName string
	DeferCleanup(func(cleanupCtx SpecContext) {
		deletePods(cleanupCtx, name, ordinaryName)
		env.DeleteNodePool(cleanupCtx, name)
		env.DeleteNodeClass(cleanupCtx, name)
		if nodeName != "" {
			Expect(env.WaitForNodeRemoval(cleanupCtx, nodeName)).To(Succeed())
		}
	}, NodeTimeout(2*environment.NodeCleanupTimeout))

	env.CreateSandboxNodeClass(ctx, name, diskGiB)
	env.WaitForNodeClassReady(ctx, name)
	tc := environment.TestCase{
		CapacityType: karpv1.CapacityTypeOnDemand, Arch: arch, InstanceTypes: []string{instanceType},
		CPULimit: cpuLimit, ConsolidateAfter: "Never", DisruptionBudgets: []map[string]any{{"nodes": "0"}},
	}
	if gpu {
		tc.GPUCount = "1"
	}
	// The helper deliberately omits runtime fields, so both are controller-owned.
	env.CreateNodePoolWithExpiry(ctx, name, name, tc, "Never")
	waitForRuntimeFields(ctx, name, true)
	env.WaitForNodePoolReady(ctx, name)
	initialNodes := env.AllNodeNames(ctx)

	pod := probePod(name, arch, runtimeClass.Name, gpu)
	created, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Create(ctx, pod, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "sandbox Pod admission; check the existing RuntimeClass")
	Expect(created.Spec.NodeSelector).To(HaveKeyWithValue(runtimeKey, "gvisor"))
	Expect(created.Spec.Tolerations).To(ContainElement(Satisfy(isRuntimeToleration)))
	env.WaitForNodeClaimLaunched(ctx, name)
	claim := onlyClaim(ctx, Default, name)
	env.WaitForNodeClaimInitialized(ctx, name)

	// CUDA may already have exited successfully: never wait for whole-Pod Ready.
	Eventually(func(g Gomega) {
		pod, err = env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pod.Spec.NodeName).NotTo(BeEmpty())
		g.Expect(pod.Status.ContainerStatuses).To(ContainElement(And(
			HaveField("Name", "probe"), HaveField("State.Running", Not(BeNil())), HaveField("RestartCount", int32(0)),
		)))
	}).WithTimeout(environment.GPUProvisioningTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	nodeName = pod.Spec.NodeName
	Expect(initialNodes).NotTo(HaveKey(nodeName), "expected a newly reconstructed node")
	node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(environment.IsNodeReady(node)).To(BeTrue())
	Expect(node.Labels).To(HaveKeyWithValue(karpv1.NodeRegisteredLabelKey, "true"))
	Expect(node.Labels).To(HaveKeyWithValue(karpv1.NodeInitializedLabelKey, "true"))
	Expect(node.Labels).To(HaveKeyWithValue(karpv1.NodePoolLabelKey, name))
	Expect(node.Labels).To(HaveKeyWithValue(karpv1.CapacityTypeLabelKey, karpv1.CapacityTypeOnDemand))
	Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelArchStable, arch))
	Expect(node.Labels).To(HaveKeyWithValue(corev1.LabelInstanceTypeStable, instanceType))
	Expect(node.Labels).To(HaveKeyWithValue(runtimeKey, "gvisor"))
	Expect(node.Spec.Taints).To(ContainElement(runtimeTaint()))
	for key, value := range pod.Spec.NodeSelector {
		Expect(node.Labels).To(HaveKeyWithValue(key, value), "admitted selector must match the assigned node")
	}
	env.WaitForDatapathAgentRunning(ctx, nodeName)
	Eventually(func(g Gomega) {
		original := onlyClaim(ctx, g, name)
		g.Expect(original.GetUID()).To(Equal(claim.GetUID()))
		g.Expect(conditionTrue(original, "Initialized")).To(BeTrue())
		g.Expect(conditionTrue(original, "Ready")).To(BeTrue())
		claimNode, _, err := unstructured.NestedString(original.Object, "status", "nodeName")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(claimNode).To(Equal(nodeName))
	}).WithTimeout(environment.NodePoolReadyTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	Expect(node.Status.NodeInfo.KernelVersion).NotTo(BeEmpty())
	waitForProbe(ctx, name, node.Status.NodeInfo.KernelVersion)
	if gpu {
		capacity, allocatable := node.Status.Capacity["nvidia.com/gpu"], node.Status.Allocatable["nvidia.com/gpu"]
		Expect(capacity.Value()).To(Equal(int64(1)))
		Expect(allocatable.Value()).To(Equal(int64(1)))
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-accelerator", "nvidia-l4"))
		Expect(node.Labels).To(HaveKeyWithValue("cloud.google.com/gke-gpu-driver-version", "default"))
		waitForCUDA(ctx, name)
	}
	if disable {
		verifyDisable(ctx, name, ordinaryName, node, claim)
	}
	Expect(onlyClaim(ctx, Default, name).GetUID()).To(Equal(claim.GetUID()))
}

func safeRuntimeClass(ctx context.Context) nodev1.RuntimeClass {
	classes, err := env.KubeClient.NodeV1().RuntimeClasses().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred(), "read existing RuntimeClasses; sandbox E2E requires read permission")
	for _, class := range classes.Items {
		if class.Handler != "gvisor" || class.Scheduling == nil {
			continue
		}
		if len(class.Scheduling.NodeSelector) == 1 && class.Scheduling.NodeSelector[runtimeKey] == "gvisor" &&
			len(class.Scheduling.Tolerations) == 1 && isRuntimeToleration(class.Scheduling.Tolerations[0]) {
			return class
		}
	}
	Fail("sandbox prerequisite: provide a GKE/admin-owned RuntimeClass (any name) with handler gvisor, only sandbox.gke.io/runtime=gvisor selector and its key-scoped NoSchedule toleration; see docs/sandbox-nodes.md. Tests never create/modify RuntimeClasses or native pools")
	return nodev1.RuntimeClass{}
}

func isRuntimeToleration(t corev1.Toleration) bool {
	return t.Key == runtimeKey && t.Effect == corev1.TaintEffectNoSchedule &&
		(t.Operator == corev1.TolerationOpExists || ((t.Operator == corev1.TolerationOpEqual || t.Operator == "") && t.Value == "gvisor"))
}

func runtimeTaint() corev1.Taint {
	return corev1.Taint{Key: runtimeKey, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule}
}

func probePod(name, arch, class string, gpu bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: environment.TestNamespace},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(class), RestartPolicy: corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 map[string]string{karpv1.NodePoolLabelKey: name, corev1.LabelArchStable: arch},
			Tolerations:                  []corev1.Toleration{{Key: poolTaintKey, Operator: corev1.TolerationOpEqual, Value: name, Effect: corev1.TaintEffectNoSchedule}},
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000), FSGroup: ptr.To[int64](1000), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Volumes: []corev1.Volume{
				{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "api-ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}}},
			},
			Containers: []corev1.Container{{
				Name: "probe", Image: pythonImage, Command: []string{"python3", "-u", "-c", probeScript},
				SecurityContext: unprivileged(),
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}, {Name: "api-ca", MountPath: "/api-ca", ReadOnly: true}},
			}},
		},
	}
	if arch == karpv1.ArchitectureArm64 {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
			Key: corev1.LabelArchStable, Value: karpv1.ArchitectureArm64,
			Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual,
		})
	}
	if gpu {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			// Read-only OCI config verification: ENTRYPOINT is /cuda-samples/sample,
			// with no CMD. Preserve it so this executes vector-add, not a shell/pause.
			Name: "cuda", Image: cudaImage, SecurityContext: unprivileged(),
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1"), corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		})
	}
	return pod
}

func unprivileged() *corev1.SecurityContext {
	return &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
}

// Compare this kernel release with the node's host kernel, as recommended by GKE.
// TLS verifies the Service certificate using the public namespace CA, not a token.
const probeScript = `import json, pathlib, platform, socket, ssl, time, urllib.request, urllib.error
ctx = ssl.create_default_context(cafile="/api-ca/ca.crt")
deadline = time.monotonic() + 120
while True:
    try:
        socket.getaddrinfo("kubernetes.default.svc", 443)
        try:
            with urllib.request.urlopen("https://kubernetes.default.svc/version", context=ctx, timeout=10) as response:
                assert response.status == 200
        except urllib.error.HTTPError as error:
            assert error.code in (401, 403), error.code
        break
    except (OSError, urllib.error.URLError):
        if time.monotonic() >= deadline:
            raise
        time.sleep(2)
path = pathlib.Path("/scratch/probe")
path.write_text("sandbox-emptydir")
assert path.read_text() == "sandbox-emptydir"
assert not pathlib.Path("/var/run/secrets/kubernetes.io/serviceaccount/token").exists()
print(json.dumps({"kernel": platform.release(), "checks": "dns,tls,emptydir,no-token"}), flush=True)
time.sleep(3600)
`

func boundedLogs(ctx context.Context, name, container string) ([]byte, error) {
	logCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return env.KubeClient.CoreV1().Pods(environment.TestNamespace).GetLogs(name, &corev1.PodLogOptions{
		Container: container, LimitBytes: ptr.To[int64](16384), TailLines: ptr.To[int64](30),
	}).DoRaw(logCtx)
}

func waitForProbe(ctx context.Context, name, hostKernel string) {
	Eventually(func(g Gomega) {
		logs, err := boundedLogs(ctx, name, "probe")
		g.Expect(err).NotTo(HaveOccurred())
		var result struct {
			Kernel string
			Checks string
		}
		g.Expect(json.Unmarshal(logs, &result)).To(Succeed(), "probe output: %s", logs)
		g.Expect(result.Checks).To(Equal("dns,tls,emptydir,no-token"))
		g.Expect(result.Kernel).NotTo(BeEmpty())
		g.Expect(result.Kernel).NotTo(Equal(hostKernel), "sandbox must not report the host kernel")
	}).WithTimeout(3 * time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func waitForCUDA(ctx context.Context, name string) {
	Eventually(func(g Gomega) {
		pod, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pod.Status.ContainerStatuses).To(ContainElement(And(
			HaveField("Name", "cuda"), HaveField("State.Terminated", Not(BeNil())),
			HaveField("State.Terminated.ExitCode", int32(0)), HaveField("RestartCount", int32(0)),
		)))
		logs, err := boundedLogs(ctx, name, "cuda")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(string(logs)).To(ContainSubstring("Test PASSED"), "CUDA vector-add must complete successfully")
	}).WithTimeout(5 * time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func waitForRuntimeFields(ctx context.Context, name string, enabled bool) {
	Eventually(func(g Gomega) {
		pool, err := env.DynamicClient.Resource(poolGVR).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		labels, _, err := unstructured.NestedStringMap(pool.Object, "spec", "template", "metadata", "labels")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(labels).To(HaveKeyWithValue("karpenter-e2e/owned", "true"))
		taints, _, err := unstructured.NestedSlice(pool.Object, "spec", "template", "spec", "taints")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(taints).To(ContainElement(map[string]any{"key": poolTaintKey, "value": name, "effect": "NoSchedule"}))
		canonical := map[string]any{"key": runtimeKey, "value": "gvisor", "effect": "NoSchedule"}
		if enabled {
			g.Expect(labels).To(HaveKeyWithValue(runtimeKey, "gvisor"))
			g.Expect(taints).To(ContainElement(canonical))
			g.Expect(taints).To(HaveLen(2))
			var ownership map[string]bool
			g.Expect(json.Unmarshal([]byte(pool.GetAnnotations()["karpenter.k8s.gcp/sandbox-ownership"]), &ownership)).To(Succeed())
			g.Expect(ownership).To(Equal(map[string]bool{"label": true, "taint": true}), "both omitted runtime fields must be controller-owned")
		} else {
			g.Expect(labels).NotTo(HaveKey(runtimeKey))
			g.Expect(taints).To(HaveLen(1))
			g.Expect(pool.GetAnnotations()).NotTo(HaveKey("karpenter.k8s.gcp/sandbox-ownership"))
		}
	}).WithTimeout(environment.NodePoolReadyTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func onlyClaim(ctx context.Context, g Gomega, pool string) *unstructured.Unstructured {
	claims, err := env.ListNodeClaims(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	var owned []unstructured.Unstructured
	for _, claim := range claims {
		if claim.GetLabels()[karpv1.NodePoolLabelKey] == pool {
			owned = append(owned, claim)
		}
	}
	g.Expect(owned).To(HaveLen(1), "pool %s must retain exactly one original claim", pool)
	if len(owned) != 1 {
		return &unstructured.Unstructured{}
	}
	return &owned[0]
}

func conditionTrue(object *unstructured.Unstructured, name string) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		if condition, ok := item.(map[string]any); ok && condition["type"] == name && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func deletePods(ctx context.Context, names ...string) {
	for _, name := range names {
		err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}
	Eventually(func(g Gomega) {
		for _, name := range names {
			_, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, name, metav1.GetOptions{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "demand Pod %s must be gone before class/pool cleanup or disable", name)
		}
	}).WithTimeout(environment.NodeCleanupTimeout).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func verifyDisable(ctx context.Context, name, ordinaryName string, node *corev1.Node, claim *unstructured.Unstructured) {
	By("requiring ordinary Pod rejection by the sandbox taint, on the same pool")
	ordinary := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: ordinaryName, Namespace: environment.TestNamespace}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:    map[string]string{karpv1.NodePoolLabelKey: name},
		Tolerations:     []corev1.Toleration{{Key: poolTaintKey, Operator: corev1.TolerationOpEqual, Value: name, Effect: corev1.TaintEffectNoSchedule}},
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "ordinary", Image: environment.PauseImage, SecurityContext: unprivileged()}},
	}}
	admitted, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Create(ctx, ordinary, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(admitted.Spec.RuntimeClassName).To(BeNil())
	Expect(admitted.Spec.Tolerations).NotTo(ContainElement(Satisfy(isRuntimeToleration)))
	Eventually(func(g Gomega) {
		pod, err := env.KubeClient.CoreV1().Pods(environment.TestNamespace).Get(ctx, ordinaryName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pod.Spec.NodeName).To(BeEmpty())
		g.Expect(pod.Status.Conditions).To(ContainElement(And(
			HaveField("Type", corev1.PodScheduled), HaveField("Status", corev1.ConditionFalse),
			HaveField("Reason", "Unschedulable"), HaveField("Message", And(ContainSubstring("untolerated taint"), ContainSubstring(runtimeKey))),
		)))
	}).WithTimeout(2 * time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	Expect(onlyClaim(ctx, Default, name).GetUID()).To(Equal(claim.GetUID()))

	By("removing demand before disabling only this spec's class, without voluntary replacement")
	deletePods(ctx, name, ordinaryName)
	snapshot, stream, err := env.ListWatchNodeClaims(ctx, name)
	Expect(err).NotTo(HaveOccurred())
	defer stream.Stop()
	Expect(snapshot.Items).To(HaveLen(1))
	Expect(snapshot.Items[0].GetUID()).To(Equal(claim.GetUID()))
	_, err = env.DynamicClient.Resource(classGVR).Patch(ctx, name, types.MergePatchType, []byte(`{"spec":{"sandboxConfig":null}}`), metav1.PatchOptions{})
	Expect(err).NotTo(HaveOccurred())
	waitForRuntimeFields(ctx, name, false)
	Eventually(func(g Gomega) {
		original := onlyClaim(ctx, g, name)
		g.Expect(original.GetUID()).To(Equal(claim.GetUID()))
		g.Expect(conditionTrue(original, "Drifted")).To(BeTrue(), "same claim must become Drifted=True (any reason)")
	}).WithTimeout(5 * time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
	Consistently(func(g Gomega) {
		original := onlyClaim(ctx, g, name)
		g.Expect(original.GetUID()).To(Equal(claim.GetUID()))
		g.Expect(original.GetDeletionTimestamp()).To(BeNil())
		current, err := env.KubeClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(current.UID).To(Equal(node.UID))
		g.Expect(current.Spec.ProviderID).To(Equal(node.Spec.ProviderID))
		g.Expect(current.DeletionTimestamp).To(BeNil())
		g.Expect(current.Labels).To(HaveKeyWithValue(runtimeKey, "gvisor"))
		g.Expect(current.Spec.Taints).To(ContainElement(runtimeTaint()))
		assertNoReplacement(g, stream, claim.GetUID())
	}).WithTimeout(time.Minute).WithPolling(environment.DefaultPollInterval).Should(Succeed())
}

func assertNoReplacement(g Gomega, stream watch.Interface, uid types.UID) {
	for {
		select {
		case event, open := <-stream.ResultChan():
			g.Expect(open).To(BeTrue(), "claim watch closed before bounded disable verification completed")
			if !open {
				return
			}
			g.Expect(event.Type).NotTo(Equal(watch.Error), "claim watch failed: %v", event.Object)
			g.Expect(event.Type).NotTo(Equal(watch.Deleted), "original claim must not be deleted")
			object, ok := event.Object.(metav1.Object)
			g.Expect(ok).To(BeTrue(), "unexpected claim watch object %T", event.Object)
			if ok {
				g.Expect(object.GetUID()).To(Equal(uid), "no replacement/additional claim may appear")
			}
		default:
			return
		}
	}
}
