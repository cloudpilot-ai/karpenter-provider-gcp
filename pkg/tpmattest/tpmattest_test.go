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

package tpmattest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMakePublic(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	public, err := MakePublic(key.Public())
	require.NoError(t, err)
	require.Equal(t, toTPMCurves[elliptic.P256().Params().Name], public.ECCParameters.CurveID)

	// The TPM point carries the coordinates without leading zero bytes, matching
	// the template the GKE exec auth plugin loads for the kubelet key.
	point, err := key.PublicKey.Bytes()
	require.NoError(t, err)
	size := (key.Curve.Params().BitSize + 7) / 8
	require.Equal(t, new(big.Int).SetBytes(point[1:1+size]).Bytes(), []byte(public.ECCParameters.Point.XRaw))
	require.Equal(t, new(big.Int).SetBytes(point[1+size:]).Bytes(), []byte(public.ECCParameters.Point.YRaw))

	name, err := public.Name()
	require.NoError(t, err)
	require.NotEmpty(t, name.Digest)

	_, err = MakePublic(&rsa.PublicKey{N: big.NewInt(1), E: 65537})
	require.Error(t, err, "only ECDSA keys can be certified this way")
}
