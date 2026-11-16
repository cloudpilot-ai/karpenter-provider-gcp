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

package csr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis"
)

const (
	bootstrapUser = "kubelet-nodepool-bootstrap"
	nodePoolOrg   = "gke:nodepool:name:karpenter-default"
	instanceOrg   = "gke:instance:123456789:us-central1-a:987654321"
)

func requestPEM(t *testing.T, template *x509.CertificateRequest) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func nodeRequest(t *testing.T, nodeName string) []byte {
	return requestPEM(t, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "system:node:" + nodeName,
			Organization: []string{"system:nodes", nodePoolOrg, instanceOrg},
		},
	})
}

func kubeletCSR(request []byte, username string, groups ...string) *certv1.CertificateSigningRequest {
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "csr-test"},
		Spec: certv1.CertificateSigningRequestSpec{
			Request:    request,
			SignerName: certv1.KubeAPIServerClientKubeletSignerName,
			Usages:     []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth},
			Username:   username,
			Groups:     groups,
		},
	}
}

func bootstrapCSR(request []byte) *certv1.CertificateSigningRequest {
	return kubeletCSR(request, bootstrapUser, nodePoolOrg, "system:authenticated")
}

func nodeCSR(request []byte, nodeName string) *certv1.CertificateSigningRequest {
	return kubeletCSR(request, "system:node:"+nodeName, "system:nodes", nodePoolOrg, instanceOrg, "system:authenticated")
}

func launchedNodeClaim(name, instance string) karpv1.NodeClaim {
	nc := karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
			Group: apis.Group,
			Kind:  "GCENodeClass",
			Name:  "default",
		}},
	}
	nc.Status.ProviderID = "gce://my-project/us-central1-a/" + instance
	nc.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
	return nc
}

func registeredNodeClaim(name, instance string) karpv1.NodeClaim {
	nc := launchedNodeClaim(name, instance)
	nc.Status.NodeName = instance
	nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	return nc
}

func TestValidateKubeletClientCSR(t *testing.T) {
	launched := launchedNodeClaim("default-abc", "karpenter-default-abc")
	registered := registeredNodeClaim("default-xyz", "karpenter-default-xyz")
	nodeClaims := []karpv1.NodeClaim{launched, registered}

	tests := []struct {
		name    string
		csr     func(t *testing.T) *certv1.CertificateSigningRequest
		approve bool
	}{
		{
			name: "bootstrap for a launched, unregistered NodeClaim",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
			},
			approve: true,
		},
		{
			name: "bootstrap with usages in a different order",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageClientAuth, certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment}
				return csr
			},
			approve: true,
		},
		{
			name: "bootstrap without key encipherment",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth}
				return csr
			},
			approve: true,
		},
		{
			name: "bootstrap with only the system:nodes organization",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes"}},
				}))
			},
			approve: true,
		},
		{
			name: "requester that is not the bootstrap identity",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, "karpenter-default-abc"), "system:serviceaccount:default:attacker", "system:serviceaccounts", nodePoolOrg, "system:authenticated")
			},
		},
		{
			name: "bootstrap for a node with no NodeClaim",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, "gke-cluster-default-pool-1234"))
			},
		},
		{
			name: "bootstrap for an already registered NodeClaim",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(nodeRequest(t, "karpenter-default-xyz"))
			},
		},
		{
			name: "renewal from the node itself",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, "karpenter-default-xyz"), "karpenter-default-xyz")
			},
			approve: true,
		},
		{
			name: "renewal for a node with no NodeClaim",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, "gke-cluster-default-pool-1234"), "gke-cluster-default-pool-1234")
			},
		},
		{
			name: "node requesting a registered node's identity",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, "karpenter-default-xyz"), "karpenter-default-abc")
			},
		},
		{
			name: "node requesting an unregistered node's identity",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(nodeRequest(t, "karpenter-default-abc"), "karpenter-default-xyz")
			},
		},
		{
			name: "requester in the nodes group with another username",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return kubeletCSR(nodeRequest(t, "karpenter-default-abc"), "some-user", "system:nodes", nodePoolOrg)
			},
		},
		{
			name: "kubelet serving signer",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
				csr.Spec.SignerName = certv1.KubeletServingSignerName
				return csr
			},
		},
		{
			name: "server auth usage",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
				csr.Spec.Usages = append(csr.Spec.Usages, certv1.UsageServerAuth)
				return csr
			},
		},
		{
			name: "missing client auth usage",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				csr := bootstrapCSR(nodeRequest(t, "karpenter-default-abc"))
				csr.Spec.Usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment}
				return csr
			},
		},
		{
			name: "wrong organization",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:masters"}},
				}))
			},
		},
		{
			name: "extra organization",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes", "system:masters"}},
				}))
			},
		},
		{
			name: "gke organizations without system:nodes",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"gke:nodepool:name:karpenter-default"}},
				}))
			},
		},
		{
			name: "common name without node prefix",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "karpenter-default-abc", Organization: []string{"system:nodes"}},
				}))
			},
		},
		{
			name: "subject alternative names",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject:  pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes"}},
					DNSNames: []string{"karpenter-default-abc"},
				}))
			},
		},
		{
			name: "bootstrap with a node pool organization the requester is not in",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes", "gke:nodepool:name:privileged-pool", instanceOrg}},
				}))
			},
		},
		{
			name: "bootstrap with an instance organization in another zone",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes", nodePoolOrg, "gke:instance:123456789:us-east1-b:987654321"}},
				}))
			},
		},
		{
			name: "bootstrap with two instance organizations",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-abc", Organization: []string{"system:nodes", nodePoolOrg, instanceOrg, "gke:instance:123456789:us-central1-a:111"}},
				}))
			},
		},
		{
			name: "renewal with an instance organization the node is not in",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-xyz", Organization: []string{"system:nodes", nodePoolOrg, "gke:instance:123456789:us-central1-a:111"}},
				}), "karpenter-default-xyz")
			},
		},
		{
			name: "renewal with a node pool organization the node is not in",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return nodeCSR(requestPEM(t, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:karpenter-default-xyz", Organization: []string{"system:nodes", "gke:nodepool:name:privileged-pool", instanceOrg}},
				}), "karpenter-default-xyz")
			},
		},
		{
			name: "request that is not PEM",
			csr: func(t *testing.T) *certv1.CertificateSigningRequest {
				return bootstrapCSR([]byte("not a csr"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKubeletClientCSR(tt.csr(t), nodeClaims)
			if tt.approve {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateKubeletClientCSRRegisteredCondition(t *testing.T) {
	nc := launchedNodeClaim("default-abc", "karpenter-default-abc")
	nc.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)

	err := validateKubeletClientCSR(bootstrapCSR(nodeRequest(t, "karpenter-default-abc")), []karpv1.NodeClaim{nc})
	require.Error(t, err)
}

func TestParseProviderID(t *testing.T) {
	zone, instance := parseProviderID("gce://project/us-central1-a/vm-1")
	require.Equal(t, "us-central1-a", zone)
	require.Equal(t, "vm-1", instance)
	for _, id := range []string{"aws:///us-east-1a/i-123", "gce://project/vm-1", "gce:///us-central1-a/vm-1", "gce://project//vm-1", "gce://project/us-central1-a/", ""} {
		zone, instance = parseProviderID(id)
		require.Empty(t, zone)
		require.Empty(t, instance)
	}
}

func TestInstanceOrgZone(t *testing.T) {
	require.Equal(t, "us-central1-a", instanceOrgZone(instanceOrg))
	require.Empty(t, instanceOrgZone("gke:instance:us-central1-a"))
}
