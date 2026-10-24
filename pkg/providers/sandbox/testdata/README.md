# Reviewed bootstrap consumers

`bootstrap-profile.json` records the public source URL, SHA256 of the original
archive, exact member fingerprints, accepted schema and independently maintained
native-policy revision. Public source read on 2026-10-22; no private metadata or
credentials are included.

`bootstrap-consumers.tar.gz` contains only the three unmodified public consumer
files at their original full paths. Tar metadata is normalized and gzip mtime is
zero; its archive digest intentionally differs from the original archive. No
script is executed. These files include their upstream copyright notices.

Reviewed boundaries: gci configure orchestration, internal helper's containerd
runtime/SMT/installables consumers, and installable.py's component/object and
container digest/run schema. Native xemu/SMT/nftables and hardware-specific core
tags are independently based on the seven sanitized native-template comparisons,
not inferred solely from these files. Matching files is consumer compatibility,
not discovery of control-plane policy. Changes require consumer and native-policy
review plus offline regression and separately authorized live acceptance before
updating the accepted profile. Do not update hashes just to unblock an upgrade.

`bootstrap-original.tar.gz` is the complete, byte-for-byte public archive named
in `bootstrap-profile.json` (SHA256 `e8282f2b799b7147de5b1388d8f7b276be0eecec1e1ae5bbf3901ba7a68652b8`).
It is retained to verify that the conservative canonical regular-file/directory
archive contract accepts the original reviewed artifact, not just the reduced
consumer fixture. It contains 97 public members and no links. Never execute or
extract it during validation.
