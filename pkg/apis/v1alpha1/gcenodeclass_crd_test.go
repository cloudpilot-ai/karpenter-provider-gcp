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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celvalidator "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
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

func TestGCENodeClassCRDMovesLimitedSwapRuleToTheSpec(t *testing.T) {
	// The kubelet defaults of a node with provider-managed swap are decided at
	// the spec level, so the LimitedSwap rule has to live there too: a nested
	// rule cannot see linuxNodeConfig.swapConfig.
	for _, v := range kubeletConfigurationSchema(t).XValidations {
		require.NotContains(t, v.Message, "LimitedSwap requires failSwapOn",
			"a nested rule cannot allow the provider to default failSwapOn for enabled swap")
	}

	const message = "memorySwap.swapBehavior LimitedSwap requires kubeletConfiguration.failSwapOn to be explicitly set to false unless linuxNodeConfig.swapConfig is enabled"
	var found bool
	for _, v := range specSchema(t).XValidations {
		found = found || v.Message == message
	}
	require.True(t, found, "LimitedSwap must still require an explicit failSwapOn false without provider-managed swap")
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

// validateSpecAdmission runs the generated CRD schema's CEL validation over a
// spec, the way the API server does. The Kubernetes structural validator walks
// every level of the schema, so the nested kubeletConfiguration, disk and
// swapConfig rules run as well as the spec-level ones, instead of a
// reimplementation of admission that only sees the root rules.
func validateSpecAdmission(t *testing.T, spec map[string]any) field.ErrorList {
	t.Helper()
	specProperties := specSchema(t)
	var internalSchema apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(&specProperties, &internalSchema, nil))
	structural, err := structuralschema.NewStructural(&internalSchema)
	require.NoError(t, err)
	validator := celvalidator.NewValidator(structural, true, celconfig.PerCallLimit)
	require.NotNil(t, validator, "the generated spec schema must carry CEL rules")
	errs, _ := validator.Validate(context.Background(), field.NewPath("spec"), structural, spec, nil, celconfig.RuntimeCELCostBudget)
	return errs
}

func requireAdmitted(t *testing.T, spec map[string]any) {
	t.Helper()
	require.Empty(t, validateSpecAdmission(t, spec), "the spec must be accepted")
}

func requireRejected(t *testing.T, spec map[string]any, message string) {
	t.Helper()
	errs := validateSpecAdmission(t, spec)
	require.NotEmpty(t, errs, "the spec must be rejected")
	require.Contains(t, errs.ToAggregate().Error(), message)
}

func swapSchema(t *testing.T) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	return specSchema(t).Properties["linuxNodeConfig"].Properties["swapConfig"]
}

func swapSpec(swap map[string]any, extra map[string]any) map[string]any {
	spec := map[string]any{"linuxNodeConfig": map[string]any{"swapConfig": swap}}
	for key, value := range extra {
		spec[key] = value
	}
	return spec
}

func bootDisk(sizeGiB int64) []any {
	return []any{map[string]any{"boot": true, "sizeGiB": sizeGiB}}
}

func TestGCENodeClassCRDAcceptsMinimalEnabledSwap(t *testing.T) {
	requireAdmitted(t, swapSpec(map[string]any{"enabled": true}, nil))
}

// TestGCENodeClassCRDAcceptsEnabledSwapWithLimitedSwapDefaults reproduces the
// offline admission fixture: enabled swap with an explicit LimitedSwap and no
// failSwapOn. The provider defaults failSwapOn to false for such a node, so
// admission must not demand the explicit value that only a node without
// provider-managed swap needs.
func TestGCENodeClassCRDAcceptsEnabledSwapWithLimitedSwapDefaults(t *testing.T) {
	requireAdmitted(t, swapSpec(map[string]any{"enabled": true}, map[string]any{
		"kubeletConfiguration": map[string]any{
			"memorySwap": map[string]any{"swapBehavior": "LimitedSwap"},
		},
	}))
}

func TestGCENodeClassCRDRejectsFailSwapOnTrueWithEnabledSwap(t *testing.T) {
	requireRejected(t, swapSpec(map[string]any{"enabled": true}, map[string]any{
		"kubeletConfiguration": map[string]any{"failSwapOn": true},
	}), "conflicts with kubeletConfiguration.failSwapOn true")

	requireAdmitted(t, swapSpec(map[string]any{"enabled": true}, map[string]any{
		"kubeletConfiguration": map[string]any{"failSwapOn": false},
	}))
}

// TestGCENodeClassCRDRejectsLimitedSwapWithoutFailSwapOnWithoutProviderSwap
// keeps the existing kubelet contract for every node class that does not create
// its own swap: the kubelet refuses to start with swap active otherwise.
func TestGCENodeClassCRDRejectsLimitedSwapWithoutFailSwapOnWithoutProviderSwap(t *testing.T) {
	limitedSwap := map[string]any{"memorySwap": map[string]any{"swapBehavior": "LimitedSwap"}}
	const message = "LimitedSwap requires kubeletConfiguration.failSwapOn to be explicitly set to false"

	requireRejected(t, map[string]any{"kubeletConfiguration": limitedSwap}, message)
	requireRejected(t, swapSpec(map[string]any{"enabled": false}, map[string]any{
		"kubeletConfiguration": limitedSwap,
	}), message)
	requireAdmitted(t, map[string]any{"kubeletConfiguration": map[string]any{
		"failSwapOn": false,
		"memorySwap": map[string]any{"swapBehavior": "LimitedSwap"},
	}})
}

func TestGCENodeClassCRDRejectsSwapSettingsWhenDisabled(t *testing.T) {
	// These rules live on the nested SwapConfig schema, so they also prove the
	// structural traversal reaches nested validation.
	for name, swap := range map[string]map[string]any{
		"boot disk profile without enabled": {"bootDiskProfile": map[string]any{"swapSizeGib": int64(4)}},
		"encryption without enabled":        {"encryptionConfig": map[string]any{"disabled": true}},
		"explicitly disabled with profile":  {"enabled": false, "bootDiskProfile": map[string]any{"swapSizeGib": int64(4)}},
	} {
		t.Run(name, func(t *testing.T) {
			requireRejected(t, swapSpec(swap, nil), "must not be set when swapConfig is not enabled")
		})
	}
	requireAdmitted(t, swapSpec(map[string]any{"enabled": false}, nil))
	requireAdmitted(t, swapSpec(map[string]any{}, nil))
}

// TestGCENodeClassCRDReachesNestedRules shows the validator used by these tests
// evaluates rules below the spec, not only the spec-level ones.
func TestGCENodeClassCRDReachesNestedRules(t *testing.T) {
	requireRejected(t, map[string]any{
		"disks": []any{map[string]any{"boot": true, "sizeGiB": int64(100), "deviceName": "data0"}},
	}, "deviceName is only supported on a data disk")
}

func TestGCENodeClassCRDLimitsSwapOnDefaultBootDisk(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"no disks":        nil,
		"only data disks": {"disks": []any{map[string]any{"sizeGiB": int64(200)}}},
	} {
		t.Run(name, func(t *testing.T) {
			requireAdmitted(t, swapSpec(map[string]any{
				"enabled": true, "bootDiskProfile": map[string]any{"swapSizeGib": int64(50)},
			}, extra))
			requireRejected(t, swapSpec(map[string]any{
				"enabled": true, "bootDiskProfile": map[string]any{"swapSizeGib": int64(51)},
			}, extra), "at most 50GiB")
		})
	}
}

func TestGCENodeClassCRDLimitsSwapToHalfTheBootDisk(t *testing.T) {
	swap := map[string]any{"enabled": true, "bootDiskProfile": map[string]any{"swapSizeGib": int64(50)}}
	requireAdmitted(t, swapSpec(swap, map[string]any{"disks": bootDisk(100)}))

	for name, tc := range map[string]struct {
		swap map[string]any
		disk int64
	}{
		"oversize swap":                 {swap: map[string]any{"enabled": true, "bootDiskProfile": map[string]any{"swapSizeGib": int64(51)}}, disk: 100},
		"swap larger than a small disk": {swap: map[string]any{"enabled": true, "bootDiskProfile": map[string]any{"swapSizeGib": int64(11)}}, disk: 20},
	} {
		t.Run(name, func(t *testing.T) {
			requireRejected(t, swapSpec(tc.swap, map[string]any{"disks": bootDisk(tc.disk)}),
				"must be at most half of the boot disk sizeGiB")
		})
	}

	// An automatic size is resolved against the machine, so admission cannot
	// bound it by the disk.
	requireAdmitted(t, swapSpec(map[string]any{"enabled": true}, map[string]any{"disks": bootDisk(20)}))
}

func TestGCENodeClassCRDSwapRuleToleratesDisksWithoutBootField(t *testing.T) {
	// A data disk that omits boot must not make the rule fail with
	// "no such key: boot", which rejected valid node classes before.
	requireAdmitted(t, swapSpec(map[string]any{"enabled": true}, map[string]any{
		"disks": []any{
			map[string]any{"boot": true, "sizeGiB": int64(100)},
			map[string]any{"sizeGiB": int64(200)},
		},
	}))
}

func TestGCENodeClassCRDSwapSchemaShape(t *testing.T) {
	swap := swapSchema(t)
	require.NotContains(t, swap.Required, "enabled", "an omitted enabled keeps swap disabled")
	require.NotContains(t, swap.Required, "bootDiskProfile", "an omitted bootDiskProfile selects the automatic size")

	size := swap.Properties["bootDiskProfile"].Properties["swapSizeGib"]
	require.NotNil(t, size.Minimum)
	require.Equal(t, float64(1), *size.Minimum)
	require.NotContains(t, swap.Properties["bootDiskProfile"].Required, "swapSizeGib",
		"an omitted swapSizeGib selects the automatic size")
}

func TestGCENodeClassSwapConfigDeepCopyKeepsAutomaticSize(t *testing.T) {
	nodeClass := &GCENodeClass{Spec: GCENodeClassSpec{LinuxNodeConfig: &LinuxNodeConfig{
		SwapConfig: &SwapConfig{Enabled: true},
	}}}
	copied := nodeClass.DeepCopy()
	require.True(t, copied.BootDiskSwapConfig().Enabled)
	require.Zero(t, copied.BootDiskSwapRequestedGiB(), "an omitted size stays automatic")
	require.True(t, copied.BootDiskSwapEncrypted(), "encryption is on unless disabled")

	nodeClass.Spec.LinuxNodeConfig.SwapConfig.BootDiskProfile = &SwapBootDiskProfile{SwapSizeGib: lo.ToPtr(int64(8))}
	nodeClass.Spec.LinuxNodeConfig.SwapConfig.EncryptionConfig = &SwapEncryptionConfig{Disabled: lo.ToPtr(true)}
	copied = nodeClass.DeepCopy()
	require.Equal(t, int64(8), copied.BootDiskSwapRequestedGiB())
	require.False(t, copied.BootDiskSwapEncrypted())
	require.Nil(t, (&GCENodeClass{}).BootDiskSwapConfig())
}
