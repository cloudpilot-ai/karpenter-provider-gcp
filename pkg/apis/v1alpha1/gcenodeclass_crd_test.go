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

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func crdPath() string {
	return filepath.Join("..", "..", "..", "charts", "karpenter", "crds", "karpenter.k8s.gcp_gcenodeclasses.yaml")
}

func nodeClassSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	raw, err := os.ReadFile(crdPath())
	require.NoError(t, err)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.Len(t, crd.Spec.Versions, 1)

	return *crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

func specSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	return nodeClassSchema(t).Properties["spec"]
}

func TestGCENodeClassCRDFreeIPCount(t *testing.T) {
	rangeSchema := nodeClassSchema(t).Properties["status"].Properties["subnetRanges"].Items.Schema
	field, ok := rangeSchema.Properties["totalFreeIP"]
	require.True(t, ok)
	require.Equal(t, "integer", field.Type)
	require.Equal(t, "int64", field.Format)
	require.NotNil(t, field.Minimum)
	require.Equal(t, float64(0), *field.Minimum)
	require.NotContains(t, rangeSchema.Required, "totalFreeIP")
	require.NotContains(t, rangeSchema.Properties, "utilization")
}

func kubeletConfigurationSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	return specSchema(t).Properties["kubeletConfiguration"]
}

func hugepagesSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	return specSchema(t).Properties["linuxNodeConfig"].Properties["hugepages"]
}

func TestGCENodeClassCRDAllowsMaxParallelImagePullsOfOne(t *testing.T) {
	field := kubeletConfigurationSchema(t).Properties["maxParallelImagePulls"]

	require.NotNil(t, field.Minimum, "maxParallelImagePulls must declare a minimum")
	require.Equal(t, float64(1), *field.Minimum, "maxParallelImagePulls must accept 1, the kubelet default")
}

func TestGCENodeClassCRDRestrictsParallelImagePullsAboveOne(t *testing.T) {
	const rule = `!has(self.maxParallelImagePulls) || self.maxParallelImagePulls < 2 || ` +
		`(has(self.serializeImagePulls) && self.serializeImagePulls == false)`

	var found bool
	for _, v := range kubeletConfigurationSchema(t).XValidations {
		if v.Rule == rule {
			found = true
			require.Equal(t,
				"maxParallelImagePulls greater than 1 requires serializeImagePulls to be explicitly set to false",
				v.Message)
		}
	}
	require.True(t, found, "serializeImagePulls must still be required from maxParallelImagePulls 2 upwards")
}

func TestGCENodeClassCRDRejectsReservedMetadataKeys(t *testing.T) {
	crd, err := os.ReadFile(crdPath())
	require.NoError(t, err)

	crdText := string(crd)
	require.Contains(t, crdText, `message: spec.metadata contains a reserved GKE metadata key`)
	require.Contains(t, crdText, `cluster-location`)

	reservedKeys := []string{
		"cluster-location",
		"cluster-name",
		"cluster-uid",
		"configure-sh",
		"containerd-configure-sh",
		"disable-address-manager",
		"enable-os-login",
		"gci-ensure-gke-docker",
		"gci-metrics-enabled",
		"gci-update-strategy",
		"instance-template",
		"install-ssh-psm1",
		"k8s-node-setup-psm1",
		"kube-env",
		"kube-labels",
		"startup-script",
		"user-data",
		"user-profile-psm1",
		"windows-startup-script-ps1",
		"common-psm1",
		"kubeconfig",
		"kubelet-config",
	}

	for _, key := range reservedKeys {
		require.Contains(t, crdText, `'`+key+`'`, "reserved metadata key %q must be denied by the CRD", key)
	}
	require.NotContains(t, crdText, `'serial-port-logging-enable'`, "non-reserved metadata keys must remain freeform")
}

func TestGCENodeClassCRDSubnetRangeNamesMutuallyExclusive(t *testing.T) {
	crd, err := os.ReadFile(crdPath())
	require.NoError(t, err)

	crdText := string(crd)
	require.Contains(t, crdText, `subnetRangeName and subnetRangeNames are mutually exclusive`)
	require.Contains(t, crdText, `!(has(self.subnetRangeName) && has(self.subnetRangeNames))`)
	require.Contains(t, crdText, `subnetRangeNames:`)
}

func TestGCENodeClassCRDSubnetRangeNamesUniqueViaCEL(t *testing.T) {
	crd, err := os.ReadFile(crdPath())
	require.NoError(t, err)

	crdText := string(crd)
	require.NotContains(t, crdText, `uniqueItems: true`, "Kubernetes CRDs forbid uniqueItems")
	require.Contains(t, crdText, `subnetRangeNames must be unique`)
	require.Contains(t, crdText, `self.all(x, self.exists_one(y, x == y))`)
}

func TestGCENodeClassCRDResourceManagerTags(t *testing.T) {
	field, ok := specSchema(t).Properties["resourceManagerTags"]
	require.True(t, ok)
	require.Equal(t, "object", field.Type)
	require.NotNil(t, field.MaxProperties)
	require.Equal(t, int64(50), *field.MaxProperties)
	require.Len(t, field.XValidations, 1)
	require.Equal(t, "invalid resourceManagerTags key", field.XValidations[0].Message)

	value := field.AdditionalProperties.Schema
	require.Equal(t, `^(tagValues/[0-9]+|[^/]+)$`, value.Pattern)
	require.NotNil(t, value.MaxLength)
	require.Equal(t, int64(63), *value.MaxLength)
}

func TestGCENodeClassCRDRejectsZeroHugepages(t *testing.T) {
	for _, name := range []string{"hugepageSize2m", "hugepageSize1g"} {
		field := hugepagesSchema(t).Properties[name]

		require.NotNil(t, field.Minimum, "%s must declare a minimum", name)
		require.Equal(t, float64(1), *field.Minimum, "%s must reject 0", name)
	}
}

func TestGCENodeClassCRDGPUSharingConfig(t *testing.T) {
	sharing := specSchema(t).Properties["gpuSharingConfig"]
	require.ElementsMatch(t, []string{"strategy", "maxSharedClientsPerGPU"}, sharing.Required)

	var strategies []string
	for _, value := range sharing.Properties["strategy"].Enum {
		strategies = append(strategies, string(value.Raw))
	}
	require.Equal(t, []string{`"TimeSharing"`}, strategies)

	clients := sharing.Properties["maxSharedClientsPerGPU"]
	require.NotNil(t, clients.Minimum)
	require.NotNil(t, clients.Maximum)
	require.Equal(t, float64(2), *clients.Minimum)
	require.Equal(t, float64(48), *clients.Maximum)
}

func TestGCENodeClassCRDUbuntuVersionPinFormat(t *testing.T) {
	celPattern := regexp.MustCompile(`matches\('([^']+)'\)`)
	term := specSchema(t).Properties["imageSelectorTerms"].Items.Schema

	patterns := map[string]string{}
	for _, v := range term.XValidations {
		if strings.HasPrefix(v.Message, "Ubuntu2404 version") || strings.HasPrefix(v.Message, "Ubuntu2204 version") {
			m := celPattern.FindStringSubmatch(v.Rule)
			require.Len(t, m, 2, "rule %q", v.Rule)
			patterns[v.Message[:len("Ubuntu2404")]] = m[1]
		}
	}
	for _, v := range term.Properties["alias"].XValidations {
		if strings.HasPrefix(v.Message, "Ubuntu version") {
			m := celPattern.FindStringSubmatch(v.Rule)
			require.Len(t, m, 2, "rule %q", v.Rule)
			patterns["Ubuntu@"] = m[1]
		}
	}
	require.Len(t, patterns, 3)

	valid := []string{"v20260416", "v20251218a", "v20251218z"}
	invalid := []string{"20260416", "v2026041", "v202604161", "v20251218ab", "v20251218A", "v20251218-a", "va20251218"}
	for key, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		prefix := ""
		if key == "Ubuntu@" {
			prefix = key
			require.True(t, re.MatchString(prefix+"latest"), "%s rejects latest", key)
		}
		for _, version := range valid {
			require.True(t, re.MatchString(prefix+version), "%s rejects %q", key, version)
		}
		for _, version := range invalid {
			require.False(t, re.MatchString(prefix+version), "%s accepts %q", key, version)
		}
	}
}

func TestGCENodeClassCRDRestrictsLimitedSwapToFailSwapOnFalse(t *testing.T) {
	const rule = `!has(self.memorySwap) || !has(self.memorySwap.swapBehavior) || ` +
		`self.memorySwap.swapBehavior != 'LimitedSwap' || (has(self.failSwapOn) && self.failSwapOn == false)`

	var found bool
	for _, v := range kubeletConfigurationSchema(t).XValidations {
		if v.Rule == rule {
			found = true
			require.Equal(t,
				"memorySwap.swapBehavior LimitedSwap requires failSwapOn to be explicitly set to false",
				v.Message)
		}
	}
	require.True(t, found, "LimitedSwap must require failSwapOn false: the kubelet refuses to start with swap active otherwise")
}

func TestGCENodeClassCRDSwapBehaviorEnum(t *testing.T) {
	field := kubeletConfigurationSchema(t).Properties["memorySwap"].Properties["swapBehavior"]

	var values []string
	for _, v := range field.Enum {
		values = append(values, strings.Trim(string(v.Raw), `"`))
	}
	require.ElementsMatch(t, []string{"NoSwap", "LimitedSwap"}, values)
}

func TestGCENodeClassCRDDiskDeviceName(t *testing.T) {
	disks := specSchema(t).Properties["disks"]
	field := disks.Items.Schema.Properties["deviceName"]
	require.Equal(t, `^[a-z]([-a-z0-9]*[a-z0-9])?$`, field.Pattern)
	require.NotNil(t, field.MaxLength)
	require.Equal(t, int64(63), *field.MaxLength)

	var reserved, dataOnly, unique bool
	for _, v := range field.XValidations {
		reserved = reserved || v.Message == "deviceName must not start with persistent-disk-, local-ssd- or gke-, which GCE and GKE assign"
	}
	for _, v := range disks.Items.Schema.XValidations {
		dataOnly = dataOnly || v.Message == "deviceName is only supported on a data disk, not the boot disk or a secondary boot disk"
	}
	for _, v := range disks.XValidations {
		unique = unique || v.Message == "deviceName must be unique across disks"
	}
	require.True(t, reserved, "GCE assigns persistent-disk-N and local-ssd-N, and the provider names secondary boot disks gke-<image>-disk")
	require.True(t, dataOnly, "the provider sets the boot and secondary boot disks' device names itself")
	require.True(t, unique, "GCE rejects an instance whose disks share a device name")
}

func TestGCENodeClassCRDBoundsShutdownGracePeriodCriticalPods(t *testing.T) {
	const rule = `!has(self.shutdownGracePeriodCriticalPods) || (has(self.shutdownGracePeriod) && ` +
		`duration(self.shutdownGracePeriodCriticalPods) <= duration(self.shutdownGracePeriod))`

	var found bool
	for _, v := range kubeletConfigurationSchema(t).XValidations {
		if v.Rule == rule {
			found = true
			require.Equal(t, "shutdownGracePeriodCriticalPods requires shutdownGracePeriod and must not exceed it", v.Message)
		}
	}
	require.True(t, found, "the kubelet does not start when shutdownGracePeriodCriticalPods exceeds shutdownGracePeriod")
}

func TestGCENodeClassCRDShutdownDurationsSurviveTypedRoundTrip(t *testing.T) {
	var config KubeletConfiguration
	require.NoError(t, json.Unmarshal([]byte("{\"shutdownGracePeriod\":\"2us\",\"shutdownGracePeriodCriticalPods\":\"1us\"}"), &config))
	require.Equal(t, 2*time.Microsecond, config.ShutdownGracePeriod.Duration)
	require.Equal(t, time.Microsecond, config.ShutdownGracePeriodCriticalPods.Duration)

	raw, err := json.Marshal(config)
	require.NoError(t, err)
	var fields map[string]string
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Equal(t, "2µs", fields["shutdownGracePeriod"])
	require.Equal(t, "1µs", fields["shutdownGracePeriodCriticalPods"])
	for _, name := range []string{"shutdownGracePeriod", "shutdownGracePeriodCriticalPods"} {
		pattern := kubeletConfigurationSchema(t).Properties[name].Pattern
		require.Regexp(t, pattern, fields[name], "%s must accept the typed client's canonical duration", name)
	}
	var roundTripped KubeletConfiguration
	require.NoError(t, json.Unmarshal(raw, &roundTripped))
	require.Equal(t, config, roundTripped)
}

func TestGCENodeClassCRDShutdownGracePeriodsAreDurations(t *testing.T) {
	for _, name := range []string{"shutdownGracePeriod", "shutdownGracePeriodCriticalPods"} {
		field := kubeletConfigurationSchema(t).Properties[name]
		require.Equal(t, "string", field.Type, name)
		require.Equal(t, `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`, field.Pattern,
			"%s must be a non-negative Go duration: the controller cannot decode any other string", name)
	}
}
