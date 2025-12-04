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

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
	metricsV1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cloudprovider"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/controllers/interruption"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	defaultTelemetryEndpoint = "https://api.cloudpilot.ai/api/v1/telemetry"
	resourcesInfoPath        = "/clusters/%s/resourcesInfo"
	interruptionEventPath    = "/clusters/%s/interruptionevent"
)

type Controller struct {
	clusterIDHash string

	kubeClient   client.Client
	metricClient metricsclientset.Interface

	once sync.Once

	client http.Client
}

func NewController(kubeClient client.Client, metricClient metricsclientset.Interface) *Controller {
	return &Controller{
		kubeClient:   kubeClient,
		metricClient: metricClient,
		once:         sync.Once{},

		client: http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Controller) Reconcile(ctx context.Context, node *corev1.Node) (reconcile.Result, error) {
	c.once.Do(func() {
		clusterIDHash, err := c.GetClusterIDHash()
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to get cluster ID hash for telemetry")
			return
		}

		c.PushTelemetryClusterResourceInfo(ctx, clusterIDHash, c.kubeClient, c.metricClient)
	})

	if interruption.IsNodeInterrupted(node) {
		clusterIDHash, err := c.GetClusterIDHash()
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to get cluster ID hash for telemetry")
			return reconcile.Result{}, nil
		}
		c.PushInterruptionEvent(ctx, clusterIDHash, node)
		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

func (c *Controller) GetClusterIDHash() (string, error) {
	if c.clusterIDHash != "" {
		return c.clusterIDHash, nil
	}

	clusterID, err := getClusterID(context.Background(), c.kubeClient)
	if err != nil {
		return "", err
	}
	c.clusterIDHash = utils.Hash(clusterID)
	return c.clusterIDHash, nil
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("telemetry").
		For(&corev1.Node{}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

type ClusterResourcesInfo struct {
	CPUUsage    int64 `json:"cpuUsage"`
	MemoryUsage int64 `json:"memoryUsage"`

	CPURequest    int64 `json:"cpuRequest"`
	MemoryRequest int64 `json:"memoryRequest"`

	CPUCapacity    int64 `json:"cpuCapacity"`
	MemoryCapacity int64 `json:"memoryCapacity"`

	InstanceTypes []string `json:"instanceTypes"`
}

func (c *Controller) PushTelemetryClusterResourceInfo(ctx context.Context, clusterIDHash string, kubeclient client.Client, metricClient metricsclientset.Interface) {
	var (
		cpuUsage       = int64(0)
		memoryUsage    = int64(0)
		cpuCapacity    = int64(0)
		memoryCapacity = int64(0)
		cpuRequest     = int64(0)
		memoryRequest  = int64(0)
		instanceTypes  = []string{}
		wg             = sync.WaitGroup{}
	)

	wg.Add(3)
	go func() {
		defer wg.Done()
		cpuCapacity, memoryCapacity, instanceTypes = getClusterResourcesCapacity(ctx, kubeclient)
	}()

	go func() {
		defer wg.Done()
		cpuRequest, memoryRequest = getClusterResourcesRequest(ctx, kubeclient)
	}()

	go func() {
		defer wg.Done()
		cpuUsage, memoryUsage = getClusterResourcesUsage(ctx, metricClient)
	}()

	wg.Wait()

	clusterResourcesInfo := ClusterResourcesInfo{
		CPUUsage:       cpuUsage,
		MemoryUsage:    memoryUsage,
		CPURequest:     cpuRequest,
		MemoryRequest:  memoryRequest,
		CPUCapacity:    cpuCapacity,
		MemoryCapacity: memoryCapacity,
		InstanceTypes:  instanceTypes,
	}

	data, err := json.Marshal(clusterResourcesInfo)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to marshal cluster resource info")
		return
	}

	push(ctx, c.client, fmt.Sprintf(defaultTelemetryEndpoint+resourcesInfoPath, clusterIDHash), data)
}

type InterruptionEvent struct {
	NodeHashID              string    `json:"nodeHashID"`
	CloudProvider           string    `json:"cloudProvider"`
	Timestamp               time.Time `json:"timestamp"`
	Region                  string    `json:"region"`
	Zone                    string    `json:"zone"`
	InstanceType            string    `json:"instanceType"`
	InstanceCreateTimestamp time.Time `json:"instanceCreateTimestamp"`
}

func (c *Controller) PushInterruptionEvent(ctx context.Context, clusterIDHash string, node *corev1.Node) {
	data, err := json.Marshal(InterruptionEvent{
		NodeHashID:              utils.Hash(string(node.UID)),
		CloudProvider:           cloudprovider.CloudProviderName,
		Timestamp:               time.Now(),
		Region:                  node.Labels[corev1.LabelZoneRegion],
		Zone:                    node.Labels[corev1.LabelZoneFailureDomain],
		InstanceType:            node.Labels[corev1.LabelInstanceTypeStable],
		InstanceCreateTimestamp: node.CreationTimestamp.Time,
	})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to marshal spot interruption event")
		return
	}

	push(ctx, c.client, fmt.Sprintf(defaultTelemetryEndpoint+interruptionEventPath, clusterIDHash), data)
}

func getClusterResourcesCapacity(ctx context.Context, kubeclient client.Client) (cpuCapacity, memoryCapacity int64, instanceTypes []string) {
	nodeList := &corev1.NodeList{}

	if err := kubeclient.List(ctx, nodeList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list nodes")
		return
	}

	nodes := make([]corev1.Node, 0)
	for ni := range nodeList.Items {
		if _, ok := nodeList.Items[ni].Labels[karpv1.NodeInitializedLabelKey]; !ok {
			nodes = append(nodes, nodeList.Items[ni])
		}
	}

	instanceTypeM := make(map[string]struct{})
	for ni := range nodes {
		cpuCapacity += nodes[ni].Status.Capacity.Cpu().MilliValue()
		memoryCapacity += nodes[ni].Status.Capacity.Memory().Value()

		if instanceType, ok := nodes[ni].Labels[corev1.LabelInstanceTypeStable]; ok && instanceType != "" {
			instanceTypeM[instanceType] = struct{}{}
		}
	}

	instanceTypes = lo.Keys(instanceTypeM)
	return
}

func getClusterResourcesRequest(ctx context.Context, kubeclient client.Client) (cpuRequest, memoryRequest int64) {
	podList := &corev1.PodList{}
	if err := kubeclient.List(ctx, podList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list pods")
		return
	}

	pods := lo.Map(podList.Items, func(pod corev1.Pod, _ int) *corev1.Pod {
		return &pod
	})

	resourcesList := resources.RequestsForPods(pods...)

	cpuRequest = resourcesList.Cpu().MilliValue()
	memoryRequest = resourcesList.Memory().Value()

	return
}

func getClusterResourcesUsage(ctx context.Context, metricClient metricsclientset.Interface) (cpuUsage, memoryUsage int64) {
	metrics, err := getNodeMetricsFromMetricsAPI(metricClient)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get node metrics")
		return
	}

	for mi := range metrics.Items {
		cpuUsage += metrics.Items[mi].Usage.Cpu().MilliValue()
		memoryUsage += metrics.Items[mi].Usage.Memory().Value()
	}

	return
}

func getNodeMetricsFromMetricsAPI(metricsClient metricsclientset.Interface) (*metricsapi.NodeMetricsList, error) {
	var err error

	versionedMetrics, err := metricsClient.MetricsV1beta1().NodeMetricses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	metrics := &metricsapi.NodeMetricsList{}
	if err := metricsV1beta1api.Convert_v1beta1_NodeMetricsList_To_metrics_NodeMetricsList(versionedMetrics, metrics, nil); err != nil {
		return nil, err
	}
	return metrics, nil
}

func push(ctx context.Context, client http.Client, urlPath string, data []byte) {
	req, err := http.NewRequest(http.MethodPost, urlPath, bytes.NewBuffer(data))
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create telemetry request")
	}

	if _, err := client.Do(req); err != nil {
		log.FromContext(ctx).Error(err, "failed to push telemetry data")
		return
	}
}

func getClusterID(ctx context.Context, kubeClient client.Client) (string, error) {
	var ns corev1.Namespace
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: metav1.NamespaceSystem}, &ns); err != nil {
		return "", err
	}

	return string(ns.UID), nil
}
