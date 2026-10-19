#!/usr/bin/env python3
"""Mirror existing container images only: no builds, deployment, or source-tag writes."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

from graph import DIGEST, Graph
from registry import MirrorError, Registry, redact

TAG = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")
REPOSITORY = re.compile(r"[a-z0-9.-]+(?::[0-9]+)?/[a-z0-9]+(?:(?:[._-]|/)[a-z0-9]+)*\Z")
REQUEST_KEYS = {"schemaVersion", "sourceRef", "targetRepository", "immutableTags", "mutableTags"}


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":")) + "\n").encode()


def write_json(path, value):
    path.write_bytes(canonical(value))


def validate_request(value):
    if not isinstance(value, dict) or set(value) != REQUEST_KEYS or type(value["schemaVersion"]) is not int or value["schemaVersion"] != 1:
        raise MirrorError("Invalid mirror request schema")
    source = value["sourceRef"]
    if not isinstance(source, str) or source.count("@") != 1:
        raise MirrorError("source-ref must be an explicit repository@sha256 digest")
    repository, digest = source.split("@")
    if not REPOSITORY.fullmatch(repository) or not DIGEST.fullmatch(digest):
        raise MirrorError("source-ref must contain an explicit registry/repository and full sha256 digest")
    if not isinstance(value["targetRepository"], str) or not REPOSITORY.fullmatch(value["targetRepository"]):
        raise MirrorError("target-repository must be an explicit registry/repository without a tag")
    for key in ("immutableTags", "mutableTags"):
        tags = value[key]
        if not isinstance(tags, list) or len(tags) > 64 or any(not isinstance(tag, str) or not TAG.fullmatch(tag) for tag in tags):
            raise MirrorError(f"{key} must be a bounded JSON array of valid OCI tags")
        if len(tags) != len(set(tags)):
            raise MirrorError(f"Duplicate {key}")
        if any(tag.startswith("mirror-sha256-") or tag.startswith("sha256-") for tag in tags):
            raise MirrorError("Digest artifact tags and internal content tags cannot be caller aliases")
    if set(value["immutableTags"]) & set(value["mutableTags"]):
        raise MirrorError("Immutable and mutable tags must be disjoint")
    return value


def report_directory(request, requested=None):
    if requested:
        directory = Path(requested).absolute()
        directory.mkdir(parents=True, exist_ok=True)
    else:
        base = Path(os.environ.get("RUNNER_TEMP", tempfile.gettempdir())) / "mirror-container-image"
        base.mkdir(parents=True, exist_ok=True)
        directory = Path(tempfile.mkdtemp(prefix="image-", dir=base))
    content = canonical(request)
    request_path = directory / "request.json"
    if request_path.exists():
        if request_path.read_bytes() != content:
            raise MirrorError("Refusing to replace an immutable request in an existing report directory")
    else:
        with request_path.open("xb") as output:
            output.write(content)
        request_path.chmod(0o444)
    checksum = hashlib.sha256(content).hexdigest()
    checksum_path = directory / "request.sha256"
    if checksum_path.exists() and checksum_path.read_text().strip() != checksum:
        raise MirrorError("Archived request checksum was changed")
    if not checksum_path.exists():
        checksum_path.write_text(checksum + "\n")
        checksum_path.chmod(0o444)
    outputs = {"request-path": str(request_path), "receipt-path": str(directory / "receipt.json"),
               "report-dir": str(directory), "request-sha256": checksum,
               "digest": request["sourceRef"].split("@", 1)[1],
               "mirror-ref": request["targetRepository"] + "@" + request["sourceRef"].split("@", 1)[1]}
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            for name, value in outputs.items():
                output.write(f"{name}={value}\n")
    return directory, checksum


def client(executable, path, *, anonymous=False, http_localhost=False):
    write_json(path, {"version": 1, "incDockerCred": not anonymous,
                      "incDockerCert": not anonymous, "hosts": {}})
    path.chmod(0o600)
    return Registry(executable, path, anonymous=anonymous, http_localhost=http_localhost)


def check_immutable(client_, repository, tags, digest):
    for tag in tags:
        existing = client_.optional_digest(f"{repository}:{tag}")
        if existing is not None and existing != digest:
            raise MirrorError(f"Immutable tag conflict: {repository}:{tag} already points to {existing}")


def source_graph(client_, repository, digest, directory, attempt):
    graph = Graph(client_, repository, digest).discover()
    write_json(directory / f"source-graph-{attempt}.json", graph.inventory())
    return graph


def mirror_graph(publisher, anonymous, request, directory):
    source, digest = request["sourceRef"].split("@")
    target = request["targetRepository"]
    content_tag = "mirror-" + digest.replace(":", "-", 1)
    content_ref = f"{target}:{content_tag}"
    check_immutable(publisher, target, request["immutableTags"], digest)
    check_immutable(publisher, target, [content_tag], digest)
    for attempt in range(1, 4):
        graph = source_graph(publisher, source, digest, directory, attempt)
        if source != target:
            publisher.call("image", "copy", "--force-recursive", "--referrers", "--digest-tags",
                           request["sourceRef"], content_ref)
        else:
            publisher.call("image", "copy", request["sourceRef"], content_ref)
        target_graph = Graph(anonymous, target, digest).discover()
        try:
            graph.verify_target(target_graph)
        except MirrorError:
            # A moving legacy tag can invalidate the pre-copy inventory. Retry
            # only after a complete source re-read proves that it changed;
            # unchanged-source verification errors retain their original failure.
            if Graph(publisher, source, digest).discover().inventory() == graph.inventory():
                raise
            continue
        updated = Graph(publisher, source, digest).discover()
        if updated.inventory() == graph.inventory():
            write_json(directory / "target-graph.json", target_graph.inventory())
            return graph, content_tag
    raise MirrorError("Source artifact inventory changed during all three mirror attempts; retry the same request")


def verify_aliases(anonymous, request, digest):
    for tag in request["immutableTags"] + request["mutableTags"]:
        if anonymous.digest(f"{request['targetRepository']}:{tag}") != digest:
            raise MirrorError(f"Anonymous tag readback mismatch: {tag}")


def publish_aliases(publisher, anonymous, request, digest):
    target = request["targetRepository"]
    # Registry tag writes are not a cross-registry transaction. ACR immutable-tag policy
    # and a single workflow owner are required to close cross-repository writer races.
    check_immutable(publisher, target, request["immutableTags"], digest)
    for tag in request["immutableTags"] + request["mutableTags"]:
        current = publisher.optional_digest(f"{target}:{tag}")
        if tag in request["immutableTags"] and current not in (None, digest):
            raise MirrorError(f"Immutable tag changed during verification: {tag}")
        if current != digest:
            publisher.call("image", "copy", f"{target}@{digest}", f"{target}:{tag}")
    verify_aliases(anonymous, request, digest)


def receipt(request, request_sha, graph, content_tag):
    source_digest = request["sourceRef"].split("@", 1)[1]
    return {"schemaVersion": 1, "source": request["sourceRef"],
            "target": request["targetRepository"] + "@" + source_digest, "digest": source_digest,
            "requestSHA256": request_sha, "platforms": sorted(graph.platforms),
            "tags": request["immutableTags"] + request["mutableTags"], "contentTag": content_tag,
            "artifacts": {"referrers": graph.referrers, "legacyTags": graph.legacy_tags},
            "verification": {"rootDigestMatch": True, "manifestGraphMatch": True,
                             "anonymousBlobCount": len(graph.blobs), "manifestCount": len(graph.manifests),
                             "referrerCoverage": "complete", "anonymousPull": True, "tagReadback": True},
            "publisher": {"repository": os.environ.get("GITHUB_REPOSITORY"),
                          "runId": os.environ.get("GITHUB_RUN_ID"),
                          "runAttempt": os.environ.get("GITHUB_RUN_ATTEMPT"),
                          "workflowRevision": os.environ.get("GITHUB_WORKFLOW_SHA")},
            "tool": "regctl-v0.11.6", "verifiedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "verified": True, "status": "complete"}


def execute(args, request, directory, request_sha):
    source, digest = request["sourceRef"].split("@")
    # Keep credentials outside the artifact tree, including when the runner is killed
    # before Python can run its cleanup handlers.
    with tempfile.TemporaryDirectory(prefix="mirror-registry-auth-", dir=os.environ.get("RUNNER_TEMP")) as credentials:
        publisher = client(args.regctl, Path(credentials) / "publisher.json", http_localhost=args.test_http_localhost)
        anonymous = client(args.regctl, Path(credentials) / "anonymous.json", anonymous=True,
                           http_localhost=args.test_http_localhost)
        if args.command == "run":
            publisher.login(request["targetRepository"].split("/", 1)[0])
            graph, content_tag = mirror_graph(publisher, anonymous, request, directory)
            publish_aliases(publisher, anonymous, request, digest)
        else:
            graph = source_graph(publisher, source, digest, directory, 1)
            target = Graph(anonymous, request["targetRepository"], digest).discover()
            graph.verify_target(target)
            write_json(directory / "target-graph.json", target.inventory())
            verify_aliases(anonymous, request, digest)
            content_tag = None
        return receipt(request, request_sha, graph, content_tag)


def parser():
    root = argparse.ArgumentParser(description=__doc__)
    commands = root.add_subparsers(dest="command", required=True)
    prepare = commands.add_parser("prepare", help="Persist a request before installing tools or writing to a registry")
    prepare.add_argument("--source-ref", required=True)
    prepare.add_argument("--target-repository", required=True)
    prepare.add_argument("--immutable-tags-json", required=True)
    prepare.add_argument("--mutable-tags-json", default="[]")
    prepare.add_argument("--report-dir")
    for name in ("run", "verify"):
        command = commands.add_parser(name, help="Mirror and verify" if name == "run" else "Read-only anonymous verification")
        command.add_argument("--request-file", required=True)
        command.add_argument("--request-sha256")
        command.add_argument("--report-dir")
        command.add_argument("--regctl", required=True)
        command.add_argument("--test-http-localhost", action="store_true", help=argparse.SUPPRESS)
    return root


def main():
    args = parser().parse_args()
    directory = None
    try:
        if args.command == "prepare":
            request = {"schemaVersion": 1, "sourceRef": args.source_ref,
                       "targetRepository": args.target_repository,
                       "immutableTags": json.loads(args.immutable_tags_json),
                       "mutableTags": json.loads(args.mutable_tags_json)}
        else:
            raw = Path(args.request_file).read_bytes()
            if args.request_sha256 and hashlib.sha256(raw).hexdigest() != args.request_sha256:
                raise MirrorError("Request checksum does not match the pre-publication request")
            request = json.loads(raw)
        validate_request(request)
        directory, request_sha = report_directory(request, args.report_dir)
        if args.command == "prepare":
            print(f"Saved immutable request: {directory / 'request.json'}")
            return 0
        result = execute(args, request, directory, request_sha)
        write_json(directory / "receipt.json", result)
        print(f"Verified mirror: {result['target']}; receipt: {directory / 'receipt.json'}")
        return 0
    except (MirrorError, ValueError, OSError, subprocess.SubprocessError) as error:
        message = redact(str(error))
        if directory:
            write_json(directory / "receipt.json", {"schemaVersion": 1, "source": request["sourceRef"],
                       "target": request["targetRepository"] + "@" + request["sourceRef"].split("@", 1)[1],
                       "digest": request["sourceRef"].split("@", 1)[1], "requestSHA256": request_sha,
                       "verified": False, "status": "failed", "error": message})
        print(message, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
