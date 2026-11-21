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

// Package tpmattest contains TPM attestation logic shared with kubelet CSR
// verification.
//
// Adapted from Google's GKE certificate approval helper:
// https://github.com/kubernetes/cloud-provider-gcp/blob/de598a6a3590934e8b352db066038015b2649600/pkg/tpmattest/tpmattest.go
// Copyright 2018 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0.
// Changes: imports the maintained go-tpm legacy TPM 2.0 package instead of v1
// and supports the curves present in go-tpm's curve map.
package tpmattest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"fmt"
	"math/big"

	"github.com/google/go-tpm/legacy/tpm2"
)

var toTPMCurves = map[string]tpm2.EllipticCurve{
	elliptic.P224().Params().Name: tpm2.CurveNISTP224,
	elliptic.P256().Params().Name: tpm2.CurveNISTP256,
	elliptic.P384().Params().Name: tpm2.CurveNISTP384,
	elliptic.P521().Params().Name: tpm2.CurveNISTP521,
}

// MakePublic creates a tpm2.Public struct for pub. This struct is a TPM
// representation of the public key and is attested by the VM attestation key.
//
// Only *ecdsa.PublicKey is allowed.
func MakePublic(pub crypto.PublicKey) (tpm2.Public, error) {
	ecKey, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return tpm2.Public{}, fmt.Errorf("public key in CSR is %T, only ECDSA keys supported", pub)
	}

	curveName := ecKey.Curve.Params().Name
	curve, ok := toTPMCurves[curveName]
	if !ok {
		return tpm2.Public{}, fmt.Errorf("public key EC curve %q not supported", curveName)
	}
	xRaw, yRaw, err := minimalCoordinates(ecKey)
	if err != nil {
		return tpm2.Public{}, err
	}
	return tpm2.Public{
		Type:    tpm2.AlgECC,
		NameAlg: tpm2.AlgSHA256,
		// Allow access for TPM user with password auth. FlagSign is required
		// to load the key (either Sign or Decrypt).
		Attributes: tpm2.FlagSign | tpm2.FlagUserWithAuth,
		ECCParameters: &tpm2.ECCParams{
			CurveID: curve,
			Sign: &tpm2.SigScheme{
				Alg:  tpm2.AlgECDSA,
				Hash: tpm2.AlgSHA256,
			},
			Point: tpm2.ECPoint{
				XRaw: xRaw,
				YRaw: yRaw,
			},
		},
	}, nil
}

// minimalCoordinates returns the EC point coordinates without leading zero
// bytes. The producer builds the TPM template for the kubelet key this way, so
// the TPM object name only matches when the encoding matches.
func minimalCoordinates(key *ecdsa.PublicKey) (xRaw, yRaw []byte, err error) {
	point, err := key.Bytes()
	if err != nil {
		return nil, nil, fmt.Errorf("encoding public key point: %w", err)
	}
	size := (key.Curve.Params().BitSize + 7) / 8
	if len(point) != 1+2*size {
		return nil, nil, fmt.Errorf("unexpected public key point encoding length %d", len(point))
	}
	x := new(big.Int).SetBytes(point[1 : 1+size])
	y := new(big.Int).SetBytes(point[1+size:])
	return x.Bytes(), y.Bytes(), nil
}
