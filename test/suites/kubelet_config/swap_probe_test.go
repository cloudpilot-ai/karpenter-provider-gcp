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

package kubelet_config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the script sent to the real probe, with only /host relocated to a
// temporary fixture directory. These tests never contact Kubernetes or GCP.
func TestSwapProbeScript(t *testing.T) {
	const sizeKiB int64 = 4194304 // 4 GiB, independent of the sizing resolver.
	for name, tc := range map[string]struct {
		encrypted bool
		encswap   bool
		backing   string
		sizeKiB   int64
		extraSwap bool
		tabs      bool
		wantError string
	}{
		"encrypted":              {encrypted: true, encswap: true, backing: "/dev/dm-0", sizeKiB: sizeKiB - 4},
		"encrypted mapper alias": {encrypted: true, encswap: true, backing: "/dev/mapper/encswap", sizeKiB: sizeKiB - 4},
		"plain file":             {backing: gkeSwapFile, sizeKiB: sizeKiB - 4},
		"tab-delimited fields":   {backing: gkeSwapFile, sizeKiB: sizeKiB - 4, tabs: true},
		"exact requested size":   {backing: gkeSwapFile, sizeKiB: sizeKiB},
		"inside tolerance":       {backing: gkeSwapFile, sizeKiB: sizeKiB - 1023},
		"at tolerance boundary":  {backing: gkeSwapFile, sizeKiB: sizeKiB - 1024, wantError: "expected one active swap"},
		"oversized":              {backing: gkeSwapFile, sizeKiB: sizeKiB + 1, wantError: "expected one active swap"},
		"missing mapping":        {encrypted: true, backing: "/dev/dm-0", sizeKiB: sizeKiB, wantError: "no encswap"},
		"inactive mapping":       {encrypted: true, encswap: true, backing: "/unrelated/swapfile", sizeKiB: sizeKiB, wantError: "not the active swap"},
		"plain with mapping":     {encswap: true, backing: gkeSwapFile, sizeKiB: sizeKiB, wantError: "encryption is disabled"},
		"extra active swap":      {backing: gkeSwapFile, sizeKiB: sizeKiB, extraSwap: true, wantError: "expected one active swap"},
		"wrong plain backing":    {backing: "/unrelated/swapfile", sizeKiB: sizeKiB, wantError: "not the active swap"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			// Linux pads the filename to 40 columns (at least one space),
			// then separates the remaining fields with tabs.
			separator := strings.Repeat(" ", max(1, 40-len(tc.backing)))
			if tc.tabs {
				separator = "\t"
			}
			files := map[string]string{
				"proc/swaps":    fmt.Sprintf("Filename\tType\tSize\tUsed\tPriority\n%s%sfile\t%d\t0\t10\n", tc.backing, separator, tc.sizeKiB),
				"swap/swapfile": "",
			}
			if tc.encswap {
				files["sys/block/dm-0/dm/name"] = "encswap\n"
			}
			if tc.extraSwap {
				files["proc/swaps"] += "/unrelated/swapfile\tfile\t1024\t0\t5\n"
			}
			for path, content := range files {
				path = filepath.Join(root, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}

			script := strings.ReplaceAll(swapProbeScript(tc.encrypted, 4), "/host/", root+"/")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "sh", "-c", script).CombinedOutput()
			if tc.wantError == "" {
				require.NoError(t, err, "%s", output)
				return
			}
			require.Error(t, err, "%s", output)
			require.Contains(t, string(output), tc.wantError)
		})
	}
}
