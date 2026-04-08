/*
Copyright 2024 The CloudPilot AI Authors.

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

package environment

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/api/googleapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const (
	// DefaultE2EDiskGiB is the boot disk size for test nodes; 30 GiB meets the
	// minimum required by ContainerOptimizedOS while keeping costs low.
	DefaultE2EDiskGiB = 30
	// DefaultNodePoolWeight separates test NodePools from any pre-existing ones.
	DefaultNodePoolWeight = 10
	// DefaultConsolidateAfter is how quickly Karpenter reclaims idle test nodes.
	DefaultConsolidateAfter = "30s"
	// ReplacementTimeout is the budget for node replacement tests (drift, expiration).
	// 2× ProvisioningTimeout accounts for draining the old node before the new one is ready.
	ReplacementTimeout = 2 * ProvisioningTimeout
)

// TestCase describes a provisioning scenario: capacity type, architecture,
// and the set of GCP instance families and types the test is allowed to use.
type TestCase struct {
	CapacityType  string
	Arch          string
	Families      []string
	InstanceTypes []string
}

// UniqueSuffix returns a 6-character random hex string safe for use in k8s names.
func UniqueSuffix() string {
	return fmt.Sprintf("%06x", rand.Uint32()&0xffffff) //nolint:gosec // weak RNG is fine for test resource name suffixes
}

// TestPrefix returns a human-readable prefix for test resource names. The arch
// and capacityType are always included; additional parts (e.g. suite name) are
// appended as extra dash-separated segments.
func TestPrefix(arch, capacityType string, parts ...string) string {
	ct := "od"
	if capacityType == karpv1.CapacityTypeSpot {
		ct = "spot"
	}
	return strings.Join(append([]string{arch, ct}, parts...), "-")
}

// CreateNodeClass creates a GCENodeClass with ContainerOptimizedOS image and a
// 30 GiB boot disk, using the environment's pods range. If a resource with the
// same name already exists (leftover from a previous run), it is deleted first.
func (e *Environment) CreateNodeClass(ctx context.Context, name string) {
	deleteIfExists(ctx, e.DynamicClient, gceNodeClassGVR, name)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.gcp/v1alpha1",
		"kind":       "GCENodeClass",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"imageSelectorTerms": []any{
				map[string]any{"alias": "ContainerOptimizedOS@latest"},
			},
			"disks": []any{
				map[string]any{"category": "pd-balanced", "sizeGiB": int64(DefaultE2EDiskGiB), "boot": true},
			},
			"subnetRangeName": e.PodsRangeName,
		},
	}}
	_, err := e.DynamicClient.Resource(gceNodeClassGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating GCENodeClass %s", name)
	e.trackNodeClass(name)
}

// CreateNodePool creates a NodePool with the given requirements and the default
// consolidation policy (WhenEmptyOrUnderutilized, consolidateAfter=30s).
func (e *Environment) CreateNodePool(ctx context.Context, name, nodeClassName string, tc TestCase) {
	e.createNodePool(ctx, name, nodeClassName, tc, "")
}

// CreateNodePoolWithExpiry creates a NodePool like CreateNodePool but sets
// expireAfter on the template so karpenter replaces nodes after the given
// duration. expireAfter must be a valid karpenter duration string, e.g. "2m".
func (e *Environment) CreateNodePoolWithExpiry(ctx context.Context, name, nodeClassName string, tc TestCase, expireAfter string) {
	e.createNodePool(ctx, name, nodeClassName, tc, expireAfter)
}

func (e *Environment) createNodePool(ctx context.Context, name, nodeClassName string, tc TestCase, expireAfter string) {
	requirements := []any{
		map[string]any{"key": karpv1.CapacityTypeLabelKey, "operator": "In", "values": []any{tc.CapacityType}},
		map[string]any{"key": gcpv1alpha1.LabelInstanceFamily, "operator": "In", "values": toAny(tc.Families)},
		map[string]any{"key": corev1.LabelInstanceTypeStable, "operator": "In", "values": toAny(tc.InstanceTypes)},
		map[string]any{"key": corev1.LabelArchStable, "operator": "In", "values": []any{tc.Arch}},
	}
	templateSpec := map[string]any{
		"nodeClassRef": map[string]any{
			"name":  nodeClassName,
			"kind":  "GCENodeClass",
			"group": "karpenter.k8s.gcp",
		},
		"requirements": requirements,
		"taints": []any{
			map[string]any{
				"key":    "karpenter-e2e/nodepool",
				"value":  name,
				"effect": "NoSchedule",
			},
		},
	}
	if expireAfter != "" {
		templateSpec["expireAfter"] = expireAfter
	}
	// When expireAfter is set we use WhenEmpty rather than WhenEmptyOrUnderutilized
	// so that node replacement is driven solely by expiration, not by concurrent
	// utilization-based consolidation which would interfere with the test.
	consolidationPolicy := "WhenEmptyOrUnderutilized"
	if expireAfter != "" {
		consolidationPolicy = "WhenEmpty"
	}
	deleteIfExists(ctx, e.DynamicClient, nodePoolGVR, name)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1",
		"kind":       "NodePool",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"weight": int64(DefaultNodePoolWeight),
			"disruption": map[string]any{
				"consolidateAfter":    DefaultConsolidateAfter,
				"consolidationPolicy": consolidationPolicy,
				"budgets":             []any{map[string]any{"nodes": "100%"}},
			},
			"template": map[string]any{"spec": templateSpec},
		},
	}}
	_, err := e.DynamicClient.Resource(nodePoolGVR).Create(ctx, obj, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating NodePool %s", name)
	e.trackNodePool(name)
}

// CreateDeployment creates a single-replica Deployment of the pause container
// pinned to the given NodePool via a NodeSelector. ARM64 deployments get the
// kubernetes.io/arch toleration required by GKE's automatic arch taint.
func (e *Environment) CreateDeployment(ctx context.Context, name, appLabel, nodePoolName, arch string) {
	replicas := int32(1)
	zero := int64(0)
	tolerations := []corev1.Toleration{{
		Key:      "karpenter-e2e/nodepool",
		Value:    nodePoolName,
		Effect:   corev1.TaintEffectNoSchedule,
		Operator: corev1.TolerationOpEqual,
	}}
	if arch == karpv1.ArchitectureArm64 {
		tolerations = append(tolerations, corev1.Toleration{
			Key:      corev1.LabelArchStable,
			Value:    karpv1.ArchitectureArm64,
			Effect:   corev1.TaintEffectNoSchedule,
			Operator: corev1.TolerationOpEqual,
		})
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: TestNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
				Spec: corev1.PodSpec{
					NodeSelector:                  map[string]string{karpv1.NodePoolLabelKey: nodePoolName},
					Tolerations:                   tolerations,
					TerminationGracePeriodSeconds: &zero,
					Containers: []corev1.Container{{
						Name:  "inflate",
						Image: PauseImage,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
				},
			},
		},
	}
	// Delete any stale Deployment from a previous run before creating the new one.
	_ = e.KubeClient.AppsV1().Deployments(TestNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	Eventually(func(g Gomega) {
		_, err := e.KubeClient.AppsV1().Deployments(TestNamespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "waiting for stale Deployment %s to be deleted", name)
	}).WithTimeout(NodeCleanupTimeout).WithPolling(5 * time.Second).Should(Succeed())

	_, err := e.KubeClient.AppsV1().Deployments(TestNamespace).Create(ctx, dep, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "creating Deployment %s", name)
}

// WaitForRunningPod polls until a pod with the given app label is Running and
// Ready. Returns the pod; fails the test if ProvisioningTimeout is exceeded.
func (e *Environment) WaitForRunningPod(ctx context.Context, appLabel string) *corev1.Pod {
	var found *corev1.Pod
	Eventually(func(g Gomega) {
		found = nil
		pods, err := e.KubeClient.CoreV1().Pods(TestNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", appLabel)})
		g.Expect(err).NotTo(HaveOccurred())
		for i := range pods.Items {
			if isRunningAndReady(&pods.Items[i]) {
				found = pods.Items[i].DeepCopy()
				return
			}
		}
		e.logWaitStatus(ctx, appLabel, pods.Items)
		g.Expect(found).NotTo(BeNil(), "no running pod with label app=%s", appLabel)
	}).WithTimeout(ProvisioningTimeout).WithPolling(5 * time.Second).Should(Succeed())
	return found
}

// IsNodeReady returns true if the node has the Ready condition set to True.
func IsNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// isRunningAndReady returns true if the pod is Running, scheduled, and Ready.
func isRunningAndReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// logWaitStatus prints pod conditions and NodeClaim statuses to help diagnose
// slow provisioning.
func (e *Environment) logWaitStatus(ctx context.Context, appLabel string, pods []corev1.Pod) {
	if len(pods) > 0 {
		p := &pods[0]
		GinkgoWriter.Printf("pod app=%s phase=%s nodeName=%q\n",
			appLabel, p.Status.Phase, p.Spec.NodeName)
		for _, c := range p.Status.Conditions {
			if c.Status != "True" {
				GinkgoWriter.Printf("  pod condition %s=%s: %s\n", c.Type, c.Status, c.Message)
			}
		}
	}
	if claims, err := e.ListNodeClaims(ctx); err == nil {
		for _, c := range claims {
			GinkgoWriter.Printf("NodeClaim %s: %v\n", c.GetName(), c.Object["status"])
		}
	}
}

// WaitForPodOnDifferentNode polls until a Running pod with appLabel is
// scheduled on a node other than excludeNode. Use when testing node replacement
// (expiration, drift) where a new node must be provisioned.
func (e *Environment) WaitForPodOnDifferentNode(ctx context.Context, appLabel, excludeNode string, timeout time.Duration) *corev1.Pod {
	var found *corev1.Pod
	Eventually(func(g Gomega) {
		found = nil
		pods, err := e.KubeClient.CoreV1().Pods(TestNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", appLabel)})
		g.Expect(err).NotTo(HaveOccurred())
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Spec.NodeName == "" || p.Spec.NodeName == excludeNode {
				continue
			}
			if isRunningAndReady(p) {
				found = p.DeepCopy()
				return
			}
		}
		if claims, err2 := e.ListNodeClaims(ctx); err2 == nil {
			for _, c := range claims {
				GinkgoWriter.Printf("NodeClaim %s: %v\n", c.GetName(), c.Object["status"])
			}
		}
		g.Expect(found).NotTo(BeNil(),
			"no running pod with label app=%s on a node other than %s", appLabel, excludeNode)
	}).WithTimeout(timeout).WithPolling(5 * time.Second).Should(Succeed())
	return found
}

// ScaleDeployment handles the Get-mutate-Update cycle required by the typed client.
func (e *Environment) ScaleDeployment(ctx context.Context, name string, replicas int32) {
	dep, err := e.KubeClient.AppsV1().Deployments(TestNamespace).Get(ctx, name, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "getting Deployment %s for scale", name)
	dep.Spec.Replicas = &replicas
	_, err = e.KubeClient.AppsV1().Deployments(TestNamespace).Update(ctx, dep, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred(), "scaling Deployment %s to %d", name, replicas)
}

// UpdateNodePoolInstanceTypes patches the NodePool's instance-type requirement
// to the given list so that nodes running a type not in the list are drifted.
// Retries automatically on resource-version conflicts (karpenter reconciles
// the NodePool concurrently and can bump the resourceVersion between our
// Get and Update).
func (e *Environment) UpdateNodePoolInstanceTypes(ctx context.Context, name string, instanceTypes []string) {
	err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		np, err := e.DynamicClient.Resource(nodePoolGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		spec, ok := np.Object["spec"].(map[string]any)
		if !ok {
			return false, fmt.Errorf("NodePool %s: missing or invalid spec field", name)
		}
		template, ok := spec["template"].(map[string]any)
		if !ok {
			return false, fmt.Errorf("NodePool %s: missing or invalid spec.template field", name)
		}
		templateSpec, ok := template["spec"].(map[string]any)
		if !ok {
			return false, fmt.Errorf("NodePool %s: missing or invalid spec.template.spec field", name)
		}
		reqs, ok := templateSpec["requirements"].([]any)
		if !ok {
			return false, fmt.Errorf("NodePool %s: missing or invalid spec.template.spec.requirements field", name)
		}
		found := false
		for i, req := range reqs {
			r, ok := req.(map[string]any)
			if !ok {
				return false, fmt.Errorf("NodePool %s: requirement at index %d is not a map", name, i)
			}
			if r["key"] == corev1.LabelInstanceTypeStable {
				r["values"] = toAny(instanceTypes)
				reqs[i] = r
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Errorf("NodePool %s: requirement key %s not found", name, corev1.LabelInstanceTypeStable)
		}
		templateSpec["requirements"] = reqs
		template["spec"] = templateSpec
		spec["template"] = template
		np.Object["spec"] = spec

		_, err = e.DynamicClient.Resource(nodePoolGVR).Update(ctx, np, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return false, nil // retry
		}
		return err == nil, err
	})
	Expect(err).NotTo(HaveOccurred(), "updating NodePool %s instance types", name)
}

// DeleteDeployment ignores 404 so callers need not check existence first.
func (e *Environment) DeleteDeployment(ctx context.Context, name string) {
	err := e.KubeClient.AppsV1().Deployments(TestNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting Deployment %s", name)
	}
}

// DeleteNodePool ignores 404 so callers need not check existence first.
func (e *Environment) DeleteNodePool(ctx context.Context, name string) {
	err := e.DynamicClient.Resource(nodePoolGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting NodePool %s", name)
	}
}

// DeleteNodeClass ignores 404 so callers need not check existence first.
func (e *Environment) DeleteNodeClass(ctx context.Context, name string) {
	err := e.DynamicClient.Resource(gceNodeClassGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting GCENodeClass %s", name)
	}
}

// AllNodeNames returns a set of node names (map keys only) for O(1) membership tests.
func (e *Environment) AllNodeNames(ctx context.Context) map[string]struct{} {
	nodes, err := e.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	result := make(map[string]struct{}, len(nodes.Items))
	for _, n := range nodes.Items {
		result[n.Name] = struct{}{}
	}
	return result
}

// ForceDeleteNodeClaim removes the karpenter finalizer from the named NodeClaim
// and then deletes it. This leaves the backing GCE VM running without an owner,
// simulating the orphaned-VM scenario that the GC controller is meant to clean up.
func (e *Environment) ForceDeleteNodeClaim(ctx context.Context, name string) {
	// Retry on conflict: karpenter may update the NodeClaim concurrently,
	// causing a resource-version conflict on our finalizer removal.
	// A 404 means karpenter already deleted the NodeClaim — nothing to do.
	Eventually(func(g Gomega) {
		nc, err := e.DynamicClient.Resource(nodeClaimGVR).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return // already gone
		}
		g.Expect(err).NotTo(HaveOccurred(), "getting NodeClaim %s", name)
		nc.SetFinalizers(nil)
		_, err = e.DynamicClient.Resource(nodeClaimGVR).Update(ctx, nc, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred(), "removing finalizer from NodeClaim %s", name)
	}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(Succeed())

	err := e.DynamicClient.Resource(nodeClaimGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred(), "deleting NodeClaim %s", name)
	}
}

// WaitForVMDeletion polls the GCE Instances API until the VM identified by
// providerID returns 404, confirming the GC controller deleted it.
// providerID format: gce://<project>/<zone>/<name>
func (e *Environment) WaitForVMDeletion(ctx context.Context, providerID string) error {
	project, zone, name, err := parseProviderID(providerID)
	if err != nil {
		return err
	}
	return wait.PollUntilContextCancel(ctx, 10*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := e.computeSvc.Instances.Get(project, zone, name).Context(ctx).Do()
			if err == nil {
				return false, nil
			}
			if isNotFound(err) {
				return true, nil
			}
			// Surface non-transient errors (auth, quota) immediately.
			return false, err
		},
	)
}

// parseProviderID extracts project, zone, and instance name from a gce://<project>/<zone>/<name> provider ID.
func parseProviderID(providerID string) (project, zone, name string, err error) {
	after, ok := strings.CutPrefix(providerID, "gce://")
	if !ok {
		return "", "", "", fmt.Errorf("providerID missing gce:// prefix: %q", providerID)
	}
	parts := strings.SplitN(after, "/", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("unexpected providerID format: %q", providerID)
	}
	return parts[0], parts[1], parts[2], nil
}

func isNotFound(err error) bool {
	var gErr *googleapi.Error
	if errors.As(err, &gErr) {
		return gErr.Code == 404
	}
	return false
}

// toAny is required because unstructured.Unstructured fields are typed as []any.
func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// deleteIfExists deletes a resource if it exists and waits for it to be fully
// removed before returning. This prevents "object is being deleted" errors when
// a resource from a previous run still has its finalizer running.
func deleteIfExists(ctx context.Context, client dynamic.Interface, gvr schema.GroupVersionResource, name string) {
	err := client.Resource(gvr).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		Expect(err).NotTo(HaveOccurred(), "deleteIfExists: deleting %s/%s", gvr.Resource, name)
		return
	}
	// Wait for the object to disappear so the caller can safely recreate it.
	Eventually(func(g Gomega) {
		_, err := client.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"waiting for %s/%s to be fully deleted", gvr.Resource, name)
	}).WithTimeout(NodeCleanupTimeout).WithPolling(5 * time.Second).Should(Succeed())
}
