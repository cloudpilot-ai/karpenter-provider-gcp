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

package main

import (
	"context"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

type ResourceSample struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

func collectControllerMetrics(ctx context.Context) (MetricCoverage, []ResourceSample) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return MetricCoverage{Reason: fmt.Sprintf("loading kubeconfig: %v", err)}, nil
	}
	client, err := metricsclient.NewForConfig(config)
	if err != nil {
		return MetricCoverage{Reason: fmt.Sprintf("creating metrics client: %v", err)}, nil
	}
	items, err := client.MetricsV1beta1().PodMetricses("karpenter-system").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=karpenter"})
	if err != nil {
		return MetricCoverage{Reason: fmt.Sprintf("collecting controller metrics: %v", err)}, nil
	}
	var samples []ResourceSample
	for _, pod := range items.Items {
		for _, container := range pod.Containers {
			samples = append(samples, ResourceSample{CPU: container.Usage.Cpu().String(), Memory: container.Usage.Memory().String()})
		}
	}
	if len(samples) == 0 {
		return MetricCoverage{Reason: "metrics API returned no Karpenter controller samples"}, nil
	}
	return MetricCoverage{Available: true}, samples
}
