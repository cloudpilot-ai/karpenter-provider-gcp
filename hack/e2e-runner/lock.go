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
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	leaseNamespace = "karpenter-system"
	leaseName      = "karpenter-e2e-tests"
	leaseTokenKey  = "karpenter.k8s.gcp/runner-token"
	maintenanceKey = "karpenter.k8s.gcp/maintenance-required"
	leaseDuration  = int32(600)
	renewInterval  = time.Minute
)

var errTargetMaintenance = errors.New("maintenance-required: target is blocked or abandoned")

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
	return client.CoordinationV1().Leases(leaseNamespace), nil
}

func acquireLease(ctx context.Context, client leaseClient, holder string, bootstrap bool) (*e2eLease, error) {
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
		if bootstrap {
			return nil, fmt.Errorf("lease bootstrap unavailable: %w", err)
		}
		current = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: leaseName}}
	} else if err != nil {
		return nil, err
	} else if current.Annotations[maintenanceKey] != "" {
		return nil, errTargetMaintenance
	} else if current.Spec.HolderIdentity != nil && *current.Spec.HolderIdentity != "" && !activeLease(current, time.Now()) {
		return nil, errTargetMaintenance
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
	if !l.owned(lease) || !activeLease(lease, time.Now()) {
		return fmt.Errorf("e2e lease ownership lost or expired")
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
	if !l.owned(lease) || !activeLease(lease, time.Now()) {
		return fmt.Errorf("e2e lease ownership lost or expired")
	}
	lease.Spec.HolderIdentity = nil
	lease.Spec.RenewTime = nil
	delete(lease.Annotations, leaseTokenKey)
	_, err = l.client.Update(ctx, lease, metav1.UpdateOptions{})
	return err
}

// leaseOwner's heartbeat outlives ordinary test cancellation, but never loss or
// uncertainty of ownership. All mutating child processes use its contexts.
var errLeaseUncertain = errors.New("maintenance-required: lease ownership uncertain")

type leaseOwner struct {
	lease  *e2eLease
	ctx    context.Context
	cancel context.CancelCauseFunc
	mu     sync.Mutex
}

func (o *leaseOwner) confirm(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := context.Cause(o.ctx); err != nil {
		return err
	}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := o.lease.renew(check); err != nil {
		cause := fmt.Errorf("%w: %w", errLeaseUncertain, err)
		o.cancel(cause)
		return cause
	}
	return nil
}

func (o *leaseOwner) cleanupContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(o.ctx, timeout)
}

func withLease(ctx context.Context, client leaseClient, id string, run func(context.Context) error) error {
	return withOwnedLease(ctx, client, id, false, func(runCtx context.Context, _ *leaseOwner) error { return run(runCtx) })
}

func withOwnedLease(ctx context.Context, client leaseClient, id string, bootstrap bool, run func(context.Context, *leaseOwner) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, err := acquireLease(ctx, client, id, bootstrap)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancelCause(context.Background())
	owner := &leaseOwner{lease: lease, ctx: lifetime, cancel: cancel}
	runCtx, stopRun := context.WithCancelCause(lifetime)
	stopSignal := context.AfterFunc(ctx, func() { stopRun(context.Cause(ctx)) })
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-lifetime.Done():
				return
			case <-ticker.C:
				if owner.confirm(lifetime) != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
		stopSignal()
		stopRun(nil)
		if cause := context.Cause(lifetime); cause != nil {
			err = errors.Join(err, cause)
		} else {
			releaseCtx, releaseCancel := context.WithTimeout(lifetime, 10*time.Second)
			if releaseErr := lease.release(releaseCtx); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("%w: releasing: %w", errLeaseUncertain, releaseErr))
			}
			releaseCancel()
		}
		cancel(nil)
	}()
	return run(runCtx, owner)
}

// A blocked target can only be cleared by an explicitly approved maintenance
// procedure. No normal runner path clears this marker, including lease release.
func (o *leaseOwner) requireMaintenance(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if cause := context.Cause(o.ctx); cause != nil {
		return cause
	}
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lease, err := o.lease.client.Get(check, leaseName, metav1.GetOptions{})
	if err == nil && (!o.lease.owned(lease) || !activeLease(lease, time.Now())) {
		err = fmt.Errorf("lease ownership lost")
	}
	if err == nil {
		lease.Annotations[maintenanceKey] = "incomplete-drain"
		_, err = o.lease.client.Update(check, lease, metav1.UpdateOptions{})
	}
	if err != nil {
		o.cancel(fmt.Errorf("%w: marking target failed: %w", errLeaseUncertain, err))
	}
	return err
}
