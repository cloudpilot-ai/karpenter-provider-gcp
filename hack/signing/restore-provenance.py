#!/usr/bin/env python3
"""Restore signing assets before replacing a Pages chart repository."""
from pathlib import Path
import re
import sys
from urllib.error import HTTPError
from urllib.request import urlopen


ARCHIVE = re.compile(r"karpenter(?:-crd)?-[0-9][A-Za-z0-9.+-]*\.tgz")
FINGERPRINT = re.compile(r"[A-F0-9]{40}")


def fetch(base, name, optional=False):
    try:
        with urlopen(f"{base.rstrip('/')}/{name}", timeout=60) as response:
            return response.read()
    except HTTPError as error:
        if optional and error.code == 404:
            return None
        raise


def records(data):
    result = {}
    for line in data.decode().splitlines():
        archive, fingerprint = line.split()
        if not ARCHIVE.fullmatch(archive) or not FINGERPRINT.fullmatch(fingerprint):
            raise ValueError(f"Invalid signed chart record: {line}")
        result[archive] = fingerprint
    return result


def restore(base, destination):
    previous = records(fetch(base, "signed-charts.txt", optional=True) or b"")
    manifest = destination / "signed-charts.txt"
    current = records(manifest.read_bytes()) if manifest.exists() else {}
    for archive in previous:
        if not (destination / archive).is_file():
            raise ValueError(f"Previously signed chart is missing: {archive}")
    for archive in destination.glob("*.tgz"):
        provenance = archive.with_suffix(".tgz.prov")
        if not provenance.exists():
            data = fetch(base, provenance.name, optional=archive.name not in previous)
            if data is not None:
                provenance.write_bytes(data)
    for fingerprint in set(previous.values()):
        public_key = destination / "keys" / f"{fingerprint}.asc"
        if not public_key.exists():
            public_key.parent.mkdir(exist_ok=True)
            public_key.write_bytes(fetch(base, f"keys/{fingerprint}.asc"))
    previous.update(current)
    manifest.write_text("".join(f"{archive} {fingerprint}\n" for archive, fingerprint in sorted(previous.items())))


if __name__ == "__main__":
    restore(sys.argv[1], Path(sys.argv[2]))
