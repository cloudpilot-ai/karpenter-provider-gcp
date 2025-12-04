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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	metricsapi "k8s.io/metrics/pkg/apis/metrics"
	metricsV1beta1api "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/node"
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

	interruptionEvents sync.Map // map[string]InterruptionEvent{}

	once sync.Once

	client http.Client
}

func NewController(kubeClient client.Client, metricClient metricsclientset.Interface) *Controller {
	return &Controller{
		kubeClient:   kubeClient,
		metricClient: metricClient,

		interruptionEvents: sync.Map{},

		once: sync.Once{},

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

		log.FromContext(ctx, "clusterIDHash", clusterIDHash).Info("starting telemetry controller")

		c.PushTelemetryClusterResourceInfo(ctx, clusterIDHash, c.kubeClient, c.metricClient)

		go c.Run(ctx)
	})

	interrupted, err := IsNodeInterrupted(ctx, c.kubeClient, node)
	if err != nil {
		// Don't fail reconciliation on this error; just log and move on.
		log.FromContext(ctx).Error(err, "failed to check if node is interrupted")
		return reconcile.Result{}, nil
	}

	if interrupted {
		c.InsertInterruptionEvent(utils.Hash(string(node.GetUID())), newInterruptionEvent(node))
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

const telemetryInterval = 10 * time.Second

func (c *Controller) Run(ctx context.Context) {
	clusterIDHash, err := c.GetClusterIDHash()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to get cluster ID hash for telemetry")
		return
	}

	handler := func(ctx context.Context) {
		events := c.GetReadyInterruptionEvents()

		workqueue.ParallelizeUntil(ctx, 10, len(events), func(piece int) {
			c.PushInterruptionEvent(ctx, clusterIDHash, events[piece])
		})
	}

	wait.UntilWithContext(ctx, handler, telemetryInterval)
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

	if err := push(ctx, c.client, fmt.Sprintf(defaultTelemetryEndpoint+resourcesInfoPath, clusterIDHash), data); err != nil {
		log.FromContext(ctx).Error(err, "failed to push cluster resource info")
		return
	}
}

type InterruptionEvent struct {
	CloudProvider           string    `json:"cloudProvider"`
	Timestamp               time.Time `json:"timestamp"`
	Region                  string    `json:"region"`
	Zone                    string    `json:"zone"`
	InstanceType            string    `json:"instanceType"`
	InstanceCreateTimestamp time.Time `json:"instanceCreateTimestamp"`

	nodeHashID            string
	latestUpdateTimestamp time.Time
}

func (c *Controller) InsertInterruptionEvent(nodeHashID string, event *InterruptionEvent) {
	actual, loaded := c.interruptionEvents.LoadOrStore(nodeHashID, *event)
	if !loaded {
		return
	}

	existingEvent := actual.(InterruptionEvent)
	existingEvent.Timestamp = lo.Ternary(event.Timestamp.Before(existingEvent.Timestamp), event.Timestamp, existingEvent.Timestamp)
	existingEvent.latestUpdateTimestamp = lo.Ternary(event.latestUpdateTimestamp.After(existingEvent.latestUpdateTimestamp), event.latestUpdateTimestamp, existingEvent.latestUpdateTimestamp)

	c.interruptionEvents.Store(nodeHashID, existingEvent)
}

const interruptionEventExpiryDuration = 30 * time.Second

func (c *Controller) GetReadyInterruptionEvents() (readyEvents []InterruptionEvent) {
	c.interruptionEvents.Range(func(key, value any) bool {
		event := value.(InterruptionEvent)
		if time.Since(event.latestUpdateTimestamp) > interruptionEventExpiryDuration {
			readyEvents = append(readyEvents, event)
		}
		return true
	})
	return
}

func (c *Controller) PushInterruptionEvent(ctx context.Context, clusterIDHash string, event InterruptionEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to marshal spot interruption event")
		return
	}

	if err := push(ctx, c.client, fmt.Sprintf(defaultTelemetryEndpoint+interruptionEventPath, clusterIDHash), data); err != nil {
		log.FromContext(ctx).Error(err, "failed to push spot interruption event")
		return
	}

	c.interruptionEvents.Delete(event.nodeHashID)
}

func getClusterResourcesCapacity(ctx context.Context, kubeclient client.Client) (cpuCapacity, memoryCapacity int64, instanceTypes []string) {
	nodeList := &corev1.NodeList{}

	if err := kubeclient.List(ctx, nodeList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list nodes")
		return
	}

	nodes := make([]corev1.Node, 0)
	for ni := range nodeList.Items {
		if v, ok := nodeList.Items[ni].Labels[karpv1.NodeInitializedLabelKey]; ok && v == "true" {
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

func push(ctx context.Context, client http.Client, urlPath string, data []byte) error {
	req, err := http.NewRequest(http.MethodPost, urlPath, bytes.NewBuffer(data))
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create telemetry request")
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to push telemetry data")
		return err
	}
	defer resp.Body.Close()

	return nil
}

func newInterruptionEvent(node *corev1.Node) *InterruptionEvent {
	now := time.Now()
	return &InterruptionEvent{
		CloudProvider:           cloudprovider.CloudProviderName,
		Timestamp:               now,
		Region:                  node.Labels[corev1.LabelZoneRegion],
		Zone:                    node.Labels[corev1.LabelZoneFailureDomain],
		InstanceType:            node.Labels[corev1.LabelInstanceTypeStable],
		InstanceCreateTimestamp: node.CreationTimestamp.Time,

		nodeHashID:            utils.Hash(string(node.GetUID())),
		latestUpdateTimestamp: now,
	}
}

func getClusterID(ctx context.Context, kubeClient client.Client) (string, error) {
	var ns corev1.Namespace
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: metav1.NamespaceSystem}, &ns); err != nil {
		return "", err
	}

	return string(ns.UID), nil
}

const disruptionWindow = 5 * time.Minute

func IsNodeInterrupted(ctx context.Context, client client.Client, currentNode *corev1.Node) (bool, error) {
	if !IsNodeShuttingDown(currentNode) {
		return false, nil
	}

	if currentNode.Labels == nil {
		return false, nil
	}

	if currentNode.Labels[karpv1.NodeInitializedLabelKey] != "true" ||
		currentNode.Labels[karpv1.CapacityTypeLabelKey] != karpv1.CapacityTypeSpot {
		return false, nil
	}

	nodeClaim, err := node.NodeClaimForNode(ctx, client, currentNode)
	if err != nil {
		return false, fmt.Errorf("getting nodeclaim for node %s: %w", currentNode.Name, err)
	}

	now := time.Now()
	for _, condition := range nodeClaim.Status.Conditions {
		if condition.Type == karpv1.ConditionTypeDisruptionReason &&
			now.Sub(condition.LastTransitionTime.Time) < disruptionWindow {
			return false, nil
		}
	}

	// No recent DisruptionReason → treat as an interruption
	return true, nil
}

func IsNodeShuttingDown(currentNode *corev1.Node) bool {
	if currentNode == nil {
		return false
	}

	condition := node.GetCondition(currentNode, corev1.NodeReady)
	return condition.Status != corev1.ConditionTrue &&
		condition.Reason == interruption.NodeConditionReasonKubeletNotReady &&
		condition.Message == interruption.NodeConditionMessageShuttingDown
}
