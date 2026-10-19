"""Discover and verify complete OCI graphs, including referrers and legacy digest tags."""

import hashlib
import json
import re

from registry import MirrorError

DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def validate_descriptor(descriptor):
    if not isinstance(descriptor, dict) or not DIGEST.fullmatch(descriptor.get("digest", "")):
        raise MirrorError("Only complete sha256 descriptors are supported")
    if not isinstance(descriptor.get("size"), int) or descriptor["size"] < 0:
        raise MirrorError("Invalid OCI descriptor size")


def platform_name(config):
    os_name, arch = config.get("os"), config.get("architecture")
    if not os_name or not arch or "unknown" in (os_name, arch):
        return None
    suffix = f"/{config['variant']}" if config.get("variant") else ""
    return f"{os_name}/{arch}{suffix}"


class Graph:
    def __init__(self, client, repository, root):
        self.client, self.repository, self.root = client, repository, root
        self.manifests, self.blobs, self.referrers, self.legacy_tags = {}, {}, {}, {}
        self.platforms = set()
        self.tags = client.call("tag", "ls", repository).decode().splitlines()

    def fetch_manifest(self, digest):
        raw = self.client.call("manifest", "get", f"{self.repository}@{digest}", "--format", "raw-body")
        if f"sha256:{hashlib.sha256(raw).hexdigest()}" != digest:
            raise MirrorError(f"Manifest bytes do not match {digest}")
        manifest = json.loads(raw)
        if not isinstance(manifest, dict) or (manifest.get("schemaVersion") != 2 and
                manifest.get("mediaType") != "application/vnd.oci.artifact.manifest.v1+json"):
            raise MirrorError("Only OCI images/artifacts and Docker schema-2 manifests are supported")
        self.manifests[digest] = {"size": len(raw), "body": manifest}
        return manifest

    def add_blob(self, descriptor):
        validate_descriptor(descriptor)
        if descriptor.get("urls"):
            raise MirrorError("External/foreign layers require an explicit delivery policy; refusing an incomplete mirror")
        previous = self.blobs.get(descriptor["digest"])
        if previous and previous["size"] != descriptor["size"]:
            raise MirrorError("Conflicting sizes for the same blob digest")
        self.blobs[descriptor["digest"]] = descriptor

    def visit(self, digest, runtime_image=True):
        if digest in self.manifests:
            return
        if not DIGEST.fullmatch(digest) or len(self.manifests) >= 4096:
            raise MirrorError("Invalid or excessively large manifest graph")
        manifest = self.fetch_manifest(digest)
        for descriptor in manifest.get("manifests", []):
            validate_descriptor(descriptor)
            self.visit(descriptor["digest"], runtime_image=runtime_image)
            if self.manifests[descriptor["digest"]]["size"] != descriptor["size"]:
                raise MirrorError("Child manifest size mismatch")
        config = manifest.get("config")
        if config:
            self.add_blob(config)
            # Read actual image config rather than trusting index platform annotations.
            if config.get("mediaType") in ("application/vnd.oci.image.config.v1+json", "application/vnd.docker.container.image.v1+json"):
                raw = self.client.call("blob", "get", self.repository, config["digest"])
                if f"sha256:{hashlib.sha256(raw).hexdigest()}" != config["digest"] or len(raw) != config["size"]:
                    raise MirrorError("Image config digest or size mismatch")
                name = platform_name(json.loads(raw))
                if name and runtime_image and not manifest.get("artifactType"):
                    self.platforms.add(name)
        for descriptor in manifest.get("layers", []) + manifest.get("blobs", []):
            self.add_blob(descriptor)
        descriptors = self.client.json("artifact", "list", f"{self.repository}@{digest}",
                                       "--format", "{{json .Descriptors}}") or []
        self.referrers[digest] = sorted(d["digest"] for d in descriptors)
        for descriptor in descriptors:
            validate_descriptor(descriptor)
            self.visit(descriptor["digest"], runtime_image=False)
        prefix = digest.replace(":", "-", 1) + "."
        for tag in self.tags:
            if tag.startswith(prefix):
                artifact_digest = self.client.digest(f"{self.repository}:{tag}")
                self.legacy_tags[tag] = artifact_digest
                self.visit(artifact_digest, runtime_image=False)

    def discover(self):
        self.visit(self.root)
        if not self.platforms:
            raise MirrorError("The source contains no runnable image platform")
        return self

    def inventory(self):
        return {"manifests": self.manifests, "blobs": self.blobs,
                "referrers": self.referrers, "legacyTags": self.legacy_tags,
                "platforms": sorted(self.platforms)}

    def verify_target(self, target):
        for digest, entry in self.manifests.items():
            if target.manifests.get(digest) != entry:
                raise MirrorError(f"Destination is missing or changed manifest {digest}")
        for subject, refs in self.referrers.items():
            if not set(refs).issubset(target.referrers.get(subject, [])):
                raise MirrorError(f"Destination is missing referrers for {subject}")
        for tag, digest in self.legacy_tags.items():
            if target.legacy_tags.get(tag) != digest:
                raise MirrorError(f"Destination is missing legacy artifact tag {tag}")
        if self.platforms != target.platforms:
            raise MirrorError("Destination runtime platforms differ from the source")
        for descriptor in self.blobs.values():
            target.client.verify_blob(target.repository, descriptor)
