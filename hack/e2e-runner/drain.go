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
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ownerLabel = "karpenter-e2e/owned"
const testNamespace = "karpenter-e2e-test"
const testWorkloadRole = "e2e-runtime-test-workloads"

var drainResources = []string{"nodepools.karpenter.sh", "nodeclaims.karpenter.sh", "nodeoverlays.karpenter.sh", "gcenodeclasses.karpenter.k8s.gcp", "gcecustommachinetypes.karpenter.k8s.gcp"}

type drainObject struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Status   struct {
		ProviderID string `json:"providerID"`
	} `json:"status"`
}
type cloudObject struct {
	ID, Name, SelfLink, Zone string
	Labels                   map[string]string
	Metadata                 struct{ Items []struct{ Key, Value string } }
	Disks                    []struct {
		Source     string
		AutoDelete bool
	}
	Users []string
}
type drainInventory struct {
	resources        map[string][]drainObject
	workloads        map[string][]drainObject
	namespace        *drainObject
	instances, disks map[string]cloudObject
}

func cloudInventory(ctx context.Context, t lifecycleTarget, kind string) ([]cloudObject, error) {
	var objects []cloudObject
	err := readToolJSON(ctx, &objects, "gcloud", "compute", kind, "list", "--project", t.Project, "--format=json")
	return objects, err
}
func targetCloudIdentity(o cloudObject, t lifecycleTarget, kind string) bool {
	prefix := "https://www.googleapis.com/compute/v1/projects/" + t.Project + "/zones/"
	if o.ID == "" || o.Name == "" || !strings.HasPrefix(o.SelfLink, prefix) || !strings.HasSuffix(o.SelfLink, "/"+kind+"/"+o.Name) {
		return false
	}
	zone := strings.TrimSuffix(strings.TrimPrefix(o.SelfLink, prefix), "/"+kind+"/"+o.Name)
	if o.Zone != "" && o.Zone != prefix+zone {
		return false
	}
	return zone != "" && !strings.Contains(zone, "/") && (t.Location == zone || t.Location == t.Region && strings.HasPrefix(zone, t.Region+"-"))
}
func inspectDrain(ctx context.Context, t lifecycleTarget) (d drainInventory, err error) {
	if t.requireTestBootstrap {
		if err := verifyTestBootstrap(ctx, t); err != nil {
			return d, err
		}
	}
	d.resources = map[string][]drainObject{}
	d.instances = map[string]cloudObject{}
	d.disks = map[string]cloudObject{}
	pools, classes, claims := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range drainResources {
		installed, err := boundedCommandOutput(ctx, "kubectl", "get", "crd", r, "-o", "name", "--ignore-not-found")
		if err != nil {
			return d, err
		}
		if len(strings.TrimSpace(string(installed))) == 0 {
			continue
		}
		var list struct{ Items []drainObject }
		if err := readToolJSON(ctx, &list, "kubectl", "get", r, "-o", "json"); err != nil {
			return d, err
		}
		d.resources[r] = list.Items
		for _, o := range list.Items {
			if o.Metadata.Labels[ownerLabel] != "true" {
				return d, fmt.Errorf("maintenance-required: unknown %s resource", r)
			}
			switch r {
			case "nodepools.karpenter.sh":
				pools[o.Metadata.Name] = true
			case "gcenodeclasses.karpenter.k8s.gcp":
				classes[o.Metadata.Name] = true
			case "nodeclaims.karpenter.sh":
				if o.Status.ProviderID != "" {
					claims[o.Status.ProviderID] = true
				}
			}
		}
	}
	data, err := boundedCommandOutput(ctx, "kubectl", "get", "namespace", testNamespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return d, err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		var ns drainObject
		if err := json.Unmarshal(data, &ns); err != nil {
			return d, err
		}
		if ns.Metadata.Labels[ownerLabel] != "true" {
			return d, fmt.Errorf("maintenance-required: unlabeled test namespace")
		}
		d.namespace = &ns
	}
	if d.namespace != nil {
		d.workloads, err = inspectTestWorkloads(ctx)
		if err != nil {
			return d, err
		}
	}
	instances, err := cloudInventory(ctx, t, "instances")
	if err != nil {
		return d, err
	}
	attached := map[string]bool{}
	systemDisks := map[string]bool{}
	for _, vm := range instances {
		pool := vm.Labels["karpenter-sh-nodepool"]
		if pool == "" {
			managed := false
			for _, item := range vm.Metadata.Items {
				managed = managed || item.Key == "created-by" && t.systemGroups[item.Value]
			}
			if !managed || !targetCloudIdentity(vm, t, "instances") || vm.Labels["goog-k8s-cluster-name"] != t.Cluster || vm.Labels["goog-k8s-cluster-location"] != t.Location {
				return d, fmt.Errorf("maintenance-required: unknown VM lacks verified GKE bootstrap identity")
			}
			for _, disk := range vm.Disks {
				if disk.Source != "" {
					systemDisks[disk.Source] = true
				}
			}
			continue
		}
		if vm.Labels["goog-k8s-cluster-name"] != t.Cluster || vm.Labels["goog-k8s-cluster-location"] != t.Location || !targetCloudIdentity(vm, t, "instances") {
			return d, fmt.Errorf("maintenance-required: unknown VM target")
		}
		zone := vm.Zone[strings.LastIndex(vm.Zone, "/")+1:]
		if !pools[pool] || !classes[vm.Labels["karpenter-k8s-gcp-gcenodeclass"]] || !claims["gce://"+t.Project+"/"+zone+"/"+vm.Name] {
			return d, fmt.Errorf("maintenance-required: orphan VM lacks owned CR correlation")
		}
		d.instances[vm.SelfLink] = vm
		for _, disk := range vm.Disks {
			if disk.Source != "" {
				attached[disk.Source] = true
			}
		}
	}
	disks, err := cloudInventory(ctx, t, "disks")
	if err != nil {
		return d, err
	}
	for _, disk := range disks {
		if attached[disk.SelfLink] {
			if !targetCloudIdentity(disk, t, "disks") {
				return d, fmt.Errorf("maintenance-required: unknown disk identity")
			}
			d.disks[disk.SelfLink] = disk
			delete(attached, disk.SelfLink)
		} else if !systemDisks[disk.SelfLink] {
			return d, fmt.Errorf("maintenance-required: orphan disk")
		}
	}
	if len(attached) != 0 {
		return d, fmt.Errorf("maintenance-required: attached disk identity unavailable")
	}
	return d, nil
}

func mutation(ctx context.Context, o *leaseOwner, name string, args ...string) error {
	if err := o.confirm(ctx); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// Bounded private tool output; never print potentially credential-bearing errors.
	cmd.Stdout = &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
	cmd.Stderr = &limitedWriter{Writer: io.Discard, Remaining: 2 << 20}
	return runIsolated(cmd)
}

func drainOwned(ctx context.Context, o *leaseOwner, t lifecycleTarget) error {
	d, err := inspectDrain(ctx, t)
	if err != nil {
		return err
	}
	// Preserve the bootstrap namespace/RBAC; only delete proven-owned workload
	// roots. Foreground GC drains their UID-correlated ReplicaSets and Pods.
	for _, deployment := range d.workloads["deployments.apps"] {
		if err := mutation(ctx, o, "kubectl", "-n", testNamespace, "delete", "deployments.apps", "--field-selector=metadata.name="+deployment.Metadata.Name, "--selector="+ownerLabel+"=true", "--cascade=foreground", "--ignore-not-found", "--wait=true", "--timeout=5m"); err != nil {
			return fmt.Errorf("maintenance-required: incomplete workload drain: %w", err)
		}
	}
	// The old controller remains running until all finalizers and cloud resources
	// have drained. Never remove finalizers or call cloud deletion APIs here.
	for _, r := range drainResources {
		if len(d.resources[r]) == 0 {
			continue
		}
		if err := mutation(ctx, o, "kubectl", "delete", r, "--selector="+ownerLabel+"=true", "--ignore-not-found", "--wait=true", "--timeout=5m"); err != nil {
			return fmt.Errorf("maintenance-required: incomplete CR drain: %w", err)
		}
	}
	return verifyDrained(ctx, o, t, d)
}

// Read-only completion proof shared by prepared CI and the local Bash cleanup.
// Captured cloud identities must disappear before the old controller is replaced.
func verifyDrained(ctx context.Context, o *leaseOwner, t lifecycleTarget, d drainInventory) error {
	for {
		if err := o.confirm(ctx); err != nil {
			return err
		}
		remaining := 0
		for _, kind := range []string{"instances", "disks"} {
			objects, err := cloudInventory(ctx, t, kind)
			if err != nil {
				return fmt.Errorf("maintenance-required: cloud deletion unconfirmed: %w", err)
			}
			captured := d.instances
			if kind == "disks" {
				captured = d.disks
			}
			for _, item := range objects {
				if old, ok := captured[item.SelfLink]; ok {
					if item.ID != old.ID || item.Name != old.Name || !targetCloudIdentity(item, t, kind) {
						return fmt.Errorf("maintenance-required: cloud identity replaced")
					}
					remaining++
				}
			}
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("maintenance-required: incomplete cloud deletion: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	// Detect CRs/VMs created while finalizers drained, rather than silently passing.
	final, err := inspectDrain(ctx, t)
	if err != nil {
		return err
	}
	for _, objects := range final.resources {
		if len(objects) > 0 {
			return fmt.Errorf("maintenance-required: resources remain after drain")
		}
	}
	for _, objects := range final.workloads {
		if len(objects) > 0 {
			return fmt.Errorf("maintenance-required: workloads remain after drain")
		}
	}
	if len(final.instances) > 0 || len(final.disks) > 0 {
		return fmt.Errorf("maintenance-required: resources remain after drain")
	}
	if (d.namespace == nil) != (final.namespace == nil) || d.namespace != nil && final.namespace.Metadata.UID != d.namespace.Metadata.UID {
		return fmt.Errorf("maintenance-required: bootstrap namespace identity changed")
	}
	return nil
}

func verifyTestBootstrap(ctx context.Context, t lifecycleTarget) error {
	data, err := boundedCommandOutput(ctx, "kubectl", "get", "namespace", testNamespace, "-o", "json", "--ignore-not-found")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		var ns drainObject
		if err := json.Unmarshal(data, &ns); err != nil {
			return err
		}
		if ns.Metadata.Name != testNamespace || ns.Metadata.UID == "" || ns.Metadata.DeletionTimestamp != nil || ns.Metadata.Labels[ownerLabel] != "true" {
			return fmt.Errorf("maintenance-required: invalid test namespace bootstrap")
		}
		var binding rbacv1.RoleBinding
		if err := readToolJSON(ctx, &binding, "kubectl", "-n", testNamespace, "get", "rolebinding", testWorkloadRole, "-o", "json"); err != nil {
			return fmt.Errorf("maintenance-required: test RoleBinding bootstrap unavailable: %w", err)
		}
		if binding.Name != testWorkloadRole || binding.Namespace != testNamespace || binding.UID == "" || binding.DeletionTimestamp != nil || binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: testWorkloadRole}) || len(binding.Subjects) != 1 || binding.Subjects[0] != (rbacv1.Subject{Kind: "User", APIGroup: rbacv1.GroupName, Name: t.Prefix + "-ci-sa@" + t.Project + ".iam.gserviceaccount.com"}) {
			return fmt.Errorf("maintenance-required: test RoleBinding bootstrap mismatch")
		}
		return nil
	}
	return fmt.Errorf("maintenance-required: test namespace bootstrap unavailable")
}

// Only label-owned Deployments and their immutable owner-reference descendants
// are supported. Current PR helpers can create unlabeled roots: leftovers from
// those helpers require maintenance, never name/namespace-based adoption.
func inspectTestWorkloads(ctx context.Context) (map[string][]drainObject, error) {
	workloads := map[string][]drainObject{}
	parents := map[string]metav1.ObjectMeta{}
	for _, resource := range []string{"deployments.apps", "replicasets.apps", "pods"} {
		var list struct{ Items []drainObject }
		if err := readToolJSON(ctx, &list, "kubectl", "-n", testNamespace, "get", resource, "-o", "json"); err != nil {
			return nil, err
		}
		children := map[string]metav1.ObjectMeta{}
		for _, object := range list.Items {
			meta := object.Metadata
			owned := meta.Name != "" && meta.UID != "" && meta.Namespace == testNamespace
			if resource == "deployments.apps" {
				owned = owned && meta.Labels[ownerLabel] == "true"
			} else {
				kind := "Deployment"
				if resource == "pods" {
					kind = "ReplicaSet"
				}
				proven := false
				for _, ref := range meta.OwnerReferences {
					parent, found := parents[ref.Name]
					proven = proven || found && ref.APIVersion == "apps/v1" && ref.Kind == kind && ref.UID == parent.UID && ref.Controller != nil && *ref.Controller
				}
				owned = owned && proven
			}
			if !owned {
				return nil, fmt.Errorf("maintenance-required: unknown test workload %s", resource)
			}
			children[meta.Name] = meta
		}
		workloads[resource] = list.Items
		parents = children
	}
	return workloads, nil
}
