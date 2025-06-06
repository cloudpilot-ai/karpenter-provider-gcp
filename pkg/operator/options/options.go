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

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"
)

const (
	projectIDEnvVarName      = "PROJECT_ID"
	projectIDFlagName        = "project-id"
	regionEnvVarName         = "REGION"
	regionFlagName           = "region"
	gkeClusterNameEnvVarName = "CLUSTER_NAME"
	gkeClusterFlagName       = "cluster-name"
	gkeEnableInterruption    = "INTERRUPTION"
	GCPAuth                  = "GOOGLE_APPLICATION_CREDENTIALS"
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

type Options struct {
	ProjectID   string
	Region      string
	ClusterName string
	// GCPAuth is the path to the Google Application Credentials JSON file.
	// https://cloud.google.com/docs/authentication/application-default-credentials
	GCPAuth      string
	Interruption bool
}

func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ProjectID, projectIDFlagName, env.WithDefaultString(projectIDEnvVarName, ""), "GCP project ID where the GKE cluster is running.")
	fs.StringVar(&o.Region, regionFlagName, env.WithDefaultString(regionEnvVarName, ""), "GCP region of the GKE cluster. If not set, the controller will attempt to infer it.")
	fs.StringVar(&o.ClusterName, gkeClusterFlagName, env.WithDefaultString(gkeClusterNameEnvVarName, ""), "Name of the GKE cluster that provisioned nodes should connect to.")
	fs.StringVar(&o.GCPAuth, GCPAuth, env.WithDefaultString(GCPAuth, ""), "Path to the Google Application Credentials JSON file. If not set, the controller will use the default credentials from the environment.")
	fs.BoolVar(&o.Interruption, gkeEnableInterruption, env.WithDefaultBool(gkeEnableInterruption, true), "Enable interruption handling.")
}

func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags, %w", err)
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
