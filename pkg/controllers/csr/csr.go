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
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis"
)

const (
	nodeUserPrefix            = "system:node:"
	nodesGroup                = "system:nodes"
	nodePoolBootstrapUsername = "kubelet-nodepool-bootstrap"
	tpmBootstrapUsername      = "kubelet-bootstrap"
	nodePoolOrgPrefix         = "gke:nodepool:name:"
	instanceOrgPrefix         = "gke:instance:"
)

var (
	requiredUsages = sets.New(certv1.UsageClientAuth)
	allowedUsages  = sets.New(certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageClientAuth)
)

type kubeletClientSubject struct {
	nodeName    string
	nodePoolOrg string
	instanceOrg string
}

type Controller struct {
	client     client.Client
	kubeClient kubernetes.Interface
}

func NewController(kubeClient kubernetes.Interface) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	var csrList certv1.CertificateSigningRequestList
	if err := c.client.List(ctx, &csrList); err != nil {
		log.FromContext(ctx).Error(err, "unable to list CSRs")
		return reconciler.Result{}, err
	}

	var nodeClaimList karpv1.NodeClaimList
	if err := c.client.List(ctx, &nodeClaimList); err != nil {
		log.FromContext(ctx).Error(err, "unable to list NodeClaims")
		return reconciler.Result{}, err
	}

	for _, csr := range csrList.Items {
		if isApprovedOrDenied(&csr) {
			continue
		}

		log.FromContext(ctx).Info("reconciling CSR", "name", csr.Name)
		if err := validateKubeletClientCSR(&csr, nodeClaimList.Items); err != nil {
			log.FromContext(ctx).V(1).Info("not approving CSR", "name", csr.Name, "username", csr.Spec.Username, "reason", err.Error())
			continue
		}

		log.FromContext(ctx).Info("approving kubelet client CSR", "name", csr.Name, "username", csr.Spec.Username)
		csr.Status.Conditions = append(csr.Status.Conditions, certv1.CertificateSigningRequestCondition{
			Type:           certv1.CertificateApproved,
			Status:         "True",
			Reason:         "KarpenterAutoApprover",
			Message:        "Automatically approved by Karpenter CSR controller",
			LastUpdateTime: metav1.Now(),
		})

		if _, err := c.kubeClient.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, csr.Name, &csr, metav1.UpdateOptions{}); err != nil {
			log.FromContext(ctx).Error(err, "failed to approve CSR", "name", csr.Name)
			continue
		}
	}
	return reconciler.Result{RequeueAfter: 30 * time.Second}, nil
}

func validateKubeletClientCSR(csr *certv1.CertificateSigningRequest, nodeClaims []karpv1.NodeClaim) error {
	if csr.Spec.SignerName != certv1.KubeAPIServerClientKubeletSignerName {
		return fmt.Errorf("signer %q is not %q", csr.Spec.SignerName, certv1.KubeAPIServerClientKubeletSignerName)
	}

	usages := sets.New(csr.Spec.Usages...)
	if !usages.IsSuperset(requiredUsages) || !allowedUsages.IsSuperset(usages) {
		return fmt.Errorf("usages %v are not valid for a kubelet client certificate", csr.Spec.Usages)
	}

	subject, err := parseKubeletClientRequest(csr.Spec.Request)
	if err != nil {
		return err
	}

	groups := sets.New(csr.Spec.Groups...)
	if subject.nodePoolOrg != "" && !groups.Has(subject.nodePoolOrg) {
		return fmt.Errorf("requester %q is not in group %q", csr.Spec.Username, subject.nodePoolOrg)
	}

	switch csr.Spec.Username {
	case nodeUserPrefix + subject.nodeName:
		return validateRenewal(subject, groups, nodeClaims)
	case nodePoolBootstrapUsername, tpmBootstrapUsername:
		return validateBootstrap(subject, nodeClaims)
	default:
		return fmt.Errorf("requester %q cannot request a certificate for node %q", csr.Spec.Username, subject.nodeName)
	}
}

func validateRenewal(subject kubeletClientSubject, groups sets.Set[string], nodeClaims []karpv1.NodeClaim) error {
	if !groups.Has(nodesGroup) {
		return fmt.Errorf("requester is not in group %q", nodesGroup)
	}
	if subject.instanceOrg != "" && !groups.Has(subject.instanceOrg) {
		return fmt.Errorf("requester is not in group %q", subject.instanceOrg)
	}
	for i := range nodeClaims {
		nc := &nodeClaims[i]
		_, instance := parseProviderID(nc.Status.ProviderID)
		if isManagedNodeClaim(nc) && nc.Status.NodeName == subject.nodeName && instance == subject.nodeName && nc.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
			return nil
		}
	}
	return fmt.Errorf("no NodeClaim is registered as node %q", subject.nodeName)
}

func validateBootstrap(subject kubeletClientSubject, nodeClaims []karpv1.NodeClaim) error {
	for i := range nodeClaims {
		nc := &nodeClaims[i]
		zone, instance := parseProviderID(nc.Status.ProviderID)
		if !isManagedNodeClaim(nc) || instance != subject.nodeName {
			continue
		}
		if !nc.DeletionTimestamp.IsZero() || !nc.StatusConditions().Get(karpv1.ConditionTypeLaunched).IsTrue() {
			return fmt.Errorf("NodeClaim %q is deleting or has not launched", nc.Name)
		}
		if nc.Status.NodeName != "" || nc.StatusConditions().Get(karpv1.ConditionTypeRegistered).IsTrue() {
			return fmt.Errorf("NodeClaim %q for node %q is already registered", nc.Name, subject.nodeName)
		}
		if subject.instanceOrg != "" && instanceOrgZone(subject.instanceOrg) != zone {
			return fmt.Errorf("subject organization %q is not in NodeClaim %q zone %q", subject.instanceOrg, nc.Name, zone)
		}
		return nil
	}
	return fmt.Errorf("no NodeClaim was launched for node %q", subject.nodeName)
}

func isManagedNodeClaim(nc *karpv1.NodeClaim) bool {
	ref := nc.Spec.NodeClassRef
	return ref != nil && ref.Group == apis.Group && ref.Kind == "GCENodeClass"
}

func parseKubeletClientRequest(request []byte) (kubeletClientSubject, error) {
	block, _ := pem.Decode(request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return kubeletClientSubject{}, fmt.Errorf("request is not a PEM-encoded certificate request")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return kubeletClientSubject{}, fmt.Errorf("parsing certificate request: %w", err)
	}
	if err := req.CheckSignature(); err != nil {
		return kubeletClientSubject{}, fmt.Errorf("checking certificate request signature: %w", err)
	}
	if len(req.DNSNames) > 0 || len(req.EmailAddresses) > 0 || len(req.IPAddresses) > 0 || len(req.URIs) > 0 {
		return kubeletClientSubject{}, fmt.Errorf("kubelet client certificate requests cannot contain subject alternative names")
	}
	return parseKubeletClientSubject(req.Subject.CommonName, req.Subject.Organization)
}

func parseKubeletClientSubject(commonName string, organizations []string) (kubeletClientSubject, error) {
	var subject kubeletClientSubject
	var ok bool
	subject.nodeName, ok = strings.CutPrefix(commonName, nodeUserPrefix)
	if !ok || subject.nodeName == "" {
		return kubeletClientSubject{}, fmt.Errorf("subject common name %q does not start with %q", commonName, nodeUserPrefix)
	}
	if !slices.Contains(organizations, nodesGroup) {
		return kubeletClientSubject{}, fmt.Errorf("subject organization %v does not include %s", organizations, nodesGroup)
	}
	for _, o := range organizations {
		var field *string
		switch {
		case o == nodesGroup:
			continue
		case strings.HasPrefix(o, nodePoolOrgPrefix):
			field = &subject.nodePoolOrg
		case strings.HasPrefix(o, instanceOrgPrefix):
			field = &subject.instanceOrg
		default:
			return kubeletClientSubject{}, fmt.Errorf("subject organization %q is not allowed", o)
		}
		if *field != "" {
			return kubeletClientSubject{}, fmt.Errorf("subject organization %v repeats %q", organizations, o)
		}
		*field = o
	}
	return subject, nil
}

func instanceOrgZone(org string) string {
	parts := strings.Split(strings.TrimPrefix(org, instanceOrgPrefix), ":")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

func parseProviderID(providerID string) (zone, instance string) {
	rest, ok := strings.CutPrefix(providerID, "gce://")
	if !ok {
		return "", ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", ""
	}
	return parts[1], parts[2]
}

func isApprovedOrDenied(csr *certv1.CertificateSigningRequest) bool {
	for _, cond := range csr.Status.Conditions {
		if cond.Type == certv1.CertificateApproved || cond.Type == certv1.CertificateDenied {
			return true
		}
	}
	return false
}

func (c *Controller) Register(ctx context.Context, m manager.Manager) error {
	c.client = m.GetClient()
	return controllerruntime.NewControllerManagedBy(m).
		Named("csr-controller").
		For(&certv1.CertificateSigningRequest{}).
		Complete(singleton.AsReconciler(c))
}
