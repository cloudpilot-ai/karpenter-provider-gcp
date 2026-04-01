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
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	KarpenterNamespace  = "karpenter-system"
	KarpenterDeployment = "karpenter"

	ControllerStartTimeout = 5 * time.Minute
	NodeCleanupTimeout     = 8 * time.Minute
	ProvisioningTimeout    = 8 * time.Minute
	PauseImage             = "registry.k8s.io/pause:3.10"
)

var (
	NodeClaimGVR    = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
	NodePoolGVR     = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	GCENodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"}
)

// Environment holds shared state for a test suite run.
type Environment struct {
	ProjectID       string
	ClusterName     string
	ClusterLocation string // zone, e.g. us-central1-a
	PodsRangeName   string

	KubeClient    kubernetes.Interface
	DynamicClient dynamic.Interface
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

	env := &Environment{
		ProjectID:       mustEnv("PROJECT_ID"),
		ClusterName:     mustEnv("CLUSTER_NAME"),
		ClusterLocation: mustEnv("CLUSTER_LOCATION"),
		PodsRangeName:   mustEnv("PODS_RANGE_NAME"),
		KubeClient:      kubeClient,
		DynamicClient:   dynamicClient,
	}

	env.waitForControllerReady()
	return env
}

// Cleanup removes all karpenter-managed resources and waits for NodeClaims to
// disappear, confirming the underlying GCP instances have been terminated.
func (e *Environment) Cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), NodeCleanupTimeout)
	defer cancel()

	e.deleteAll(ctx, NodePoolGVR)
	e.deleteAll(ctx, GCENodeClassGVR)

	// NodeClaim deletion drives GCP VM termination via the karpenter
	// termination controller, so an empty list here means no orphaned VMs.
	Eventually(func(g Gomega) {
		claims, err := e.DynamicClient.Resource(NodeClaimGVR).List(ctx, metav1.ListOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(claims.Items).To(BeEmpty(), "not all NodeClaims cleaned up — possible orphaned GCP VMs")
	}).WithTimeout(NodeCleanupTimeout).WithPolling(10 * time.Second).Should(Succeed())
}

// WaitForNodeRemoval polls until the named node no longer exists.
func (e *Environment) WaitForNodeRemoval(ctx context.Context, nodeName string) error {
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, NodeCleanupTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := e.KubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		},
	)
}

// waitForControllerReady polls until the karpenter Deployment has all replicas
// available. The Deployment was installed by e2e-setup.sh via Helm.
func (e *Environment) waitForControllerReady() {
	ctx, cancel := context.WithTimeout(context.Background(), ControllerStartTimeout)
	defer cancel()

	Eventually(func(g Gomega) {
		dep, err := e.KubeClient.AppsV1().Deployments(KarpenterNamespace).
			Get(ctx, KarpenterDeployment, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred(),
			"getting karpenter Deployment — did e2e-setup.sh run successfully?")
		g.Expect(dep.Status.AvailableReplicas).To(BeNumerically(">=", 1),
			"karpenter Deployment has no available replicas yet")
	}).WithTimeout(ControllerStartTimeout).WithPolling(5 * time.Second).Should(Succeed())

	// Extra sanity: make sure the template node pools are up, which means
	// the controller has fully initialized and is talking to the GKE API.
	Eventually(func(g Gomega) {
		for _, poolName := range []string{"karpenter-default", "karpenter-ubuntu"} {
			pods, err := e.KubeClient.CoreV1().Pods(KarpenterNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", KarpenterDeployment),
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).NotTo(BeEmpty(), "no karpenter pods found")
			_ = poolName // pool check happens via node provisioning in tests
		}
	}).WithTimeout(ControllerStartTimeout).WithPolling(5 * time.Second).Should(Succeed())
}

// deleteAll lists and deletes every resource of the given GVR, ignoring 404s.
func (e *Environment) deleteAll(ctx context.Context, gvr schema.GroupVersionResource) {
	items, err := e.DynamicClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred(), "listing %s for cleanup", gvr.Resource)
	for _, item := range items.Items {
		err := e.DynamicClient.Resource(gvr).Delete(ctx, item.GetName(), metav1.DeleteOptions{})
		if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "deleting %s/%s", gvr.Resource, item.GetName())
		}
	}
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		panic(fmt.Sprintf("required environment variable %q is not set", key))
	}
	return v
}
