/*
Copyright 2026 The Karpenter GCP Authors.

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

package instance

import (
	"context"
	"fmt"
	"strconv"

	"google.golang.org/api/compute/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/utils"
)

// InstanceIdentity is the authoritative Compute Engine identity evidence for a
// live VM. It is used to verify TPM attestation proofs submitted with kubelet
// client CSRs, so it is always read fresh from the Compute API instead of the
// name-keyed instance cache.
type InstanceIdentity struct {
	// ProjectID is the configured project name the lookup was made in.
	ProjectID string
	// ProjectNumber is the authoritative numeric ID of ProjectID.
	ProjectNumber uint64
	Zone          string
	Name          string
	// NumericID is the numeric Compute Engine instance ID of the live VM.
	NumericID uint64
	// AttestationKeyPEM is the PEM-encoded RSA vTPM attestation (signing) key.
	AttestationKeyPEM string
}

// GetIdentity returns authoritative identity evidence for the VM with this name
// in the configured project and zone. A VM that does not exist returns
// (nil, nil) so callers can leave the request pending instead of retrying a
// request that can no longer be satisfied.
func (p *DefaultProvider) GetIdentity(ctx context.Context, zone, name string) (*InstanceIdentity, error) {
	projectNumber, err := p.gkeProvider.ProjectNumber(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving project number: %w", err)
	}

	before, err := p.getInstance(ctx, zone, name)
	if err != nil || before == nil {
		return nil, err
	}
	if before.Id == 0 {
		return nil, fmt.Errorf("instance %q has no numeric instance ID", name)
	}

	attestationKeyPEM, err := p.attestationKeyPEM(ctx, zone, before)
	if err != nil || attestationKeyPEM == "" {
		return nil, err
	}

	// The attestation key must belong to the same incarnation as the VM identity
	// that is compared with the proof, so reject a VM replaced during the lookup.
	after, err := p.getInstance(ctx, zone, name)
	if err != nil {
		return nil, err
	}
	if after == nil {
		return nil, nil
	}
	if after.Id != before.Id {
		return nil, fmt.Errorf("instance %q was replaced during the attestation identity lookup", name)
	}

	return &InstanceIdentity{
		ProjectID:         p.projectID,
		ProjectNumber:     projectNumber,
		Zone:              zone,
		Name:              name,
		NumericID:         after.Id,
		AttestationKeyPEM: attestationKeyPEM,
	}, nil
}

// InstanceIncarnation is the freshly read VM incarnation and NodeClaim ownership
// association used for CSR approval that carries no TPM attestation. It is an
// association under trusted provisioning IAM, not proof of who requested a
// certificate.
type InstanceIncarnation struct {
	// ProjectID is the configured project name the lookup was made in.
	ProjectID string
	Zone      string
	Name      string
	// NumericID is the numeric Compute Engine instance ID of the live VM.
	NumericID uint64
	// NodeClaimUID is the NodeClaim UID recorded in the VM's ownership label, or
	// empty when the VM was launched without it.
	NodeClaimUID string
}

// GetIncarnation returns the live numeric ID and NodeClaim ownership label of the
// VM with this name in the configured project and zone. Like GetIdentity it reads
// the Compute API fresh instead of the name-keyed instance cache, but it needs no
// attestation key. A VM that does not exist returns (nil, nil).
func (p *DefaultProvider) GetIncarnation(ctx context.Context, zone, name string) (*InstanceIncarnation, error) {
	live, err := p.getInstance(ctx, zone, name)
	if err != nil || live == nil {
		return nil, err
	}
	if live.Id == 0 {
		return nil, fmt.Errorf("instance %q has no numeric instance ID", name)
	}
	return &InstanceIncarnation{
		ProjectID:    p.projectID,
		Zone:         zone,
		Name:         name,
		NumericID:    live.Id,
		NodeClaimUID: live.Labels[utils.SanitizeGCELabelValue(utils.LabelNodeClaimUIDKey)],
	}, nil
}

// attestationKeyPEM reads the vTPM attestation signing key of the instance this
// identity was observed for. The read is addressed by the numeric incarnation, so
// a VM recreated under the same name cannot supply another instance's key. An
// empty result means the VM no longer exists.
func (p *DefaultProvider) attestationKeyPEM(ctx context.Context, zone string, instance *compute.Instance) (string, error) {
	shielded, err := p.computeService.Instances.GetShieldedInstanceIdentity(p.projectID, zone, strconv.FormatUint(instance.Id, 10)).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading shielded instance identity: %w", err)
	}
	if shielded.SigningKey == nil || shielded.SigningKey.EkPub == "" {
		return "", fmt.Errorf("instance %q has no RSA attestation signing key", instance.Name)
	}
	return shielded.SigningKey.EkPub, nil
}

func (p *DefaultProvider) getInstance(ctx context.Context, zone, name string) (*compute.Instance, error) {
	instance, err := p.computeService.Instances.Get(p.projectID, zone, name).Context(ctx).Do()
	if err != nil {
		if isInstanceNotFoundError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting instance: %w", err)
	}
	return instance, nil
}
