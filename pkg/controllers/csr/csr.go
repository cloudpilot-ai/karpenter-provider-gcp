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

package csr

import (
	"context"
	"reflect"
	"time"

	"github.com/awslabs/operatorpkg/singleton"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	KubeletClientSignerName = sets.NewString("kubernetes.io/kube-apiserver-client-kubelet", "kubernetes.io/kubelet-serving")
)

type Controller struct {
	client     client.Client
	kubeClient kubernetes.Interface
}

func NewController(kubeClient kubernetes.Interface) *Controller {
	return &Controller{
		kubeClient: kubeClient,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	var csrList certv1.CertificateSigningRequestList
	if err := c.client.List(ctx, &csrList); err != nil {
		log.FromContext(ctx).Error(err, "unable to list CSRs")
		return reconcile.Result{}, err
	}

	for _, csr := range csrList.Items {
		if isApprovedOrDenied(&csr) {
			continue
		}

		log.FromContext(ctx).Info("reconciling CSR", "name", csr.Name)
		if !KubeletClientSignerName.Has(csr.Spec.SignerName) {
			continue
		}

		if !reflect.DeepEqual(csr.Spec.Usages, []certv1.KeyUsage{
			certv1.UsageDigitalSignature,
			certv1.UsageKeyEncipherment,
			certv1.UsageClientAuth,
		}) {
			continue
		}

		log.FromContext(ctx).Info("approving bootstrap CSR", "name", csr.Name, "username", csr.Spec.Username)
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
	return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
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
