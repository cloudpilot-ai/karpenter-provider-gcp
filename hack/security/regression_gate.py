#!/usr/bin/env python3
"""Fail a pull request only for blocking findings it newly introduces.

Compares a baseline scanner report against a candidate report produced by the
same pinned scanner, then exits non-zero when the candidate has a blocking
finding whose identity is not already blocking in the baseline.

Identity units differ per scanner because native output differs:

* zizmor: workflow path, audit rule, primary symbolic YAML route and whether the
  location is a whole feature or a subfeature. Concrete offsets and subfeature
  text are excluded, so line and whitespace edits stay stable. Sequence indices
  are part of the route, so inserting or reordering steps, renaming jobs and
  moving files can conservatively re-report inherited findings.
* govulncheck: advisory, module, package and reached symbol. Caller frames are
  folded together: a reached vulnerable symbol is one exposure however many call
  paths reach it.
* grype: advisory, package type and name, package location and the scan context
  (image platform). Package versions are excluded, so an unchanged exposure does
  not reappear when a vulnerable package changes version.

Exit codes: 0 no new blocking findings, 1 new blocking findings found, 2 report
or comparison error. An error is never reported as success.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterator, Sequence

EXIT_CLEAN = 0
EXIT_REGRESSION = 1
EXIT_ERROR = 2

BLOCKING_SEVERITIES = frozenset({"high", "critical"})
ZIZMOR_SEVERITIES = frozenset({"informational", "low", "medium", "high", "unknown"})
GOVULNCHECK_MESSAGE_KEYS = frozenset({"config", "progress", "SBOM", "osv", "finding"})
GOVULNCHECK_SCAN_LEVELS = frozenset({"module", "package", "symbol"})
GOVULNCHECK_SCAN_MODES = frozenset({"source", "binary", "extract"})
# System package types: the only finding types the image policy blocks on.
OS_PACKAGE_TYPES = frozenset({"deb", "rpm", "apk", "alpm", "portage"})


class ReportError(Exception):
    """A report or comparison input cannot be interpreted safely."""


@dataclass(frozen=True)
class Finding:
    identity: tuple[str, ...]
    blocking: bool
    severity: str
    location: str
    detail: str


@dataclass
class ScanReport:
    scanner: str
    path: Path
    sha256: str
    metadata: dict[str, str] = field(default_factory=dict)
    findings: list[Finding] = field(default_factory=list)
    duplicates: int = 0


@dataclass
class Comparison:
    new_blocking: list[Finding]
    resolved_blocking: list[Finding]
    baseline_blocking: int
    candidate_blocking: int
    report_only: Counter


def read_report(path: Path) -> tuple[str, str]:
    try:
        payload = path.read_bytes()
    except OSError as exc:
        raise ReportError(f"cannot read report {path}: {exc}") from exc
    if not payload.strip():
        raise ReportError(f"report {path} is empty")
    try:
        text = payload.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ReportError(f"report {path} is not UTF-8: {exc}") from exc
    return text, hashlib.sha256(payload).hexdigest()


def parse_json_document(text: str, path: Path) -> Any:
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise ReportError(f"report {path} is not valid JSON: {exc}") from exc


def iter_json_stream(text: str, path: Path) -> Iterator[Any]:
    """Yield the objects of a concatenated JSON object stream."""
    decoder = json.JSONDecoder()
    index = 0
    end = len(text)
    while True:
        while index < end and text[index].isspace():
            index += 1
        if index >= end:
            return
        try:
            value, index = decoder.raw_decode(text, index)
        except json.JSONDecodeError as exc:
            raise ReportError(f"report {path} is not a valid JSON object stream: {exc}") from exc
        yield value


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ReportError(message)


def require_mapping(value: Any, context: str) -> dict[str, Any]:
    require(
        isinstance(value, dict),
        f"{context} must be a JSON object, got {type(value).__name__}",
    )
    return value


def require_list(value: Any, context: str) -> list[Any]:
    require(isinstance(value, list), f"{context} must be a JSON array, got {type(value).__name__}")
    return value


def require_str(value: Any, context: str) -> str:
    require(isinstance(value, str) and value != "", f"{context} must be a non-empty string")
    return value


def require_single_variant(value: Any, context: str) -> tuple[str, Any]:
    mapping = require_mapping(value, context)
    require(
        len(mapping) == 1,
        f"{context} must have exactly one variant, got {sorted(mapping)}",
    )
    return next(iter(mapping.items()))


# --- zizmor -----------------------------------------------------------------


def zizmor_input_path(key: Any, path: Path) -> str:
    kind, value = require_single_variant(key, f"report {path}: symbolic key")
    if kind != "Local":
        raise ReportError(
            f"report {path}: unsupported zizmor input kind {kind!r}; "
            "paired scans must audit local files of the checkout"
        )
    local = require_mapping(value, f"report {path}: local symbolic key")
    verbatim = require_str(local.get("verbatim_path"), f"report {path}: verbatim_path")
    require(
        not verbatim.startswith("/"),
        f"report {path}: zizmor input path {verbatim!r} is absolute; scan relative to the checkout root",
    )
    normalized = str(Path(verbatim))
    require(
        not normalized.startswith(".."),
        f"report {path}: zizmor input path {verbatim!r} leaves the checkout root",
    )
    return normalized


def zizmor_route(route: Any, path: Path) -> str:
    mapping = require_mapping(route, f"report {path}: symbolic route")
    components = require_list(mapping.get("route"), f"report {path}: symbolic route.route")
    joined = ""
    for component in components:
        name, value = require_single_variant(component, f"report {path}: route component")
        if name == "Key":
            key = require_str(value, f"report {path}: route key")
            joined = f"{joined}.{key}" if joined else key
        elif name == "Index":
            require(isinstance(value, int), f"report {path}: route index must be an integer")
            joined += f"[{value}]"
        else:
            raise ReportError(f"report {path}: unsupported route component {name!r}")
    return joined or "<document>"


def zizmor_feature_kind(feature_kind: Any, path: Path) -> str:
    if feature_kind == "Normal":
        return "feature"
    name, value = require_single_variant(feature_kind, f"report {path}: feature kind")
    if name != "Subfeature":
        raise ReportError(f"report {path}: unsupported feature kind {name!r}")
    subfeature = require_mapping(value, f"report {path}: subfeature")
    require(
        isinstance(subfeature.get("after"), int),
        f"report {path}: subfeature.after must be an integer",
    )
    return "subfeature"


def load_zizmor(path: Path, context: str) -> ScanReport:
    text, digest = read_report(path)
    document = parse_json_document(text, path)
    findings = require_list(document, f"report {path}: zizmor --format json output")
    report = ScanReport(scanner="zizmor", path=path, sha256=digest)
    for index, raw in enumerate(findings):
        finding = require_mapping(raw, f"report {path}: finding {index}")
        ident = require_str(finding.get("ident"), f"report {path}: finding {index} ident")
        description = require_str(finding.get("desc"), f"report {path}: finding {index} desc")
        determinations = require_mapping(
            finding.get("determinations"), f"report {path}: finding {index} determinations"
        )
        severity = require_str(
            determinations.get("severity"), f"report {path}: finding {index} severity"
        )
        require(
            severity.lower() in ZIZMOR_SEVERITIES,
            f"report {path}: finding {index} has unknown severity {severity!r}",
        )
        ignored = finding.get("ignored")
        require(
            isinstance(ignored, bool),
            f"report {path}: finding {index} ignored must be a boolean",
        )
        locations = require_list(
            finding.get("locations"), f"report {path}: finding {index} locations"
        )
        primary = []
        for location in locations:
            location = require_mapping(location, f"report {path}: finding {index} location")
            symbolic = require_mapping(
                location.get("symbolic"), f"report {path}: finding {index} symbolic location"
            )
            if symbolic.get("kind") == "Primary":
                primary.append(location)
        require(
            len(primary) == 1,
            f"report {path}: finding {index} has {len(primary)} primary symbolic "
            "locations, expected exactly one",
        )
        symbolic = require_mapping(
            primary[0].get("symbolic"), f"report {path}: finding {index} symbolic location"
        )
        source = zizmor_input_path(symbolic.get("key"), path)
        route = zizmor_route(symbolic.get("route"), path)
        feature_kind = zizmor_feature_kind(symbolic.get("feature_kind"), path)
        annotation = symbolic.get("annotation")
        row = zizmor_start_row(primary[0], path, index)
        report.findings.append(
            Finding(
                identity=("zizmor", source, ident, route, feature_kind),
                blocking=(not ignored) and severity.lower() in BLOCKING_SEVERITIES,
                severity=severity,
                location=f"{source}:{route}" + (f" (line {row})" if row else ""),
                detail="; ".join(
                    part
                    for part in (
                        description,
                        annotation if isinstance(annotation, str) else "",
                        "suppressed by inline ignore" if ignored else "",
                    )
                    if part
                ),
            )
        )
    return report


def zizmor_start_row(location: dict[str, Any], path: Path, index: int) -> int | None:
    concrete = require_mapping(location.get("concrete"), f"report {path}: finding {index} concrete")
    source = require_mapping(
        concrete.get("location"), f"report {path}: finding {index} concrete location"
    )
    start = require_mapping(source.get("start_point"), f"report {path}: finding {index} start point")
    row = start.get("row")
    require(isinstance(row, int), f"report {path}: finding {index} start_point.row must be an integer")
    return row


# --- govulncheck ------------------------------------------------------------


def load_govulncheck(path: Path, context: str) -> ScanReport:
    text, digest = read_report(path)
    messages = [
        require_mapping(message, f"report {path}: message")
        for message in iter_json_stream(text, path)
    ]
    require(messages, f"report {path}: govulncheck JSON stream is empty")
    for index, message in enumerate(messages):
        unknown = set(message) - GOVULNCHECK_MESSAGE_KEYS
        require(not unknown, f"report {path}: message {index} has unknown keys {sorted(unknown)}")
    configs = [message["config"] for message in messages if "config" in message]
    require(len(configs) == 1, f"report {path}: expected exactly one govulncheck config message")
    config = require_mapping(configs[0], f"report {path}: govulncheck config")
    require(
        config.get("scanner_name") == "govulncheck",
        f"report {path}: scanner_name is {config.get('scanner_name')!r}, expected 'govulncheck'",
    )
    scan_level = require_str(config.get("scan_level"), f"report {path}: scan_level")
    require(
        scan_level in GOVULNCHECK_SCAN_LEVELS, f"report {path}: unknown scan_level {scan_level!r}"
    )
    scan_mode = require_str(config.get("scan_mode"), f"report {path}: scan_mode")
    require(scan_mode in GOVULNCHECK_SCAN_MODES, f"report {path}: unknown scan_mode {scan_mode!r}")
    report = ScanReport(scanner="govulncheck", path=path, sha256=digest)
    report.metadata = {
        "scanner_version": require_str(
            config.get("scanner_version"), f"report {path}: scanner_version"
        ),
        "database": require_str(config.get("db"), f"report {path}: db"),
        "database_last_modified": str(config.get("db_last_modified", "")),
        "go_version": require_str(config.get("go_version"), f"report {path}: go_version"),
        "scan_level": scan_level,
        "scan_mode": scan_mode,
        "protocol_version": require_str(
            config.get("protocol_version"), f"report {path}: protocol_version"
        ),
    }

    # Folded by identity: caller paths to the same vulnerable symbol collapse, and
    # the exposure stays blocking if any duplicate reports a reached symbol.
    folded: dict[tuple[str, ...], Finding] = {}
    for index, message in enumerate(messages):
        if "finding" not in message:
            continue
        finding = require_mapping(message["finding"], f"report {path}: finding {index}")
        advisory = require_str(finding.get("osv"), f"report {path}: finding {index} osv")
        trace = require_list(finding.get("trace"), f"report {path}: finding {index} trace")
        require(trace, f"report {path}: finding {index} has an empty trace")
        entry = require_mapping(trace[0], f"report {path}: finding {index} entry frame")
        module = str(entry.get("module", ""))
        package = str(entry.get("package", ""))
        receiver = str(entry.get("receiver", ""))
        function = str(entry.get("function", ""))
        symbol = f"{receiver}.{function}" if function else ""
        identity = ("govulncheck", advisory, module, package, symbol)
        # A reached symbol is blocking. govulncheck carries no severity, and a
        # newly reachable vulnerability blocks even when severity is unknown.
        # Module- and package-level findings record import exposure only.
        blocking = entry.get("function") != "" and entry.get("function") is not None
        # The package path already carries the module path; fall back to the module
        # only for findings that are not attributed to a package.
        location = " ".join(part for part in (package or module, symbol) if part) or "<unknown>"
        fixed_version = finding.get("fixed_version")
        detail = f"{advisory} fixed in {fixed_version}" if fixed_version else advisory
        existing = folded.get(identity)
        if existing is None:
            folded[identity] = Finding(
                identity=identity,
                blocking=blocking,
                severity="unknown",
                location=location,
                detail=detail,
            )
            continue
        report.duplicates += 1
        if blocking and not existing.blocking:
            folded[identity] = Finding(
                identity=identity,
                blocking=True,
                severity="unknown",
                location=location,
                detail=detail,
            )
    report.findings = list(folded.values())
    return report


# --- grype ------------------------------------------------------------------


def grype_database_status(path: Path, descriptor: dict[str, Any]) -> dict[str, str]:
    database = require_mapping(descriptor.get("db"), f"report {path}: descriptor.db")
    status = require_mapping(database.get("status"), f"report {path}: descriptor.db.status")
    require(
        status.get("valid") is True,
        f"report {path}: grype reported an invalid vulnerability database",
    )
    return {
        "database_schema_version": require_str(
            status.get("schemaVersion"), f"report {path}: descriptor.db.status.schemaVersion"
        ),
        "database_built": require_str(
            status.get("built"), f"report {path}: descriptor.db.status.built"
        ),
    }


def grype_locations(artifact: dict[str, Any], path: Path, index: int) -> str:
    locations = require_list(
        artifact.get("locations") or [], f"report {path}: match {index} artifact.locations"
    )
    paths = set()
    for raw in locations:
        location = require_mapping(raw, f"report {path}: match {index} artifact.location")
        value = location.get("path") or location.get("accessPath") or ""
        if value:
            paths.add(str(value))
    return ",".join(sorted(paths)) or "<no location>"


def load_grype(path: Path, context: str) -> ScanReport:
    text, digest = read_report(path)
    document = require_mapping(parse_json_document(text, path), f"report {path}: grype JSON output")
    descriptor = require_mapping(document.get("descriptor"), f"report {path}: descriptor")
    require(
        descriptor.get("name") == "grype",
        f"report {path}: descriptor.name is {descriptor.get('name')!r}, expected 'grype'",
    )
    report = ScanReport(scanner="grype", path=path, sha256=digest)
    report.metadata = {
        "scanner_version": require_str(
            descriptor.get("version"), f"report {path}: descriptor.version"
        ),
        **grype_database_status(path, descriptor),
        "context": context,
    }
    distro = document.get("distro")
    if isinstance(distro, dict):
        report.metadata["distro"] = f"{distro.get('name', '')} {distro.get('version', '')}".strip()

    # Folded by identity: one advisory against one package location is one
    # exposure, and it stays blocking if any duplicate meets the gate policy.
    folded: dict[tuple[str, ...], Finding] = {}
    for index, raw in enumerate(require_list(document.get("matches"), f"report {path}: matches")):
        match = require_mapping(raw, f"report {path}: match {index}")
        vulnerability = require_mapping(
            match.get("vulnerability"), f"report {path}: match {index} vulnerability"
        )
        artifact = require_mapping(match.get("artifact"), f"report {path}: match {index} artifact")
        advisory = require_str(
            vulnerability.get("id"), f"report {path}: match {index} vulnerability.id"
        )
        severity = str(vulnerability.get("severity") or "Unknown")
        package_type = require_str(
            artifact.get("type"), f"report {path}: match {index} artifact.type"
        )
        package_name = require_str(
            artifact.get("name"), f"report {path}: match {index} artifact.name"
        )
        location = grype_locations(artifact, path, index)
        identity = ("grype", context, advisory, package_type, package_name, location)
        fix = require_mapping(vulnerability.get("fix") or {}, f"report {path}: match {index} fix")
        fix_versions = require_list(
            fix.get("versions") or [], f"report {path}: match {index} fix.versions"
        )
        fixable = fix.get("state") == "fixed" and any(
            isinstance(version, str) and version != "" for version in fix_versions
        )
        blocking = (
            package_type in OS_PACKAGE_TYPES
            and severity.lower() in BLOCKING_SEVERITIES
            and fixable
        )
        detail = f"{package_name} {artifact.get('version', '')}".strip()
        if fixable:
            detail += f" fixed in {', '.join(str(version) for version in fix_versions)}"
        existing = folded.get(identity)
        if existing is None:
            folded[identity] = Finding(
                identity=identity,
                blocking=blocking,
                severity=severity,
                location=f"{advisory} {package_type}/{package_name} at {location}",
                detail=detail,
            )
            continue
        report.duplicates += 1
        if blocking and not existing.blocking:
            folded[identity] = Finding(
                identity=identity,
                blocking=True,
                severity=severity,
                location=existing.location,
                detail=detail,
            )
    report.findings = list(folded.values())
    return report


LOADERS = {
    "zizmor": load_zizmor,
    "govulncheck": load_govulncheck,
    "grype": load_grype,
}


# --- comparison -------------------------------------------------------------


# Recorded for the reader but allowed to differ between revisions: the intended Go
# toolchain, the scanned image distro, and database timestamps that local
# govulncheck databases do not populate.
INFORMATIONAL_METADATA = frozenset(
    {"go_version", "distro", "database_last_modified", "context"}
)


def check_comparable(baseline: ScanReport, candidate: ScanReport) -> None:
    require(
        baseline.scanner == candidate.scanner,
        f"cannot compare {baseline.scanner} with {candidate.scanner}",
    )
    for key in sorted((set(baseline.metadata) | set(candidate.metadata)) - INFORMATIONAL_METADATA):
        require(
            baseline.metadata.get(key) == candidate.metadata.get(key),
            f"reports were produced with different {key}: "
            f"{baseline.metadata.get(key)!r} (baseline) vs "
            f"{candidate.metadata.get(key)!r} (candidate)",
        )


def compare(baseline: ScanReport, candidate: ScanReport) -> Comparison:
    baseline_blocking = Counter(
        finding.identity for finding in baseline.findings if finding.blocking
    )
    remaining = Counter(baseline_blocking)
    consumed: Counter = Counter()
    new_blocking: list[Finding] = []
    candidate_blocking = 0
    for finding in sorted(
        (finding for finding in candidate.findings if finding.blocking),
        key=lambda finding: finding.identity,
    ):
        candidate_blocking += 1
        if remaining[finding.identity] > 0:
            remaining[finding.identity] -= 1
            consumed[finding.identity] += 1
        else:
            new_blocking.append(finding)
    resolved: list[Finding] = []
    for finding in sorted(
        (finding for finding in baseline.findings if finding.blocking),
        key=lambda finding: finding.identity,
    ):
        identity = finding.identity
        unresolved = baseline_blocking[identity] - consumed[identity]
        if unresolved > 0:
            resolved.append(finding)
            consumed[identity] += 1
    return Comparison(
        new_blocking=new_blocking,
        resolved_blocking=resolved,
        baseline_blocking=sum(baseline_blocking.values()),
        candidate_blocking=candidate_blocking,
        report_only=Counter(
            finding.severity.lower() for finding in candidate.findings if not finding.blocking
        ),
    )


# --- reporting --------------------------------------------------------------


def render_markdown(
    scanner: str, baseline: ScanReport, candidate: ScanReport, comparison: Comparison
) -> str:
    outcome = "regression" if comparison.new_blocking else "clean"
    lines = [
        f"# Security regression gate: {scanner} ({outcome})",
        "",
        f"- Baseline report: `{baseline.path}` (sha256 `{baseline.sha256}`)",
        f"- Candidate report: `{candidate.path}` (sha256 `{candidate.sha256}`)",
        f"- Blocking findings: baseline {comparison.baseline_blocking}, "
        f"candidate {comparison.candidate_blocking}",
        f"- New blocking findings: {len(comparison.new_blocking)}",
        f"- Resolved blocking findings: {len(comparison.resolved_blocking)}",
        "- Report-only candidate findings: "
        + (
            ", ".join(
                f"{severity} {count}" for severity, count in sorted(comparison.report_only.items())
            )
            or "none"
        ),
        f"- Duplicate native results folded: baseline {baseline.duplicates}, "
        f"candidate {candidate.duplicates}",
    ]
    for key in sorted(set(baseline.metadata) | set(candidate.metadata)):
        baseline_value = baseline.metadata.get(key, "")
        candidate_value = candidate.metadata.get(key, "")
        if baseline_value == candidate_value:
            lines.append(f"- {key}: `{baseline_value or 'unset'}`")
        else:
            lines.append(
                f"- {key}: baseline `{baseline_value or 'unset'}`, "
                f"candidate `{candidate_value or 'unset'}`"
            )
    lines.append("")
    if comparison.new_blocking:
        lines.extend(["## New blocking findings", ""])
        for finding in comparison.new_blocking:
            lines.append(f"- [{finding.severity}] `{finding.location}`")
            lines.append(f"  - {finding.detail or 'no detail'}")
            lines.append(f"  - identity: `{' / '.join(finding.identity[1:])}`")
        lines.append("")
    if comparison.resolved_blocking:
        lines.extend(["## Resolved blocking findings", ""])
        for finding in comparison.resolved_blocking:
            lines.append(f"- [{finding.severity}] `{finding.location}`")
        lines.append("")
    return "\n".join(lines)


def render_json(
    scanner: str, baseline: ScanReport, candidate: ScanReport, comparison: Comparison
) -> dict[str, Any]:
    def payload(finding: Finding) -> dict[str, Any]:
        return {
            "identity": list(finding.identity),
            "severity": finding.severity,
            "location": finding.location,
            "detail": finding.detail,
        }

    def report_payload(report: ScanReport) -> dict[str, Any]:
        return {
            "path": str(report.path),
            "sha256": report.sha256,
            "metadata": report.metadata,
            "findings": len(report.findings),
            "duplicates_folded": report.duplicates,
        }

    return {
        "scanner": scanner,
        "result": "regression" if comparison.new_blocking else "clean",
        "baseline": report_payload(baseline),
        "candidate": report_payload(candidate),
        "blocking": {
            "baseline": comparison.baseline_blocking,
            "candidate": comparison.candidate_blocking,
            "new": len(comparison.new_blocking),
            "resolved": len(comparison.resolved_blocking),
        },
        "report_only": dict(sorted(comparison.report_only.items())),
        "new_blocking_findings": [payload(finding) for finding in comparison.new_blocking],
        "resolved_blocking_findings": [
            payload(finding) for finding in comparison.resolved_blocking
        ],
    }


def write_output(path: Path, content: str) -> None:
    try:
        path.write_text(content, encoding="utf-8")
    except OSError as exc:
        raise ReportError(f"cannot write {path}: {exc}") from exc


def parse_args(argv: Sequence[str] | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Compare baseline and candidate security scanner reports.",
        epilog="Exit codes: 0 clean, 1 new blocking findings, 2 report or comparison error.",
    )
    parser.add_argument("--scanner", required=True, choices=sorted(LOADERS))
    parser.add_argument("--baseline", required=True, type=Path)
    parser.add_argument("--candidate", required=True, type=Path)
    parser.add_argument(
        "--context",
        default="",
        help="scan context folded into grype identities, for example linux/amd64",
    )
    parser.add_argument("--summary-out", type=Path, help="write the markdown summary here")
    parser.add_argument("--json-out", type=Path, help="write the machine-readable summary here")
    args = parser.parse_args(argv)
    if args.context and args.scanner != "grype":
        parser.error("--context only applies to the grype scanner")
    return args


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        baseline = LOADERS[args.scanner](args.baseline, args.context)
        candidate = LOADERS[args.scanner](args.candidate, args.context)
        check_comparable(baseline, candidate)
        comparison = compare(baseline, candidate)
        summary = render_markdown(args.scanner, baseline, candidate, comparison)
        if args.summary_out:
            write_output(args.summary_out, summary)
        if args.json_out:
            document = render_json(args.scanner, baseline, candidate, comparison)
            write_output(
                args.json_out,
                json.dumps(document, indent=2, sort_keys=True) + "\n",
            )
    except ReportError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_ERROR
    print(summary)
    return EXIT_REGRESSION if comparison.new_blocking else EXIT_CLEAN


if __name__ == "__main__":
    sys.exit(main())
