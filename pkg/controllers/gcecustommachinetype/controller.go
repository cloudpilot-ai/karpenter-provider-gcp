/*
Copyright 2026 The CloudPilot AI Authors.

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

// Package gcecustommachinetype reconciles v1alpha1.GCECustomMachineType objects, resolving
// each registered shape's vCPU/memory/zone-availability via machineTypes.get. GCP's
// machineTypes.aggregatedList API (used to populate the regular instance type catalog) never
// returns custom shapes since it does not enumerate the space of valid custom configurations;
// machineTypes.get, unlike the aggregated list, does resolve a specific valid custom shape on
// demand. See proposals/0009-custom-machine-type-catalog.md.
package gcecustommachinetype

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/awslabs/operatorpkg/status"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/auth"
	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/providers/gke"
)

// resolveTTL bounds how long a resolution is trusted before being re-verified against GCE, so
// new zone availability (or a shape becoming resolvable after transient failures) is picked up
// without requiring the object to be touched.
const resolveTTL = time.Hour

// getMachineType matches the signature of *compute.MachineTypesClient.Get, aliased so tests can
// substitute a fake without a live GCP client.
type getMachineType func(ctx context.Context, req *computepb.GetMachineTypeRequest, opts ...gax.CallOption) (*computepb.MachineType, error)

type Controller struct {
	kubeClient     client.Client
	authOptions    *auth.Credential
	gkeProvider    gke.Provider
	getMachineType getMachineType
}

func NewController(kubeClient client.Client, authOptions *auth.Credential, gkeProvider gke.Provider) *Controller {
	machineTypesClient, err := compute.NewMachineTypesRESTClient(context.Background())
	if err != nil {
		log.Log.Error(err, "failed to create machine types client for gcecustommachinetype controller")
		os.Exit(1)
	}
	return &Controller{
		kubeClient:     kubeClient,
		authOptions:    authOptions,
		gkeProvider:    gkeProvider,
		getMachineType: machineTypesClient.Get,
	}
}

func (c *Controller) Reconcile(ctx context.Context, obj *v1alpha1.GCECustomMachineType) (reconcile.Result, error) {
	stored := obj.DeepCopy()

	superseded, err := c.isSupersededDuplicate(ctx, obj)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("checking for duplicate registrations: %w", err)
	}
	if superseded {
		obj.Status.GuestCpus = 0
		obj.Status.MemoryMb = 0
		obj.Status.Zones = nil
		obj.StatusConditions().SetFalse(status.ConditionReady, "MachineTypeAlreadyRegistered",
			fmt.Sprintf("another GCECustomMachineType already registers %q; delete the duplicate or rename this one", obj.Spec.MachineType))
		return c.patchStatus(ctx, stored, obj)
	}

	zones, err := c.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("resolving cluster zones: %w", err)
	}

	guestCpus, memoryMb, resolvedZones, sawUnresolvedZone := c.resolveZones(ctx, obj, zones)

	switch {
	case len(resolvedZones) == 0 && sawUnresolvedZone:
		// Previously confirmed zones were removed or returned 404, while the remaining
		// zones are unknown. Withdraw the old offerings without claiming definitive absence.
		if len(obj.Status.Zones) > 0 {
			obj.Status.Zones = nil
			obj.StatusConditions().SetUnknownWithReason(status.ConditionReady, "MachineTypeResolutionFailed", "no confirmed zones remain; retrying unresolved zones")
			if _, err := c.patchStatus(ctx, stored, obj); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, fmt.Errorf("could not resolve %q in any zone due to transient errors", obj.Spec.MachineType)
	case len(resolvedZones) == 0:
		obj.Status.GuestCpus = 0
		obj.Status.MemoryMb = 0
		obj.Status.Zones = nil
		obj.StatusConditions().SetFalse(status.ConditionReady, "MachineTypeNotFound",
			fmt.Sprintf("%q is not a valid or available custom machine type in any cluster zone", obj.Spec.MachineType))
	default:
		obj.Status.GuestCpus = guestCpus
		obj.Status.MemoryMb = memoryMb
		obj.Status.Zones = resolvedZones
		obj.StatusConditions().SetTrue(status.ConditionReady)
	}

	if sawUnresolvedZone {
		// Some zone's availability is still unconfirmed. Persist what we do know (below) but
		// retry promptly via the controller's own backoff instead of waiting the full
		// resolveTTL, so a transient failure doesn't strand a valid zone out of the catalog
		// for up to an hour.
		if _, err := c.patchStatus(ctx, stored, obj); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, fmt.Errorf("could not resolve %q in every zone due to transient errors; retrying", obj.Spec.MachineType)
	}

	return c.patchStatus(ctx, stored, obj)
}

// resolveZones retains previously confirmed zones on transient errors; only a definitive
// absence or removal from the cluster's zones can withdraw their offerings.
func (c *Controller) resolveZones(ctx context.Context, obj *v1alpha1.GCECustomMachineType, zones []string) (guestCpus, memoryMb int32, resolvedZones []string, sawUnresolvedZone bool) {
	machineType := obj.Spec.MachineType
	wasReady := obj.StatusConditions().Get(status.ConditionReady).IsTrue()
	if wasReady {
		guestCpus, memoryMb = obj.Status.GuestCpus, obj.Status.MemoryMb
	}
	for _, zone := range zones {
		mt, err := c.getMachineType(ctx, &computepb.GetMachineTypeRequest{
			Project:     c.authOptions.ProjectID,
			Zone:        zone,
			MachineType: machineType,
		})
		switch {
		case err == nil && mt != nil:
			guestCpus, memoryMb = mt.GetGuestCpus(), mt.GetMemoryMb()
			resolvedZones = append(resolvedZones, zone)
		case isMachineTypeNotFoundError(err):
			// Confirmed: not a valid/available shape in this zone.
		default:
			// Transient, authorization, quota, or context error: this zone's availability is
			// unknown, not confirmed absent. Retry rather than reporting Ready=False on it.
			sawUnresolvedZone = true
			if wasReady && slices.Contains(obj.Status.Zones, zone) {
				resolvedZones = append(resolvedZones, zone)
			}
			log.FromContext(ctx).Error(err, "failed to resolve custom machine type in zone, will retry",
				"machineType", machineType, "zone", zone)
		}
	}
	return guestCpus, memoryMb, resolvedZones, sawUnresolvedZone
}

// patchStatus persists status/condition changes to obj (diffed against stored) and returns the
// standard resolveTTL requeue. A concurrent modification requeues immediately instead of erroring.
func (c *Controller) patchStatus(ctx context.Context, stored, obj *v1alpha1.GCECustomMachineType) (reconcile.Result, error) {
	if equality.Semantic.DeepEqual(stored, obj) {
		return reconcile.Result{RequeueAfter: resolveTTL}, nil
	}
	if err := c.kubeClient.Status().Patch(ctx, obj, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			// Returning the error (rather than the deprecated Result.Requeue) lets the
			// controller's own rate limiter drive the retry, same as a fresh resourceVersion
			// becoming available almost immediately.
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{RequeueAfter: resolveTTL}, nil
}

// isSupersededDuplicate reports whether another GCECustomMachineType already claims the same
// spec.machineType and outranks obj. Nothing prevents two objects from registering the same
// shape - admission validates each object in isolation - so without this check the catalog
// merge would silently use whichever one's price happened to be indexed last. Exactly one of
// any conflicting set outranks the rest (earliest creation, ties broken by name), so the result
// is consistent regardless of which object is reconciled first or how many there are.
func (c *Controller) isSupersededDuplicate(ctx context.Context, obj *v1alpha1.GCECustomMachineType) (bool, error) {
	list := &v1alpha1.GCECustomMachineTypeList{}
	if err := c.kubeClient.List(ctx, list); err != nil {
		return false, err
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == obj.Name || other.Spec.MachineType != obj.Spec.MachineType {
			continue
		}
		if outranks(other, obj) {
			return true, nil
		}
	}
	return false, nil
}

// outranks reports whether a should win a naming conflict over b: earlier CreationTimestamp
// wins; ties fall back to the lexicographically smaller name for a deterministic, symmetric
// result no matter which of the two objects is being asked.
func outranks(a, b *v1alpha1.GCECustomMachineType) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.Name < b.Name
}

// isMachineTypeNotFoundError reports whether err is a definitive "this machine type does not
// exist in this zone" response (HTTP 404), as opposed to a transient, authorization, quota, or
// context error, which says nothing about whether the shape is actually valid or available.
func isMachineTypeNotFoundError(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("gcecustommachinetype").
		For(&v1alpha1.GCECustomMachineType{}).
		Watches(&v1alpha1.GCECustomMachineType{}, handler.EnqueueRequestsFromMapFunc(c.enqueueSiblingsWithSameMachineType)).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}

// enqueueSiblingsWithSameMachineType requeues every other GCECustomMachineType registering the
// same spec.machineType as obj whenever obj changes, including deletion: isSupersededDuplicate's
// result for a duplicate depends on which other objects currently exist, so a losing duplicate
// must be re-reconciled when the registration it lost to is deleted (or edited to a different
// machineType), not just on its own resolveTTL cadence. Without this, deleting a winning
// registration would leave its machine type unschedulable for up to resolveTTL.
func (c *Controller) enqueueSiblingsWithSameMachineType(ctx context.Context, obj client.Object) []reconcile.Request {
	cmt, ok := obj.(*v1alpha1.GCECustomMachineType)
	if !ok {
		return nil
	}
	list := &v1alpha1.GCECustomMachineTypeList{}
	if err := c.kubeClient.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing GCECustomMachineTypes to requeue siblings")
		return nil
	}
	var requests []reconcile.Request
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == cmt.Name || other.Spec.MachineType != cmt.Spec.MachineType {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(other)})
	}
	return requests
}
