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
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/awslabs/operatorpkg/status"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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

	zones, err := c.gkeProvider.ResolveClusterZones(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("resolving cluster zones: %w", err)
	}

	var guestCpus, memoryMb int32
	var resolvedZones []string
	sawUnresolvedZone := false
	for _, zone := range zones {
		mt, err := c.getMachineType(ctx, &computepb.GetMachineTypeRequest{
			Project:     c.authOptions.ProjectID,
			Zone:        zone,
			MachineType: obj.Spec.MachineType,
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
			log.FromContext(ctx).Error(err, "failed to resolve custom machine type in zone, will retry",
				"machineType", obj.Spec.MachineType, "zone", zone)
		}
	}

	if len(resolvedZones) == 0 && sawUnresolvedZone {
		// No definitive answer at all yet; leave status/conditions untouched and retry with
		// the controller's standard backoff rather than flapping Ready to False.
		return reconcile.Result{}, fmt.Errorf("could not resolve %q in any zone due to transient errors", obj.Spec.MachineType)
	}

	if len(resolvedZones) == 0 {
		obj.Status.GuestCpus = 0
		obj.Status.MemoryMb = 0
		obj.Status.Zones = nil
		obj.StatusConditions().SetFalse(status.ConditionReady, "MachineTypeNotFound",
			fmt.Sprintf("%q is not a valid or available custom machine type in any cluster zone", obj.Spec.MachineType))
	} else {
		obj.Status.GuestCpus = guestCpus
		obj.Status.MemoryMb = memoryMb
		obj.Status.Zones = resolvedZones
		obj.StatusConditions().SetTrue(status.ConditionReady)
	}

	if !equality.Semantic.DeepEqual(stored, obj) {
		if err := c.kubeClient.Status().Patch(ctx, obj, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return reconcile.Result{Requeue: true}, nil
			}
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}

	return reconcile.Result{RequeueAfter: resolveTTL}, nil
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
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
