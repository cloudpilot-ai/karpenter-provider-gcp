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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestLeaseLifecycle(t *testing.T) {
	ctx := context.Background()
	client := &memoryLeaseClient{}
	first, err := acquireLease(ctx, client, "host:123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLease(ctx, client, "host:456"); err == nil || !strings.Contains(err.Error(), "host:123") {
		t.Fatalf("expected busy lease with holder, got %v", err)
	}
	if err := first.renew(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLease(ctx, client, "host:456"); err != nil {
		t.Fatalf("expected released lease to be available: %v", err)
	}
}

func TestExpiredLeaseCannotBeReleasedByOldOwner(t *testing.T) {
	ctx := context.Background()
	client := &memoryLeaseClient{}
	old, err := acquireLease(ctx, client, "shared")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := client.Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	past := metav1.NewMicroTime(time.Now().Add(-11 * time.Minute))
	lease.Spec.RenewTime = &past
	if _, err := client.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	current, err := acquireLease(ctx, client, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if old.token == current.token {
		t.Fatal("takeover reused the old owner's token")
	}
	if err := old.release(ctx); err == nil {
		t.Fatal("old owner released a lease after takeover")
	}
	if err := old.renew(ctx); err == nil {
		t.Fatal("old owner renewed a lease after takeover")
	}
	stored, err := client.Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil || stored.Spec.HolderIdentity == nil || *stored.Spec.HolderIdentity != "shared" {
		t.Fatalf("new owner lost lease: %v, %v", stored, err)
	}
	if err := current.release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExistingUnexpiredLease(t *testing.T) {
	ctx := context.Background()
	client := &memoryLeaseClient{lease: &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "karpenter-system"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr("other"),
			LeaseDurationSeconds: ptr(int32(600)),
			RenewTime:            ptr(metav1.NewMicroTime(time.Now())),
		},
	}}
	if _, err := acquireLease(ctx, client, "me"); err == nil {
		t.Fatal("expected live lease to block")
	}
}

func TestInterruptedLeaseIsReleased(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &memoryLeaseClient{}
	err := withLease(ctx, client, "host:123", func(runCtx context.Context) error {
		cancel() // signal.NotifyContext cancels the same context on Ctrl+C.
		<-runCtx.Done()
		return runCtx.Err()
	})
	if err == nil {
		t.Fatal("expected interrupted run to fail")
	}
	if _, err := acquireLease(context.Background(), client, "next"); err != nil {
		t.Fatalf("lease was not released on interruption: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

type memoryLeaseClient struct {
	mu      sync.Mutex
	lease   *coordinationv1.Lease
	version int
}

var leaseResource = schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}

func (c *memoryLeaseClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*coordinationv1.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil {
		return nil, apierrors.NewNotFound(leaseResource, name)
	}
	return c.lease.DeepCopy(), nil
}

func (c *memoryLeaseClient) Create(_ context.Context, lease *coordinationv1.Lease, _ metav1.CreateOptions) (*coordinationv1.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease != nil {
		return nil, apierrors.NewAlreadyExists(leaseResource, lease.Name)
	}
	return c.save(lease), nil
}

func (c *memoryLeaseClient) Update(_ context.Context, lease *coordinationv1.Lease, _ metav1.UpdateOptions) (*coordinationv1.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lease == nil || lease.ResourceVersion != c.lease.ResourceVersion {
		return nil, apierrors.NewConflict(leaseResource, lease.Name, fmt.Errorf("resource version changed"))
	}
	return c.save(lease), nil
}

func (c *memoryLeaseClient) save(lease *coordinationv1.Lease) *coordinationv1.Lease {
	c.version++
	c.lease = lease.DeepCopy()
	c.lease.ResourceVersion = strconv.Itoa(c.version)
	return c.lease.DeepCopy()
}
