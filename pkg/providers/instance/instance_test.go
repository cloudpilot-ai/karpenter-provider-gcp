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

package instance

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/apis/v1alpha1"
	pkgcache "github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/cache"
)

func TestMergeInstanceTagsPreservesTemplateAndAddsNetworkTags(t *testing.T) {
	base := &compute.Tags{Items: []string{"gke-default", "existing"}, Fingerprint: "fp"}

	merged := mergeInstanceTags(base, []v1alpha1.NetworkTag{"existing", "custom"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"gke-default", "existing", "existing", "custom"}, merged.Items)
	require.Equal(t, "fp", merged.Fingerprint)
	require.Equal(t, []string{"gke-default", "existing"}, base.Items, "base tags should not be mutated")
	require.NotSame(t, base, merged)
}

func TestMergeInstanceTagsHandlesNilTemplate(t *testing.T) {
	merged := mergeInstanceTags(nil, []v1alpha1.NetworkTag{"tag-one", "tag-two"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"tag-one", "tag-two"}, merged.Items)
	require.Empty(t, merged.Fingerprint)
}

func TestMergeInstanceTagsReturnsNilWhenNoTags(t *testing.T) {
	require.Nil(t, mergeInstanceTags(nil, nil))
	require.Nil(t, mergeInstanceTags(&compute.Tags{}, nil))
}

func TestIsInsufficientCapacityErrorMatchesCode(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Code: "IP_SPACE_EXHAUSTED_WITH_DETAILS"}

	require.True(t, isInsufficientCapacityError(entry))
}

func TestIsInsufficientCapacityErrorIgnoresMessageOnly(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Message: "some failure IP_SPACE_EXHAUSTED_WITH_DETAILS for range"}

	require.False(t, isInsufficientCapacityError(entry))
}

func TestIsInsufficientCapacityErrorNonMatching(t *testing.T) {
	t.Parallel()

	entry := &compute.OperationErrorErrors{Code: "UNKNOWN", Message: "other issue"}

	require.False(t, isInsufficientCapacityError(entry))
}

func TestExtractInsertInsufficientCapacityReasonMatchesReason(t *testing.T) {
	t.Parallel()

	reason, code, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{
		Errors: []googleapi.ErrorItem{{Reason: "IP_SPACE_EXHAUSTED_WITH_DETAILS"}},
	})

	require.True(t, ok)
	require.Equal(t, "IP_SPACE_EXHAUSTED_WITH_DETAILS", reason)
	require.Equal(t, "IP_SPACE_EXHAUSTED_WITH_DETAILS", code)
}

func TestExtractInsertInsufficientCapacityReasonRequiresStructuredReason(t *testing.T) {
	t.Parallel()

	reason, code, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{
		Message: "some failure IP_SPACE_EXHAUSTED for range",
	})

	require.False(t, ok)
	require.Empty(t, reason)
	require.Empty(t, code)
}

func TestExtractInsertInsufficientCapacityReasonNonMatching(t *testing.T) {
	t.Parallel()

	_, _, ok := extractInsertInsufficientCapacityReason(&googleapi.Error{Message: "some other issue"})

	require.False(t, ok)
}

func TestInsufficientCapacityBackoffTTLForIPSpace(t *testing.T) {
	t.Parallel()

	ttl := insufficientCapacityBackoffTTL("IP_SPACE_EXHAUSTED")

	require.Equal(t, ipSpaceInsufficientCapacityTTL, ttl)
}

func TestInsufficientCapacityBackoffTTLForOtherReasons(t *testing.T) {
	t.Parallel()

	ttl := insufficientCapacityBackoffTTL("ZONE_RESOURCE_POOL_EXHAUSTED")

	require.Equal(t, pkgcache.UnavailableOfferingsTTL, ttl)
}
