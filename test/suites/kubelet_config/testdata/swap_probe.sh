#!/bin/sh
# Usage: swap_probe.sh ENCRYPTED EXPECTED_KIB [HOST_ROOT]
# Reads mounted host state only; HOST_ROOT defaults to /host.
set -eu

encrypted="$1"
expected_kib="$2"
host_root="${3:-/host}"
swaps="$host_root/proc/swaps"

test -r "$swaps" || { echo "cannot read $swaps"; exit 1; }
echo "active swaps:"
cat "$swaps"

encswap=""
for dmname in "$host_root"/sys/block/dm-*/dm/name; do
  test -r "$dmname" || continue
  if [ "$(cat "$dmname")" = "encswap" ]; then
    encswap="/dev/$(basename "$(dirname "$(dirname "$dmname")")")"
  fi
done

if [ "$encrypted" = "true" ]; then
  test -n "$encswap" || { echo "no encswap device-mapper device"; exit 1; }
  grep -Eq "^($encswap|/dev/mapper/encswap)[[:space:]]" "$swaps" || {
    echo "the encrypted device is not the active swap"; exit 1;
  }
else
  test -z "$encswap" || { echo "encswap exists although encryption is disabled: $encswap"; exit 1; }
  grep -q '^/mnt/stateful_partition/swap/swapfile[[:space:]]' "$swaps" || {
    echo "the GKE swap file is not the active swap"; exit 1;
  }
  test -f "$host_root/swap/swapfile" || { echo "swap file missing"; exit 1; }
fi

# /proc/swaps reports KiB. Allow less than 1 MiB for the mkswap header,
# but reject a larger active size or additional swap entries.
awk -v expectedKiB="$expected_kib" '
  NR > 1 && NF { count++; sizeKiB = $3 }
  END {
    if (count != 1 || sizeKiB > expectedKiB || sizeKiB <= expectedKiB - 1024) {
      print "expected one active swap of", expectedKiB, "KiB (less than 1 MiB header tolerance); found", count, "entries, size", sizeKiB
      exit 1
    }
  }
' "$swaps"
