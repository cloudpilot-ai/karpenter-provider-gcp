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
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/gomega"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	KarpenterNamespace  = "karpenter-system"
	KarpenterDeployment = "karpenter"
	// TestNamespace is the namespace used by all e2e test suites for workloads.
	TestNamespace = "karpenter-e2e-test"

	ControllerStartTimeout = 5 * time.Minute
	// NodePoolReadyTimeout is used when waiting for GKE template node pools to
	// reach RUNNING state. Pool creation (triggered by karpenter on first start)
	// can take up to 10 minutes, longer than a plain Deployment rollout.
	NodePoolReadyTimeout = 10 * time.Minute
	// NodeCleanupTimeout is the budget for a karpenter-provisioned VM to be
	// terminated after the owning NodePool is deleted. GCP VM deletion typically
	// completes in 60–90 s. Test nodes carry a per-nodepool taint so no system
	// pods land on them; there is nothing to evict before deletion.
	NodeCleanupTimeout = 3 * time.Minute
	// ProvisioningTimeout is the maximum time for a GCP VM to be created, boot,
	// register with the cluster, and for the pod to reach Running. GCP typically
	// takes 4–7 minutes; 10 minutes gives a comfortable margin.
	ProvisioningTimeout = 10 * time.Minute
	PauseImage          = "registry.k8s.io/pause:3.10"
)

var (
	// nodeClaimGVR is package-private; use ListNodeClaims to access NodeClaims.
	nodeClaimGVR    = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	NodePoolGVR     = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	GCENodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"}
)

// Environment holds shared state for a test suite run.
type Environment struct {
	ProjectID       string
	ClusterName     string
	ClusterLocation string // region or zone, e.g. "us-central1" or "us-central1-a"
	PodsRangeName   string

	KubeClient    kubernetes.Interface
	DynamicClient dynamic.Interface
	containerSvc  *container.Service
	computeSvc    *compute.Service

	// mu guards ownedNodePools and ownedNodeClasses so Cleanup is safe when
	// multiple Ginkgo processes share the same Environment.
	mu               sync.Mutex
	ownedNodePools   map[string]struct{}
	ownedNodeClasses map[string]struct{}
}

// NewEnvironment reads config from env vars, creates k8s clients, and waits
// for the karpenter Deployment (installed by e2e-setup.sh via Helm) to be ready.
//
// Required env vars: PROJECT_ID, CLUSTER_NAME, CLUSTER_LOCATION, PODS_RANGE_NAME
// The kubeconfig must already point at the e2e cluster (set by e2e-setup.sh).
func NewEnvironment() *Environment {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred(), "loading kubeconfig")

	kubeClient, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred(), "creating kubernetes client")

	dynamicClient, err := dynamic.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred(), "creating dynamic client")

	initCtx, initCancel := context.WithTimeout(context.Background(), ControllerStartTimeout)
	defer initCancel()
	containerSvc, err := container.NewService(initCtx)
	Expect(err).NotTo(HaveOccurred(), "creating GCP container service client")

	computeSvc, err := compute.NewService(initCtx)
	Expect(err).NotTo(HaveOccurred(), "creating GCP compute service client")

	env := &Environment{
		ProjectID:        mustEnv("PROJECT_ID"),
		ClusterName:      mustEnv("CLUSTER_NAME"),
		ClusterLocation:  mustEnv("CLUSTER_LOCATION"),
		PodsRangeName:    mustEnv("PODS_RANGE_NAME"),
		KubeClient:       kubeClient,
		DynamicClient:    dynamicClient,
		containerSvc:     containerSvc,
		computeSvc:       computeSvc,
		ownedNodePools:   make(map[string]struct{}),
		ownedNodeClasses: make(map[string]struct{}),
	}

	// Fast-fail: verify the cluster exists at the configured location before
	// entering any Eventually loop. Without this, a wrong CLUSTER_LOCATION
	// silently times out for up to NodePoolReadyTimeout minutes and makes all
	// specs appear to pass (0 specs ran = no failures in ginkgo).
	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		env.ProjectID, env.ClusterLocation, env.ClusterName)
	if _, err := containerSvc.Projects.Locations.Clusters.Get(clusterPath).Context(initCtx).Do(); err != nil {
		panic(fmt.Sprintf(
			"cluster %q not reachable at location %q (PROJECT_ID=%s) — check CLUSTER_LOCATION env var (E2E_ZONE in Makefile): %v",
			env.ClusterName, env.ClusterLocation, env.ProjectID, err,
		))
	}

	env.waitForControllerReady()
	return env
}

// trackNodePool records a NodePool name as owned by this Environment instance.
// Must be called after successfully creating a NodePool.
func (e *Environment) trackNodePool(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownedNodePools[name] = struct{}{}
}

// trackNodeClass records a GCENodeClass name as owned by this Environment instance.
// Must be called after successfully creating a GCENodeClass.
func (e *Environment) trackNodeClass(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownedNodeClasses[name] = struct{}{}
}

// Cleanup removes karpenter resources created by this Environment and waits
// for their NodeClaims to drain. Scoped to owned resources so that parallel
// suite processes do not interfere with each other.
func (e *Environment) Cleanup() {
	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), NodeCleanupTimeout)
	defer deleteCancel()

	e.mu.Lock()
	pools := make([]string, 0, len(e.ownedNodePools))
	for name := range e.ownedNodePools {
		pools = append(pools, name)
	}
	classes := make([]string, 0, len(e.ownedNodeClasses))
	for name := range e.ownedNodeClasses {
		classes = append(classes, name)
	}
	e.mu.Unlock()

	for _, name := range pools {
		err := e.DynamicClient.Resource(NodePoolGVR).Delete(deleteCtx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "cleanup: deleting NodePool %s", name)
		}
	}
	for _, name := range classes {
		err := e.DynamicClient.Resource(GCENodeClassGVR).Delete(deleteCtx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "cleanup: deleting GCENodeClass %s", name)
		}
	}

	if len(pools) == 0 {
		return
	}

	// Wait only for NodeClaims belonging to this suite's NodePools to drain.
	// This avoids blocking on NodeClaims from concurrently running suites.
	poolSet := make(map[string]struct{}, len(pools))
	for _, p := range pools {
		poolSet[p] = struct{}{}
	}
	Eventually(func(g Gomega) {
		claims, err := e.DynamicClient.Resource(nodeClaimGVR).List(deleteCtx, metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		for _, c := range claims.Items {
			poolName := c.GetLabels()[karpv1.NodePoolLabelKey]
			if _, owned := poolSet[poolName]; owned {
				g.Expect(false).To(BeTrue(),
					"NodeClaim %s (nodepool=%s) not yet cleaned up — possible orphaned GCP VM",
					c.GetName(), poolName)
			}
		}
	}).WithTimeout(NodeCleanupTimeout).WithPolling(10 * time.Second).Should(Succeed())
}

// WaitForNodeRemoval polls until the named node no longer exists. Transient API
// errors are retried; the caller's context is the sole deadline authority.
func (e *Environment) WaitForNodeRemoval(ctx context.Context, nodeName string) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := e.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			// Treat all errors (transient network, throttling) as retryable.
			return false, nil
		},
	)
}

// ListNodeClaims returns all NodeClaim objects in the cluster.
func (e *Environment) ListNodeClaims(ctx context.Context) ([]unstructured.Unstructured, error) {
	list, err := e.DynamicClient.Resource(nodeClaimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// waitForControllerReady polls until the karpenter Deployment has all replicas
// available and both GKE template node pools are RUNNING.
// The Deployment was installed by e2e-setup.sh via Helm.
func (e *Environment) waitForControllerReady() {
	// Each phase gets its own independent 5-minute budget so a slow deployment
	// start does not eat into the pool-readiness window.
	deployCtx, deployCancel := context.WithTimeout(context.Background(), ControllerStartTimeout)
	defer deployCancel()

	Eventually(func(g Gomega) {
		dep, err := e.KubeClient.AppsV1().Deployments(KarpenterNamespace).
			Get(deployCtx, KarpenterDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred(),
			"getting karpenter Deployment — did e2e-setup.sh run successfully?")
		g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", 1),
			"karpenter Deployment has no available replicas yet")
	}).WithTimeout(ControllerStartTimeout).WithPolling(5 * time.Second).Should(Succeed())

	// Wait for karpenter's template node pools to reach RUNNING state, which
	// confirms the controller has initialized and the GKE node pool templates
	// are ready to back instance provisioning. Without this, GetInstanceTemplates
	// returns an empty map and every provisioning attempt fails immediately.
	// Both pools are checked in a single Eventually so they poll concurrently
	// rather than blocking sequentially on each one.
	poolCtx, poolCancel := context.WithTimeout(context.Background(), NodePoolReadyTimeout)
	defer poolCancel()
	Eventually(func(g Gomega) {
		for _, poolName := range []string{"karpenter-default", "karpenter-ubuntu"} {
			poolPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s/nodePools/%s",
				e.ProjectID, e.ClusterLocation, e.ClusterName, poolName)
			pool, err := e.containerSvc.Projects.Locations.Clusters.NodePools.
				Get(poolPath).Context(poolCtx).Do()
			g.Expect(err).NotTo(HaveOccurred(), "getting GKE node pool %s", poolName)
			g.Expect(pool.Status).To(Equal("RUNNING"),
				"GKE node pool %s is not RUNNING (status=%s)", poolName, pool.Status)
		}
	}).WithTimeout(NodePoolReadyTimeout).WithPolling(10 * time.Second).Should(Succeed())
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}
