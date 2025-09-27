package instance

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/api/compute/v1"
)

func TestMergeInstanceTagsPreservesTemplateAndAddsNetworkTags(t *testing.T) {
	base := &compute.Tags{Items: []string{"gke-default", "existing"}, Fingerprint: "fp"}

	merged := mergeInstanceTags(base, []string{"existing", "custom"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"gke-default", "existing", "existing", "custom"}, merged.Items)
	require.Equal(t, "fp", merged.Fingerprint)
	require.Equal(t, []string{"gke-default", "existing"}, base.Items, "base tags should not be mutated")
	require.NotSame(t, base, merged)
}

func TestMergeInstanceTagsHandlesNilTemplate(t *testing.T) {
	merged := mergeInstanceTags(nil, []string{"tag-one", "tag-two"})

	require.NotNil(t, merged)
	require.Equal(t, []string{"tag-one", "tag-two"}, merged.Items)
	require.Empty(t, merged.Fingerprint)
}

func TestMergeInstanceTagsReturnsNilWhenNoTags(t *testing.T) {
	require.Nil(t, mergeInstanceTags(nil, nil))
	require.Nil(t, mergeInstanceTags(&compute.Tags{}, nil))
}
