/*
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

package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"

	"github.com/cloudpilot-ai/karpenter-provider-gcp/pkg/utils"
)

const (
	projectIDEnvVarName                   = "PROJECT_ID"
	projectIDFlagName                     = "project-id"
	clusterLocationEnvVarName             = "CLUSTER_LOCATION"
	clusterLocationFlagName               = "cluster-location"
	nodeLocationEnvVarName                = "NODE_LOCATION"
	nodeLocationFlagName                  = "node-location"
	gkeClusterNameEnvVarName              = "CLUSTER_NAME"
	gkeClusterFlagName                    = "cluster-name"
	vmMemoryOverheadPercentEnvVarName     = "VM_MEMORY_OVERHEAD_PERCENT"
	vmMemoryOverheadPercentFlagName       = "vm-memory-overhead-percent"
	gkeEnableInterruption                 = "INTERRUPTION"
	GCPAuth                               = "GOOGLE_APPLICATION_CREDENTIALS"
	nodePoolServiceAccountEnvVarName      = "DEFAULT_NODEPOOL_SERVICE_ACCOUNT"
	nodePoolServiceAccountFlagName        = "default-nodepool-service-account"
	defaultNodePoolTemplateNameEnvVarName = "DEFAULT_NODEPOOL_TEMPLATE_NAME"
	defaultNodePoolTemplateNameFlagName   = "default-nodepool-template-name"
	quotaExceededTTLEnvVarName            = "QUOTA_EXCEEDED_TTL"
	quotaExceededTTLFlagName              = "quota-exceeded-ttl"
	stockoutTTLEnvVarName                 = "STOCKOUT_TTL"
	stockoutTTLFlagName                   = "stockout-ttl"
	ipSpaceExhaustedTTLEnvVarName         = "IP_SPACE_EXHAUSTED_TTL"
	ipSpaceExhaustedTTLFlagName           = "ip-space-exhausted-ttl"
	unsupportedConfigurationTTLEnvVarName = "UNSUPPORTED_CONFIGURATION_TTL"
	unsupportedConfigurationTTLFlagName   = "unsupported-configuration-ttl"
	launchTimeoutEnvVarName               = "LAUNCH_TIMEOUT"
	launchTimeoutFlagName                 = "launch-timeout"

	// DefaultQuotaExceededTTL is how long an instance type stays unavailable in a
	// region after GCE rejects it for exceeded quota.
	DefaultQuotaExceededTTL = 30 * time.Minute
	// DefaultStockoutTTL is how long an offering stays unavailable after GCE
	// reports ZONE_RESOURCE_POOL_EXHAUSTED.
	DefaultStockoutTTL = 5 * time.Minute
	// DefaultIPSpaceExhaustedTTL is how long an offering stays unavailable after
	// GCE reports IP_SPACE_EXHAUSTED and no other pod range is left to try.
	DefaultIPSpaceExhaustedTTL = 30 * time.Second
	// DefaultUnsupportedConfigurationTTL is how long an offering stays unavailable
	// after GCE reports the zone cannot serve the requested configuration.
	DefaultUnsupportedConfigurationTTL = time.Hour
	// DefaultLaunchTimeout matches the karpenter core default for how long a
	// NodeClaim may fail to launch before it is deleted and replaced.
	DefaultLaunchTimeout = 5 * time.Minute
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

type Options struct {
	ProjectID               string
	ClusterLocation         string
	NodeLocation            string
	ClusterName             string
	VMMemoryOverheadPercent float64
	// GCPAuth is the path to the Google Application Credentials JSON file.
	// https://cloud.google.com/docs/authentication/application-default-credentials
	GCPAuth                     string
	NodePoolServiceAccount      string
	DefaultNodePoolTemplateName string
	Interruption                bool
	// QuotaExceededTTL is how long an instance type stays unavailable after a
	// QUOTA_EXCEEDED launch failure. Zero disables quota backoff.
	QuotaExceededTTL time.Duration
	// StockoutTTL is how long an offering stays unavailable after a
	// ZONE_RESOURCE_POOL_EXHAUSTED launch failure.
	StockoutTTL time.Duration
	// IPSpaceExhaustedTTL is how long an offering stays unavailable after an
	// IP_SPACE_EXHAUSTED launch failure.
	IPSpaceExhaustedTTL time.Duration
	// UnsupportedConfigurationTTL is how long an offering stays unavailable after
	// GCE reports the zone does not support the requested configuration.
	UnsupportedConfigurationTTL time.Duration
	// LaunchTimeout is how long a NodeClaim may keep failing to launch with a
	// non-capacity error before karpenter core deletes and replaces it.
	LaunchTimeout time.Duration
}

func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ProjectID, projectIDFlagName, env.WithDefaultString(projectIDEnvVarName, ""), "GCP project ID where the GKE cluster is running.")
	fs.StringVar(&o.ClusterLocation, clusterLocationFlagName, env.WithDefaultString(clusterLocationEnvVarName, ""), "GCP region for instance type discovery (e.g., us-central1).")
	fs.StringVar(&o.NodeLocation, nodeLocationFlagName, env.WithDefaultString(nodeLocationEnvVarName, ""), "Exact GCP location of the GKE cluster for API calls (e.g., us-central1-a for zonal, us-central1 for regional). Defaults to 'clusterLocation' if not set.")
	fs.StringVar(&o.ClusterName, gkeClusterFlagName, env.WithDefaultString(gkeClusterNameEnvVarName, ""), "Name of the GKE cluster that provisioned nodes should connect to.")
	fs.Float64Var(&o.VMMemoryOverheadPercent, vmMemoryOverheadPercentFlagName, utils.WithDefaultFloat64(vmMemoryOverheadPercentEnvVarName, 0.07), "Percentage of memory overhead for VM. If not set, the controller will use the default value 7%.")
	fs.StringVar(&o.GCPAuth, GCPAuth, env.WithDefaultString(GCPAuth, ""), "Path to the Google Application Credentials JSON file. If not set, the controller will use the default credentials from the environment.")
	fs.StringVar(&o.NodePoolServiceAccount, nodePoolServiceAccountFlagName, env.WithDefaultString(nodePoolServiceAccountEnvVarName, ""), "Service account to use for default node pool templates. If not set, uses <project number>-compute@developer.gserviceaccount.com")
	fs.StringVar(&o.DefaultNodePoolTemplateName, defaultNodePoolTemplateNameFlagName, env.WithDefaultString(defaultNodePoolTemplateNameEnvVarName, ""), "Pin the bootstrap source pool by name. If set, Karpenter uses this pool exclusively and returns an error if it is not RUNNING.")
	fs.BoolVar(&o.Interruption, gkeEnableInterruption, env.WithDefaultBool(gkeEnableInterruption, true), "Enable interruption handling.")
	fs.DurationVar(&o.QuotaExceededTTL, quotaExceededTTLFlagName, env.WithDefaultDuration(quotaExceededTTLEnvVarName, DefaultQuotaExceededTTL), "How long an instance type stays unavailable in the region after a launch fails with QUOTA_EXCEEDED. Set to 0 to disable quota backoff.")
	fs.DurationVar(&o.StockoutTTL, stockoutTTLFlagName, env.WithDefaultDuration(stockoutTTLEnvVarName, DefaultStockoutTTL), "How long an instance type, zone and capacity type stays unavailable after a launch fails with ZONE_RESOURCE_POOL_EXHAUSTED. Must be positive.")
	fs.DurationVar(&o.IPSpaceExhaustedTTL, ipSpaceExhaustedTTLFlagName, env.WithDefaultDuration(ipSpaceExhaustedTTLEnvVarName, DefaultIPSpaceExhaustedTTL), "How long an instance type, zone and capacity type stays unavailable after a launch fails with IP_SPACE_EXHAUSTED on every pod range. Must be positive.")
	fs.DurationVar(&o.UnsupportedConfigurationTTL, unsupportedConfigurationTTLFlagName, env.WithDefaultDuration(unsupportedConfigurationTTLEnvVarName, DefaultUnsupportedConfigurationTTL), "How long an instance type, zone and capacity type stays unavailable after GCE reports the zone does not support the requested configuration (MACHINE_TYPE_UNSUPPORTED or configuration_availability). Must be positive.")
	fs.DurationVar(&o.LaunchTimeout, launchTimeoutFlagName, env.WithDefaultDuration(launchTimeoutEnvVarName, DefaultLaunchTimeout), "How long a NodeClaim may keep failing to launch with a non-capacity error before it is deleted and replaced. Must be positive.")
}

func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags, %w", err)
	}

	// Backward compatibility: if NodeLocation not set, default to ClusterLocation
	if o.NodeLocation == "" {
		o.NodeLocation = o.ClusterLocation
	}

	if err := o.Validate(); err != nil {
		return fmt.Errorf("validating options, %w", err)
	}
	return nil
}

func (o *Options) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, o)
}

func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func FromContext(ctx context.Context) *Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		return nil
	}
	return retval.(*Options)
}
