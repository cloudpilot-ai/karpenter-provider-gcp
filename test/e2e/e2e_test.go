package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
)

const (
	provisioningTimeout = 5 * time.Minute
	cleanupTimeout      = 5 * time.Minute
	pauseImage          = "registry.k8s.io/pause:3.10"
)

var (
	gceNodeClassGVR = schema.GroupVersionResource{Group: "karpenter.k8s.gcp", Version: "v1alpha1", Resource: "gcenodeclasses"}
	nodePoolGVR     = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
)

type provisioningCase struct {
	name         string
	capacityType string
	arch         string
	families     []string
}

func TestE2E(t *testing.T) {
	for _, tc := range append(onDemandCases(), spotCases()...) {
		t.Run(tc.name, func(t *testing.T) {
			runProvisioningTest(t, tc)
		})
	}
}

func runProvisioningTest(t *testing.T, tc provisioningCase) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	baseName := resourceName(testEnv.prefix, tc.name)
	nodeClassName := resourceName(baseName, "class")
	nodePoolName := resourceName(baseName, "pool")
	deploymentName := resourceName(baseName, "deploy")
	appLabel := resourceName(baseName, "app")

	initialNodes, err := allNodeNames(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		deleteDeployment(cleanupCtx, deploymentName)
		deleteNodePool(cleanupCtx, nodePoolName)
		deleteNodeClass(cleanupCtx, nodeClassName)
	})

	require.NoError(t, createNodeClass(ctx, nodeClassName))
	require.NoError(t, createNodePool(ctx, nodePoolName, nodeClassName, tc))
	require.NoError(t, createDeployment(ctx, deploymentName, appLabel, nodePoolName))

	pod, err := waitForRunningPod(ctx, appLabel)
	require.NoError(t, err)
	require.NotEmpty(t, pod.Spec.NodeName)

	node, err := testEnv.clientset.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	require.NoError(t, err)

	_, existedBefore := initialNodes[node.Name]
	assert.False(t, existedBefore, "expected a new node to be provisioned")
	assert.Equal(t, "true", node.Labels[karpv1.NodeRegisteredLabelKey])
	assert.Equal(t, nodePoolName, node.Labels[karpv1.NodePoolLabelKey])
	assert.Equal(t, tc.capacityType, node.Labels[karpv1.CapacityTypeLabelKey])
	assert.Equal(t, tc.arch, node.Labels[corev1.LabelArchStable])
	assert.Contains(t, tc.families, node.Labels[gcpv1alpha1.LabelInstanceFamily])

	require.NoError(t, deleteDeployment(ctx, deploymentName))
	require.NoError(t, waitForNodeRemoval(ctx, node.Name))
	require.NoError(t, deleteNodePool(ctx, nodePoolName))
	require.NoError(t, deleteNodeClass(ctx, nodeClassName))
}

func createNodeClass(ctx context.Context, name string) error {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.k8s.gcp/v1alpha1",
		"kind":       "GCENodeClass",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"imageSelectorTerms": []any{
				map[string]any{"alias": "ContainerOptimizedOS@latest"},
			},
			"disks": []any{
				map[string]any{
					"category": "pd-balanced",
					"sizeGiB":  int64(60),
					"boot":     true,
				},
			},
		},
	}}

	_, err := testEnv.dynamicClient.Resource(gceNodeClassGVR).Create(ctx, object, metav1.CreateOptions{})
	return err
}

func createNodePool(ctx context.Context, name, nodeClassName string, tc provisioningCase) error {
	requirements := []any{
		map[string]any{
			"key":      karpv1.CapacityTypeLabelKey,
			"operator": "In",
			"values":   []any{tc.capacityType},
		},
		map[string]any{
			"key":      gcpv1alpha1.LabelInstanceFamily,
			"operator": "In",
			"values":   stringSliceToAny(tc.families),
		},
		map[string]any{
			"key":      corev1.LabelArchStable,
			"operator": "In",
			"values":   []any{tc.arch},
		},
		map[string]any{
			"key":      corev1.LabelTopologyZone,
			"operator": "In",
			"values":   stringSliceToAny(testEnv.zones),
		},
	}

	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "karpenter.sh/v1",
		"kind":       "NodePool",
		"metadata": map[string]any{
			"name": name,
		},
		"spec": map[string]any{
			"weight": int64(10),
			"disruption": map[string]any{
				"consolidateAfter":    "30s",
				"consolidationPolicy": "WhenEmptyOrUnderutilized",
				"budgets": []any{
					map[string]any{"nodes": "100%"},
				},
			},
			"template": map[string]any{
				"spec": map[string]any{
					"nodeClassRef": map[string]any{
						"name":  nodeClassName,
						"kind":  "GCENodeClass",
						"group": "karpenter.k8s.gcp",
					},
					"requirements": requirements,
				},
			},
		},
	}}

	_, err := testEnv.dynamicClient.Resource(nodePoolGVR).Create(ctx, object, metav1.CreateOptions{})
	return err
}

func createDeployment(ctx context.Context, name, appLabel, nodePoolName string) error {
	replicas := int32(1)
	zero := int64(0)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appLabel}},
				Spec: corev1.PodSpec{
					NodeSelector:                  map[string]string{karpv1.NodePoolLabelKey: nodePoolName},
					TerminationGracePeriodSeconds: &zero,
					Containers: []corev1.Container{
						{
							Name:  "inflate",
							Image: pauseImage,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("250m"),
									corev1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := testEnv.clientset.AppsV1().Deployments(testNamespace).Create(ctx, deployment, metav1.CreateOptions{})
	return err
}

func waitForRunningPod(ctx context.Context, appLabel string) (*corev1.Pod, error) {
	var runningPod *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, provisioningTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := testEnv.clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", appLabel)})
		if err != nil {
			return false, err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Status.Phase == corev1.PodRunning && pod.Spec.NodeName != "" {
				runningPod = pod.DeepCopy()
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return runningPod, nil
}

func waitForNodeRemoval(ctx context.Context, nodeName string) error {
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, cleanupTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := testEnv.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func deleteDeployment(ctx context.Context, name string) error {
	err := testEnv.clientset.AppsV1().Deployments(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func deleteNodePool(ctx context.Context, name string) error {
	err := testEnv.dynamicClient.Resource(nodePoolGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func deleteNodeClass(ctx context.Context, name string) error {
	err := testEnv.dynamicClient.Resource(gceNodeClassGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func allNodeNames(ctx context.Context) (map[string]struct{}, error) {
	nodes, err := testEnv.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{}, len(nodes.Items))
	for _, node := range nodes.Items {
		result[node.Name] = struct{}{}
	}
	return result, nil
}

func stringSliceToAny(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func resourceName(parts ...string) string {
	joined := strings.ToLower(strings.Join(parts, "-"))
	joined = strings.NewReplacer("_", "-", "/", "-", ".", "-").Replace(joined)
	var builder strings.Builder
	for _, ch := range joined {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			builder.WriteRune(ch)
		}
	}
	base := strings.Trim(builder.String(), "-")
	if base == "" {
		base = "e2e"
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	maxBaseLen := max(1, 63-len(suffix)-1)
	if len(base) > maxBaseLen {
		base = strings.Trim(base[:maxBaseLen], "-")
	}
	if base == "" {
		base = "e2e"
	}
	return fmt.Sprintf("%s-%s", base, suffix)
}
