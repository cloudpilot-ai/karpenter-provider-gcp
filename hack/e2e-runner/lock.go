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
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	leaseName     = "karpenter-e2e-tests"
	leaseTokenKey = "karpenter.k8s.gcp/runner-token"
	leaseDuration = int32(600)
	renewInterval = time.Minute
)

type leaseClient interface {
	Get(context.Context, string, metav1.GetOptions) (*coordinationv1.Lease, error)
	Create(context.Context, *coordinationv1.Lease, metav1.CreateOptions) (*coordinationv1.Lease, error)
	Update(context.Context, *coordinationv1.Lease, metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

type e2eLease struct {
	client leaseClient
	holder string
	token  string
}

func newLeaseClient() (leaseClient, error) {
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	namespace, _ := controllerTarget()
	return client.CoordinationV1().Leases(namespace), nil
}

func acquireLease(ctx context.Context, client leaseClient, holder string) (*e2eLease, error) {
	if holder == "" {
		return nil, fmt.Errorf("lock ID must not be empty")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	lock := &e2eLease{client: client, holder: holder, token: fmt.Sprintf("%x", nonce)}
	current, err := client.Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		current = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName}}
	} else if err != nil {
		return nil, err
	} else if activeLease(current, time.Now()) {
		return nil, fmt.Errorf("e2e tests locked by %q", *current.Spec.HolderIdentity)
	}

	now := metav1.NewMicroTime(time.Now())
	current.Spec.HolderIdentity = &holder
	duration := leaseDuration
	current.Spec.LeaseDurationSeconds = &duration
	current.Spec.AcquireTime = &now
	current.Spec.RenewTime = &now
	if current.Annotations == nil {
		current.Annotations = make(map[string]string)
	}
	current.Annotations[leaseTokenKey] = lock.token
	if current.ResourceVersion == "" {
		_, err = client.Create(ctx, current, metav1.CreateOptions{})
	} else {
		_, err = client.Update(ctx, current, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("acquiring e2e lease: %w", err)
	}
	return lock, nil
}

func activeLease(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return false
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 {
		return true
	}
	last := lease.Spec.RenewTime
	if last == nil {
		last = lease.Spec.AcquireTime
	}
	return last == nil || now.Before(last.Add(time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second))
}

func (l *e2eLease) owned(lease *coordinationv1.Lease) bool {
	return lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == l.holder && lease.Annotations[leaseTokenKey] == l.token
}

func (l *e2eLease) renew(ctx context.Context) error {
	lease, err := l.client.Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !l.owned(lease) {
		return fmt.Errorf("e2e lease ownership lost")
	}
	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	_, err = l.client.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

func (l *e2eLease) release(ctx context.Context) error {
	lease, err := l.client.Get(ctx, leaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !l.owned(lease) {
		return fmt.Errorf("e2e lease ownership lost")
	}
	lease.Spec.HolderIdentity = nil
	lease.Spec.RenewTime = nil
	delete(lease.Annotations, leaseTokenKey)
	_, err = l.client.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

func withLease(ctx context.Context, client leaseClient, id string, run func(context.Context) error) (err error) {
	lease, err := acquireLease(ctx, client, id)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	stop := lease.maintain(runCtx, cancel)
	defer func() {
		stop()
		cancel(nil)
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer releaseCancel()
		err = errors.Join(err, lease.release(releaseCtx))
	}()
	result := run(runCtx)
	if cause := context.Cause(runCtx); cause != nil && cause != context.Canceled {
		return errors.Join(result, cause)
	}
	return result
}

func (l *e2eLease) maintain(ctx context.Context, cancel context.CancelCauseFunc) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := l.renew(ctx); err != nil {
					cancel(fmt.Errorf("renewing e2e lease: %w", err))
					return
				}
			}
		}
	}()
	return func() { close(stop); <-done }
}
