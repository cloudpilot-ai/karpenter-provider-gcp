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

import "fmt"

// swapProbeScript checks the active backing and size from the host kernel,
// rather than relying on the requested metadata or kubelet-reported total.
func swapProbeScript(encrypted bool, sizeGiB int64) string {
	var expectation string
	if encrypted {
		expectation = `test -n "$encswap" || { echo "no encswap device-mapper device"; exit 1; }
grep -Eq "^($encswap|/dev/mapper/encswap)[[:space:]]" "$swaps" || { echo "the encrypted device is not the active swap"; exit 1; }`
	} else {
		expectation = `test -z "$encswap" || { echo "encswap exists although encryption is disabled: $encswap"; exit 1; }
grep -q "^` + gkeSwapFile + `[[:space:]]" "$swaps" || { echo "the GKE swap file is not the active swap"; exit 1; }
test -f /host/swap/swapfile || { echo "swap file missing"; exit 1; }`
	}

	// /proc/swaps reports KiB. mkswap reserves a header page; allow up to
	// 1 MiB less than the file, but reject extra swap or a larger active size.
	return `set -eu
swaps=/host/proc/swaps
test -r "$swaps" || { echo "cannot read $swaps"; exit 1; }
echo "active swaps:"; cat "$swaps"
encswap=""
for dmname in /host/sys/block/dm-*/dm/name; do
  test -r "$dmname" || continue
  if [ "$(cat "$dmname")" = "encswap" ]; then
    encswap="/dev/$(basename "$(dirname "$(dirname "$dmname")")")"
  fi
done
` + expectation + fmt.Sprintf(`
awk -v expectedKiB=%d '
  NR > 1 && NF { count++; sizeKiB = $3 }
  END {
    if (count != 1 || sizeKiB > expectedKiB || sizeKiB <= expectedKiB - 1024) {
      print "expected one active swap of", expectedKiB, "KiB (less than 1 MiB header tolerance); found", count, "entries, size", sizeKiB
      exit 1
    }
  }
' "$swaps"
`, sizeGiB*1024*1024)
}
