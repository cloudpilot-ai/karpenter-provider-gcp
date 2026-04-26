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
	corev1 "k8s.io/api/core/v1"
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

	// Timeouts — ordered from shortest to longest.
	NodePoolReadyTimeout   = 3 * time.Minute         // Karpenter NodePool Ready condition
	NodeClassReadyTimeout  = 3 * time.Minute         // GCENodeClass Ready condition
	NodeClaimLaunchTimeout = 3 * time.Minute         // NodeClaim reaches Launched=True
	NodeCleanupTimeout     = 3 * time.Minute         // karpenter-provisioned VM terminated after NodePool deletion
	ControllerStartTimeout = 5 * time.Minute         // karpenter controller Deployment becomes available
	ProvisioningTimeout    = 10 * time.Minute        // VM created, booted, registered, pod Running
	ReplacementTimeout     = 2 * ProvisioningTimeout // node replacement (drift, expiration): drain + reprovision
	// GPUProvisioningTimeout is longer than ProvisioningTimeout to account for
	// GPU driver installation and NVIDIA device plugin startup before the
	// nvidia.com/gpu resource becomes allocatable.
	GPUProvisioningTimeout = 20 * time.Minute
	// GKENodePoolReadyTimeout is for GKE template node pools reaching RUNNING
	// state, which can take up to 10 minutes on first start.
	GKENodePoolReadyTimeout = 10 * time.Minute

	// DefaultPollInterval is the Gomega Eventually polling cadence used across all e2e waits.
	DefaultPollInterval = 5 * time.Second

	PauseImage = "registry.k8s.io/pause:3.10"
)

var (
	// Access NodeClaims through ListNodeClaims rather than this GVR directly.
	nodeClaimGVR    = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	nodePoolGVR     = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	gceNodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"}
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
	// silently times out for up to GKENodePoolReadyTimeout minutes and makes all
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
	env.ensureTestNamespace()
	return env
}

func (e *Environment) ensureTestNamespace() {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: TestNamespace}}
	_, err := e.KubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred(), "creating test namespace %s", TestNamespace)
	}
}

// Must be called after successfully creating a NodePool so that Cleanup deletes it.
func (e *Environment) trackNodePool(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ownedNodePools[name] = struct{}{}
}

// Must be called after successfully creating a GCENodeClass so that Cleanup deletes it.
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
		err := e.DynamicClient.Resource(nodePoolGVR).Delete(deleteCtx, name, metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "cleanup: deleting NodePool %s", name)
		}
	}
	for _, name := range classes {
		err := e.DynamicClient.Resource(gceNodeClassGVR).Delete(deleteCtx, name, metav1.DeleteOptions{})
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
	}).WithTimeout(NodeCleanupTimeout).WithPolling(DefaultPollInterval).Should(Succeed())
}

// WaitForNodeRemoval polls until the named node no longer exists. Transient
// errors (network, throttling) are retried; permanent errors (auth, forbidden)
// are returned immediately. The caller's context is the sole deadline authority.
func (e *Environment) WaitForNodeRemoval(ctx context.Context, nodeName string) error {
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true,
		func(ctx context.Context) (bool, error) {
			_, err := e.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
				return false, err
			}
			return false, nil
		},
	)
}

// ListNodeClaims uses the dynamic client because the typed karpenter client is not available in tests.
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
	deployCtx, deployCancel := context.WithTimeout(context.Background(), ControllerStartTimeout)
	defer deployCancel()

	Eventually(func(g Gomega) {
		dep, err := e.KubeClient.AppsV1().Deployments(KarpenterNamespace).
			Get(deployCtx, KarpenterDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred(),
			"getting karpenter Deployment — did e2e-setup.sh run successfully?")
		g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", 1),
			"karpenter Deployment has no available replicas yet")
	}).WithTimeout(ControllerStartTimeout).WithPolling(DefaultPollInterval).Should(Succeed())

	// Wait for at least one RUNNING node pool, which confirms the Karpenter
	// nodepooltemplate controller has completed pool discovery and bootstrap metadata
	// is available for instance provisioning.
	poolCtx, poolCancel := context.WithTimeout(context.Background(), GKENodePoolReadyTimeout)
	defer poolCancel()
	clusterPath := fmt.Sprintf("projects/%s/locations/%s/clusters/%s",
		e.ProjectID, e.ClusterLocation, e.ClusterName)
	Eventually(func(g Gomega) {
		resp, err := e.containerSvc.Projects.Locations.Clusters.NodePools.
			List(clusterPath).Context(poolCtx).Do()
		g.Expect(err).NotTo(HaveOccurred(), "listing GKE node pools")
		var running int
		for _, pool := range resp.NodePools {
			if pool.Status == "RUNNING" || pool.Status == "RUNNING_WITH_ERROR" {
				running++
			}
		}
		g.Expect(running).To(BeNumerically(">=", 1),
			"expected at least one RUNNING node pool for bootstrap source discovery")
	}).WithTimeout(GKENodePoolReadyTimeout).WithPolling(DefaultPollInterval).Should(Succeed())
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}
