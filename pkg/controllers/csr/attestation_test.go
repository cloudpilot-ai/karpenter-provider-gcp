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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/google/go-tpm/legacy/tpm2"
	"github.com/stretchr/testify/require"

	"github.com/karpenter-gcp/karpenter-provider-gcp/pkg/tpmattest"
)

// attestationVM is the VM identity a valid local attestation fixture claims.
var attestationVM = vmIdentity{
	Zone:        "us-central1-a",
	ID:          987654321,
	Name:        "karpenter-default-abc",
	ProjectID:   123456789,
	ProjectName: "my-project",
}

// pemBlock is one block of a CSR attestation envelope.
type pemBlock struct {
	Type  string
	Bytes []byte
}

func encodePEMBlocks(blocks []pemBlock) []byte {
	var encoded []byte
	for _, block := range blocks {
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: block.Bytes})...)
	}
	return encoded
}

// attestationFixture is a locally generated TPM attestation proof. It contains
// no real cluster, key or certificate material.
type attestationFixture struct {
	vm  vmIdentity
	key *ecdsa.PrivateKey // CSR key
	ak  *rsa.PrivateKey   // VM attestation key
	der []byte            // certificate request DER
	// data and signature are the TPM certify structure and its RSA signature.
	data []byte
	sig  []byte
}

func newAttestationFixture(t *testing.T) *attestationFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ak, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	fixture := &attestationFixture{vm: attestationVM, key: key, ak: ak}
	fixture.der = certificateRequestDER(t, key, fixture.vm.Name)
	fixture.data, fixture.sig = certifyKey(t, key, ak)
	return fixture
}

func certificateRequestDER(t *testing.T, key *ecdsa.PrivateKey, nodeName string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   "system:node:" + nodeName,
			Organization: []string{"system:nodes", nodePoolOrg, instanceOrg},
		},
	}, key)
	require.NoError(t, err)
	return der
}

// certifyKey builds a TPM certify structure over the key's TPM public object and
// signs it with the attestation key, as the GKE exec auth plugin does.
func certifyKey(t *testing.T, key *ecdsa.PrivateKey, ak *rsa.PrivateKey) (data, sig []byte) {
	t.Helper()
	public, err := tpmattest.MakePublic(key.Public())
	require.NoError(t, err)
	name, err := public.Name()
	require.NoError(t, err)
	data, err = (&tpm2.AttestationData{
		Magic:               0xff544347,
		Type:                tpm2.TagAttestCertify,
		AttestedCertifyInfo: &tpm2.CertifyInfo{Name: name, QualifiedName: name},
	}).Encode()
	require.NoError(t, err)
	return data, signAttestation(t, ak, data)
}

func signAttestation(t *testing.T, ak *rsa.PrivateKey, data []byte) []byte {
	t.Helper()
	hash := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, ak, crypto.SHA256, hash[:])
	require.NoError(t, err)
	return sig
}

func (f *attestationFixture) vmIdentityBytes(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(f.vm)
	require.NoError(t, err)
	return encoded
}

// blocks returns a valid four-block attestation envelope.
func (f *attestationFixture) blocks(t *testing.T) []pemBlock {
	t.Helper()
	return []pemBlock{
		{Type: csrRequestBlock, Bytes: f.der},
		{Type: vmIdentityBlock, Bytes: f.vmIdentityBytes(t)},
		{Type: attestationDataBlock, Bytes: f.data},
		{Type: attestationSignatureBlock, Bytes: f.sig},
	}
}

// requestWithOrganizations returns a valid attestation envelope whose CSR
// requests these subject organizations for the certified key, so only the
// organization itself differs from a legitimate request.
func (f *attestationFixture) requestWithOrganizations(t *testing.T, organizations []string) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "system:node:" + f.vm.Name, Organization: organizations},
	}, f.key)
	require.NoError(t, err)
	blocks := f.blocks(t)
	blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: der}
	return encodePEMBlocks(blocks)
}

// attestationKeyPEM returns the authoritative attestation key in the form the
// Compute API returns it.
func (f *attestationFixture) attestationKeyPEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&f.ak.PublicKey)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParseAttestationEnvelope(t *testing.T) {
	fixture := newAttestationFixture(t)

	for _, tt := range []struct {
		name    string
		request func(t *testing.T) []byte
	}{
		{
			name: "valid envelope",
			request: func(t *testing.T) []byte {
				return encodePEMBlocks(fixture.blocks(t))
			},
		},
		{
			name: "valid envelope in a different block order",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				return encodePEMBlocks([]pemBlock{blocks[1], blocks[3], blocks[2], blocks[0]})
			},
		},
		{
			name: "missing attestation signature",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				return encodePEMBlocks(blocks[:3])
			},
		},
		{
			name: "missing VM identity",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				return encodePEMBlocks([]pemBlock{blocks[0], blocks[2], blocks[3]})
			},
		},
		{
			name: "duplicate attestation data",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				return encodePEMBlocks(append(blocks, pemBlock{Type: attestationDataBlock, Bytes: fixture.data}))
			},
		},
		{
			name: "unsupported block type",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				return encodePEMBlocks(append(blocks, pemBlock{Type: "UNKNOWN ATTESTATION", Bytes: []byte("pem")}))
			},
		},
		{
			// pem.Decode skips a malformed block and returns the next valid one; the
			// skipped data must not be silently dropped.
			name: "malformed PEM block before the envelope",
			request: func(t *testing.T) []byte {
				malformed := []byte("-----BEGIN CERTIFICATE REQUEST-----\n!!!!not-base64!!!!\n-----END CERTIFICATE REQUEST-----\n")
				return append(malformed, encodePEMBlocks(fixture.blocks(t))...)
			},
		},
		{
			name: "malformed PEM block inside the envelope",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				encoded := encodePEMBlocks(blocks[:1])
				encoded = append(encoded, []byte("-----BEGIN ATTESTATION DATA-----\n!!!!not-base64!!!!\n-----END ATTESTATION DATA-----\n")...)
				return append(encoded, encodePEMBlocks(blocks[1:])...)
			},
		},
		{
			name: "trailing data after the envelope",
			request: func(t *testing.T) []byte {
				return append(encodePEMBlocks(fixture.blocks(t)), []byte("not a pem block")...)
			},
		},
		{
			name: "pem headers",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				var encoded []byte
				for _, block := range blocks {
					encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: block.Type, Headers: map[string]string{"X-Extra": "1"}, Bytes: block.Bytes})...)
				}
				return encoded
			},
		},
		{
			name: "embedded request is a different CSR",
			request: func(t *testing.T) []byte {
				other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: certificateRequestDER(t, other, fixture.vm.Name)}
				return encodePEMBlocks(blocks)
			},
		},
		{
			name: "VM identity without a numeric instance ID",
			request: func(t *testing.T) []byte {
				identity := fixture.vm
				identity.ID = 0
				encoded, err := json.Marshal(identity)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[1] = pemBlock{Type: vmIdentityBlock, Bytes: encoded}
				return encodePEMBlocks(blocks)
			},
		},
		{
			name: "VM identity that is not JSON",
			request: func(t *testing.T) []byte {
				blocks := fixture.blocks(t)
				blocks[1] = pemBlock{Type: vmIdentityBlock, Bytes: []byte("not json")}
				return encodePEMBlocks(blocks)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := tt.request(t)
			csr, err := x509.ParseCertificateRequest(fixture.der)
			require.NoError(t, err)
			proof, err := parseAttestationEnvelope(request, csr)
			if tt.name == "valid envelope" || tt.name == "valid envelope in a different block order" {
				require.NoError(t, err)
				require.Equal(t, attestationVM, proof.vm)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestOptionalAttestationCertificateDoesNotSelectTheTrustAnchor(t *testing.T) {
	fixture := newAttestationFixture(t)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "untrusted attestation certificate"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(4102444800, 0),
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &fixture.ak.PublicKey, fixture.ak)
	require.NoError(t, err)
	optional := pemBlock{Type: "ATTESTATION CERTIFICATE", Bytes: certificateDER}
	blocks := append(fixture.blocks(t), optional)
	request, err := x509.ParseCertificateRequest(fixture.der)
	require.NoError(t, err)
	proof, err := parseAttestationEnvelope(encodePEMBlocks(blocks), request)
	require.NoError(t, err, "the native producer may append an optional attestation certificate")
	require.NoError(t, verifyAttestationProof(proof, request, &fixture.ak.PublicKey))

	otherAK, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	require.Error(t, verifyAttestationProof(proof, request, &otherAK.PublicKey),
		"the supplied certificate must not override the authoritative Compute key")
	_, err = parseAttestationEnvelope(encodePEMBlocks(append(blocks, optional)), request)
	require.Error(t, err, "optional blocks must not bypass duplicate rejection")
}

func TestVerifyAttestationProof(t *testing.T) {
	fixture := newAttestationFixture(t)

	for _, tt := range []struct {
		name    string
		request func(t *testing.T) ([]byte, *rsa.PublicKey)
		valid   bool
	}{
		{
			name: "valid proof",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				return encodePEMBlocks(fixture.blocks(t)), &fixture.ak.PublicKey
			},
			valid: true,
		},
		{
			name: "requested key differs from the certified key",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: certificateRequestDER(t, other, fixture.vm.Name)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "attestation signature from another key",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				other, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				return encodePEMBlocks(fixture.blocks(t)), &other.PublicKey
			},
		},
		{
			name: "tampered attestation signature",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				blocks := fixture.blocks(t)
				sig := append([]byte{}, fixture.sig...)
				sig[len(sig)-1] ^= 0xff
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: sig}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "tampered attestation data",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				blocks := fixture.blocks(t)
				data := append([]byte{}, fixture.data...)
				data[len(data)-1] ^= 0xff
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "non-canonical attestation data with trailing bytes",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				data := append(append([]byte{}, fixture.data...), 0x00)
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "attestation data is not a certify structure",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				// Re-tag the certify structure as a quote; the signature still verifies.
				data := append([]byte{}, fixture.data...)
				data[5] = byte(uint16(tpm2.TagAttestQuote) & 0xff)
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "malformed attestation data",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				data := []byte{0xff, 0x54, 0x43}
				blocks := fixture.blocks(t)
				blocks[2] = pemBlock{Type: attestationDataBlock, Bytes: data}
				blocks[3] = pemBlock{Type: attestationSignatureBlock, Bytes: signAttestation(t, fixture.ak, data)}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
		{
			name: "CSR key algorithm that cannot be certified",
			request: func(t *testing.T) ([]byte, *rsa.PublicKey) {
				rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
					Subject: pkix.Name{CommonName: "system:node:" + fixture.vm.Name, Organization: []string{"system:nodes"}},
				}, rsaKey)
				require.NoError(t, err)
				blocks := fixture.blocks(t)
				blocks[0] = pemBlock{Type: csrRequestBlock, Bytes: der}
				return encodePEMBlocks(blocks), &fixture.ak.PublicKey
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request, attestationKey := tt.request(t)
			// The controller parses the request's first PEM block as the CSR.
			csr, err := x509.ParseCertificateRequest(mustPEMBlock(t, request, csrRequestBlock))
			require.NoError(t, err)
			proof, err := parseAttestationEnvelope(request, csr)
			require.NoError(t, err)
			err = verifyAttestationProof(proof, csr, attestationKey)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func mustPEMBlock(t *testing.T, request []byte, blockType string) []byte {
	t.Helper()
	raw := request
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		require.NotNil(t, block)
		if block.Type == blockType {
			return block.Bytes
		}
		raw = rest
	}
	t.Fatalf("no %q block", blockType)
	return nil
}

func TestParseAttestationKey(t *testing.T) {
	fixture := newAttestationFixture(t)
	valid := fixture.attestationKeyPEM(t)

	for _, tt := range []struct {
		name string
		key  string
	}{
		{name: "valid RSA attestation key", key: valid},
		{name: "empty key", key: ""},
		{name: "not PEM", key: "not a key"},
		{name: "trailing data", key: valid + "-----BEGIN PUBLIC KEY-----\n"},
		{name: "EC key", key: func() string {
			der, err := x509.MarshalPKIXPublicKey(&fixture.key.PublicKey)
			require.NoError(t, err)
			return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key, err := parseAttestationKey(tt.key)
			if tt.name == "valid RSA attestation key" {
				require.NoError(t, err)
				require.NotNil(t, key)
				return
			}
			require.Error(t, err)
		})
	}
}
