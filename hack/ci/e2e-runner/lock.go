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
	"path/filepath"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type ownershipLock struct {
	path      string
	leaseName string
	client    kubernetes.Interface
}

func acquireOwnership(ctx context.Context, target Target, artifacts string) (*ownershipLock, error) {
	path := filepath.Join(artifacts, ".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire local ownership lock: %w", err)
	}
	file.Close()
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("load kubeconfig for ownership lease: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	name := "karpenter-e2e-run-" + target.Prefix
	_, err = client.CoordinationV1().Leases("karpenter-system").Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		os.Remove(path)
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("target ownership lease %q is already held", name)
		}
		return nil, err
	}
	return &ownershipLock{path: path, leaseName: name, client: client}, nil
}

func (l *ownershipLock) release(ctx context.Context) {
	_ = l.client.CoordinationV1().Leases("karpenter-system").Delete(ctx, l.leaseName, metav1.DeleteOptions{})
	_ = os.Remove(l.path)
}
