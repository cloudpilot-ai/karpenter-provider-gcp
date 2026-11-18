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

package provisioning

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	gcpv1alpha1 "github.com/karpenter-gcp/karpenter-provider-gcp/pkg/apis/v1alpha1"
	"github.com/karpenter-gcp/karpenter-provider-gcp/test/pkg/environment"
)

// attestationProofBlocks are the required PEM blocks of a native GKE TPM
// envelope. The plugin may also append an optional ATTESTATION CERTIFICATE.
var attestationProofBlocks = []string{"CERTIFICATE REQUEST", "ATTESTATION DATA", "ATTESTATION SIGNATURE", "VM IDENTITY"}

var _ = Describe("Attested kubelet enrollment", Label("suite:provisioning"), func() {
	// This covers native enrollment of one fresh vTPM node: the provider records the
	// trusted VM incarnation and the controller approves the attested CSR. It does not
	// exercise native certificate rotation or re-enrollment during drain; those need a
	// separately approved candidate deployment.
	It("records the launched VM incarnation and approves the attested kubelet CSR", func(ctx SpecContext) {
		prefix := "attested-enrollment"
		name := prefix + "-" + environment.UniqueSuffix()
		GinkgoWriter.Printf("[setup] nodePool=%s\n", name)

		initialNodes := env.AllNodeNames(ctx)

		var provisionedNodeName string
		DeferCleanup(func(ctx context.Context) {
			env.DeleteDeployment(ctx, name)
			env.DeleteNodePool(ctx, name)
			env.DeleteNodeClass(ctx, name)
			if provisionedNodeName != "" {
				Expect(env.WaitForNodeRemoval(ctx, provisionedNodeName)).To(Succeed())
			}
		})

		env.CreateNodeClassWithVtpm(ctx, name, gcpv1alpha1.ImageFamilyContainerOptimizedOS)
		env.WaitForNodeClassReady(ctx, name)
		env.CreateNodePool(ctx, name, name, environment.TestCase{
			CapacityType:  karpv1.CapacityTypeOnDemand,
			Arch:          karpv1.ArchitectureAmd64,
			Families:      []string{"n2"},
			InstanceTypes: []string{"n2-standard-2", "n2-standard-4"},
		})
		env.WaitForNodePoolReady(ctx, name)
		env.CreateDeployment(ctx, name, name, name, karpv1.ArchitectureAmd64)

		env.WaitForNodeClaimLaunched(ctx, name)
		pod := env.WaitForRunningPod(ctx, name)
		Expect(pod.Spec.NodeName).NotTo(BeEmpty())

		node, err := env.KubeClient.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		provisionedNodeName = node.Name
		_, existedBefore := initialNodes[node.Name]
		Expect(existedBefore).To(BeFalse(), "expected a newly provisioned node, got a pre-existing one")
		Expect(environment.IsNodeReady(node)).To(BeTrue(), "node %s is not Ready", node.Name)

		// The claim records Compute's numeric instance ID, not the VM name.
		claim, err := env.GetNodeClaimForNode(ctx, provisionedNodeName)
		Expect(err).NotTo(HaveOccurred())
		recordedID, _, err := unstructured.NestedString(claim.Object, "metadata", "annotations", gcpv1alpha1.AnnotationInstanceNumericID)
		Expect(err).NotTo(HaveOccurred())
		Expect(recordedID).NotTo(BeEmpty(), "NodeClaim %s has no recorded VM instance ID", claim.GetName())

		instance, err := env.GetGCEInstance(ctx, node.Spec.ProviderID)
		Expect(err).NotTo(HaveOccurred())
		Expect(recordedID).To(Equal(strconv.FormatUint(instance.Id, 10)),
			"the recorded VM incarnation must be the live instance, not a name-based guess")
		Expect(instance.ShieldedInstanceConfig).NotTo(BeNil())
		Expect(instance.ShieldedInstanceConfig.EnableVtpm).To(BeTrue(), "the attested path needs a vTPM")

		// The kubelet client CSR for this node carried the native TPM proof and was
		// approved by this controller.
		var attested *certv1.CertificateSigningRequest
		Eventually(func(g Gomega) {
			csrs, err := env.KubeClient.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			for i := range csrs.Items {
				csr := &csrs.Items[i]
				if csr.Spec.SignerName != certv1.KubeAPIServerClientKubeletSignerName {
					continue
				}
				if !slices.Contains(pemBlockTypes(csr.Spec.Request), "VM IDENTITY") {
					continue
				}
				if requestNodeName(csr.Spec.Request) != provisionedNodeName {
					continue
				}
				attested = csr
				return
			}
			g.Expect(attested).NotTo(BeNil(), "no attested kubelet client CSR found for the provisioned node")
		}).WithTimeout(2 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())

		blockTypes := pemBlockTypes(attested.Spec.Request)
		expectedBlocks := slices.Clone(attestationProofBlocks)
		if slices.Contains(blockTypes, "ATTESTATION CERTIFICATE") {
			expectedBlocks = append(expectedBlocks, "ATTESTATION CERTIFICATE")
		}
		Expect(blockTypes).To(ConsistOf(expectedBlocks),
			"the native proof must contain the four required blocks and at most one optional certificate")
		Expect(attested.Spec.Username).To(BeElementOf("kubelet-nodepool-bootstrap", "kubelet-bootstrap"))
		var approvedByUs bool
		for _, condition := range attested.Status.Conditions {
			if condition.Type == certv1.CertificateApproved && condition.Reason == "KarpenterAutoApprover" {
				approvedByUs = true
			}
		}
		Expect(approvedByUs).To(BeTrue(), "the attested kubelet client CSR was not approved by this controller")
		Expect(attested.Status.Certificate).NotTo(BeEmpty(), "the approved request never received a certificate")
	}, SpecTimeout(20*time.Minute))
})

// requestNodeName returns the node name requested by the CSR's common name.
func requestNodeName(request []byte) string {
	block, _ := pem.Decode(request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return ""
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(csr.Subject.CommonName, "system:node:")
}

func pemBlockTypes(request []byte) []string {
	var types []string
	raw := request
	for {
		block, rest := pem.Decode(raw)
		if block == nil {
			return types
		}
		types = append(types, block.Type)
		raw = rest
	}
}
