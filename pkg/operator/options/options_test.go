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
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
)

// Env vars are read when flags are registered, so these tests use t.Setenv and
// cannot run in parallel.
func TestQuotaExceededTTL(t *testing.T) {
	requiredArgs := []string{"--project-id=p", "--cluster-name=c", "--cluster-location=us-central1"}

	tests := []struct {
		name    string
		env     *string
		args    []string
		want    time.Duration
		wantErr string
	}{
		{name: "unset uses default", want: DefaultQuotaExceededTTL},
		{name: "env custom duration", env: ptr.To("1h"), want: time.Hour},
		{name: "flag custom duration", args: []string{"--quota-exceeded-ttl=45m"}, want: 45 * time.Minute},
		{name: "env 0s disables backoff", env: ptr.To("0s"), want: 0},
		{name: "flag 0s disables backoff", args: []string{"--quota-exceeded-ttl=0s"}, want: 0},
		{name: "env negative is rejected", env: ptr.To("-5m"), wantErr: "quota-exceeded-ttl must not be negative"},
		{name: "flag negative is rejected", args: []string{"--quota-exceeded-ttl=-5m"}, wantErr: "quota-exceeded-ttl must not be negative"},
		{name: "flag takes precedence over env", env: ptr.To("1h"), args: []string{"--quota-exceeded-ttl=2h"}, want: 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(quotaExceededTTLEnvVarName, "")
			if tt.env != nil {
				t.Setenv(quotaExceededTTLEnvVarName, *tt.env)
			} else {
				require.NoError(t, os.Unsetenv(quotaExceededTTLEnvVarName))
			}

			o := &Options{}
			fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("test", flag.ContinueOnError)}
			o.AddFlags(fs)
			err := o.Parse(fs, append(requiredArgs, tt.args...)...)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, o.QuotaExceededTTL)
		})
	}
}
