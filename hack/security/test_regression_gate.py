#!/usr/bin/env python3
"""Unit tests for the security regression gate comparator.

Fixtures in ``testdata`` are verbatim native scanner output (see
``testdata/README.md``). Tests that need a scenario the pinned scanners cannot
produce on demand derive their candidate report from a fixture in memory and
rewrite only the fields under test, so the surrounding document keeps its native
shape.
"""

from __future__ import annotations

import contextlib
import copy
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Any, Sequence

import regression_gate

TESTDATA = Path(__file__).resolve().parent / "testdata"


def fixture(scanner: str, name: str) -> Path:
    return TESTDATA / scanner / name


def run(arguments: Sequence[str]) -> tuple[int, str, str]:
    stdout, stderr = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        code = regression_gate.main(list(arguments))
    return code, stdout.getvalue(), stderr.getvalue()


class TempReports:
    """Writes derived candidate reports into a temporary directory."""

    def __init__(self) -> None:
        self._directory = tempfile.TemporaryDirectory()
        self.root = Path(self._directory.name)

    def cleanup(self) -> None:
        self._directory.cleanup()

    def load(self, scanner: str, name: str) -> Any:
        return json.loads(fixture(scanner, name).read_text())

    def write(self, name: str, document: Any) -> Path:
        path = self.root / name
        path.write_text(json.dumps(document))
        return path

    def write_text(self, name: str, text: str) -> Path:
        path = self.root / name
        path.write_text(text)
        return path

    def write_stream(self, name: str, messages: list[dict[str, Any]]) -> Path:
        return self.write_text(name, "\n".join(json.dumps(message) for message in messages) + "\n")


class GateTestCase(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = TempReports()
        self.addCleanup(self.temp.cleanup)

    def compare(self, scanner: str, baseline: Path, candidate: Path, *extra: str) -> tuple[int, str, str]:
        return run(
            [
                "--scanner",
                scanner,
                "--baseline",
                str(baseline),
                "--candidate",
                str(candidate),
                *extra,
            ]
        )


class ZizmorGateTest(GateTestCase):
    def test_inherited_findings_pass(self) -> None:
        code, stdout, _ = self.compare(
            "zizmor", fixture("zizmor", "baseline.json"), fixture("zizmor", "baseline.json")
        )
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)

    def test_line_and_whitespace_shifts_do_not_add_findings(self) -> None:
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            fixture("zizmor", "candidate-line-shift.json"),
        )
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)

    def test_added_high_finding_blocks(self) -> None:
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            fixture("zizmor", "candidate-added-step.json"),
        )
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)
        self.assertIn("jobs.build.steps[2].uses", stdout)

    def test_added_finding_blocks_even_when_another_disappears(self) -> None:
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            fixture("zizmor", "candidate-different-site.json"),
        )
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)
        self.assertIn("Resolved blocking findings: 1", stdout)

    def test_low_and_medium_findings_are_report_only(self) -> None:
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            fixture("zizmor", "candidate-report-only.json"),
        )
        self.assertEqual(code, 0)
        self.assertIn("Report-only candidate findings: low 1, medium 1", stdout)

    def test_threshold_escalation_blocks(self) -> None:
        baseline = self.temp.load("zizmor", "baseline.json")
        for finding in baseline:
            if finding["ident"] == "unpinned-uses":
                finding["determinations"]["severity"] = "Medium"
        code, stdout, _ = self.compare(
            "zizmor",
            self.temp.write("baseline-medium.json", baseline),
            fixture("zizmor", "baseline.json"),
        )
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)

    def test_inline_suppressed_finding_is_report_only(self) -> None:
        candidate = self.temp.load("zizmor", "candidate-added-step.json")
        suppressed = 0
        for finding in candidate:
            if finding["ident"] != "unpinned-uses":
                continue
            for location in finding["locations"]:
                symbolic = location["symbolic"]
                if symbolic["kind"] == "Primary" and symbolic["route"]["route"][-2] == {"Index": 2}:
                    finding["ignored"] = True
                    suppressed += 1
        self.assertEqual(suppressed, 1)
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            self.temp.write("candidate-suppressed.json", candidate),
        )
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)

    def test_repeated_identities_preserve_multiplicity(self) -> None:
        finding = next(
            item
            for item in self.temp.load("zizmor", "baseline.json")
            if item["determinations"]["severity"] == "High"
        )
        baseline = [copy.deepcopy(finding), copy.deepcopy(finding)]
        # Different subfeatures can share a symbolic route without sharing text.
        baseline[1]["desc"] += " (another occurrence)"
        candidate = copy.deepcopy(baseline) + [copy.deepcopy(finding)]
        code, stdout, _ = self.compare(
            "zizmor",
            self.temp.write("baseline-duplicated.json", baseline),
            self.temp.write("candidate-triplicated.json", candidate),
        )
        self.assertEqual(code, 1)
        self.assertIn("Blocking findings: baseline 2, candidate 3", stdout)
        self.assertIn("New blocking findings: 1", stdout)

    def test_absolute_input_path_fails(self) -> None:
        candidate = self.temp.load("zizmor", "baseline.json")
        for location in candidate[0]["locations"]:
            location["symbolic"]["key"]["Local"]["verbatim_path"] = "/home/runner/work/ci.yaml"
        code, _, stderr = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            self.temp.write("candidate-absolute.json", candidate),
        )
        self.assertEqual(code, 2)
        self.assertIn("absolute", stderr)

    def test_object_report_instead_of_array_fails(self) -> None:
        code, _, stderr = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            self.temp.write("candidate-object.json", {"version": "1", "audits": {}}),
        )
        self.assertEqual(code, 2)
        self.assertIn("must be a JSON array", stderr)


class GovulncheckGateTest(GateTestCase):
    def test_newly_reachable_unknown_severity_blocks(self) -> None:
        code, stdout, _ = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-clean.json"),
            fixture("govulncheck", "baseline-reached.json"),
        )
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)
        self.assertIn("GO-2026-4503", stdout)
        self.assertIn("*Point.MultiScalarMult", stdout)
        # Module- and package-level findings record import exposure only.
        self.assertIn("Report-only candidate findings: unknown 2", stdout)

    def test_unchanged_exposure_survives_caller_shift(self) -> None:
        code, stdout, _ = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-reached.json"),
            fixture("govulncheck", "candidate-reached-wrapped.json"),
        )
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)
        self.assertIn("Blocking findings: baseline 1, candidate 1", stdout)

    def test_package_level_findings_are_report_only(self) -> None:
        code, stdout, _ = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-clean-package-level.json"),
            fixture("govulncheck", "candidate-package-level.json"),
        )
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)
        self.assertIn("Report-only candidate findings: unknown 2", stdout)

    def test_scan_level_mismatch_fails(self) -> None:
        code, _, stderr = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-reached.json"),
            fixture("govulncheck", "candidate-package-level.json"),
        )
        self.assertEqual(code, 2)
        self.assertIn("different scan_level", stderr)

    def test_database_mismatch_fails(self) -> None:
        candidate = self._messages("candidate-reached-wrapped.json")
        candidate[0]["config"]["db"] = "https://vuln.go.dev"
        code, _, stderr = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-reached.json"),
            self.temp.write_stream("candidate-db.json", candidate),
        )
        self.assertEqual(code, 2)
        self.assertIn("different database", stderr)

    def test_unknown_message_key_fails(self) -> None:
        candidate = self._messages("candidate-reached-wrapped.json")
        candidate.append({"unexpected": {"value": True}})
        code, _, stderr = self.compare(
            "govulncheck",
            fixture("govulncheck", "baseline-reached.json"),
            self.temp.write_stream("candidate-unknown.json", candidate),
        )
        self.assertEqual(code, 2)
        self.assertIn("unknown keys", stderr)

    def test_truncated_stream_fails(self) -> None:
        path = self.temp.write_text("candidate-truncated.json", '{"config": {"scanner_name": ')
        code, _, stderr = self.compare(
            "govulncheck", fixture("govulncheck", "baseline-reached.json"), path
        )
        self.assertEqual(code, 2)
        self.assertIn("object stream", stderr)

    def _messages(self, name: str) -> list[dict[str, Any]]:
        path = fixture("govulncheck", name)
        return list(regression_gate.iter_json_stream(path.read_text(), path))


class GrypeGateTest(GateTestCase):
    def compare_grype(self, candidate: Path) -> tuple[int, str, str]:
        return self.compare(
            "grype",
            fixture("grype", "baseline.json"),
            candidate,
            "--context",
            "linux/amd64",
        )

    def matches(self) -> list[dict[str, Any]]:
        return self.temp.load("grype", "baseline.json")

    def blocking(self, document: dict[str, Any]) -> dict[str, Any]:
        return next(
            match
            for match in document["matches"]
            if match["vulnerability"]["severity"] == "High"
            and match["vulnerability"]["fix"]["state"] == "fixed"
        )

    def test_inherited_findings_pass(self) -> None:
        code, stdout, _ = self.compare_grype(fixture("grype", "baseline.json"))
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)
        self.assertIn("database_schema_version", stdout)

    def test_package_version_change_keeps_exposure(self) -> None:
        candidate = self.matches()
        self.blocking(candidate)["artifact"]["version"] = "1.2.4_git20230717-r7"
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-version.json", candidate))
        self.assertEqual(code, 0)
        self.assertIn("New blocking findings: 0", stdout)

    def test_new_fixable_high_os_finding_blocks(self) -> None:
        candidate = self.matches()
        added = copy.deepcopy(self.blocking(candidate))
        added["vulnerability"]["id"] = "CVE-2026-99999"
        added["artifact"]["name"] = "openssl"
        candidate["matches"].append(added)
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-added.json", candidate))
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)
        self.assertIn("CVE-2026-99999", stdout)

    def test_threshold_escalation_blocks(self) -> None:
        candidate = self.matches()
        medium = next(
            match
            for match in candidate["matches"]
            if match["vulnerability"]["severity"] == "Medium"
            and match["vulnerability"]["fix"]["state"] == "fixed"
        )
        medium["vulnerability"]["severity"] = "High"
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-escalated.json", candidate))
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)

    def test_fix_becoming_available_blocks(self) -> None:
        candidate = self.matches()
        unfixed = next(
            match
            for match in candidate["matches"]
            if match["vulnerability"]["fix"]["state"] == "unknown"
        )
        unfixed["vulnerability"]["severity"] = "High"
        unfixed["vulnerability"]["fix"] = {"versions": ["9.9.9"], "state": "fixed"}
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-fixed.json", candidate))
        self.assertEqual(code, 1)
        self.assertIn("New blocking findings: 1", stdout)

    def test_unfixed_high_os_finding_is_report_only(self) -> None:
        candidate = self.matches()
        self.blocking(candidate)["vulnerability"]["fix"] = {"versions": [], "state": "unknown"}
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-unfixed.json", candidate))
        self.assertEqual(code, 0)
        self.assertIn("Report-only candidate findings: high 1", stdout)

    def test_high_fixable_library_finding_is_report_only(self) -> None:
        candidate = self.matches()
        library = self.blocking(candidate)
        library["artifact"]["type"] = "go-module"
        library["artifact"]["locations"] = [{"path": "/usr/local/bin/controller"}]
        code, stdout, _ = self.compare_grype(self.temp.write("candidate-library.json", candidate))
        self.assertEqual(code, 0)
        self.assertIn("Report-only candidate findings: high 1", stdout)

    def test_database_mismatch_fails(self) -> None:
        candidate = self.matches()
        candidate["descriptor"]["db"]["status"]["built"] = "2020-01-01T00:00:00Z"
        code, _, stderr = self.compare_grype(self.temp.write("candidate-db.json", candidate))
        self.assertEqual(code, 2)
        self.assertIn("different database_built", stderr)

    def test_missing_matches_fails(self) -> None:
        candidate = self.matches()
        del candidate["matches"]
        code, _, stderr = self.compare_grype(self.temp.write("candidate-nomatches.json", candidate))
        self.assertEqual(code, 2)
        self.assertIn("matches must be a JSON array", stderr)

    def test_image_without_findings_passes(self) -> None:
        code, stdout, _ = self.compare(
            "grype",
            fixture("grype", "controller-image.json"),
            fixture("grype", "controller-image.json"),
            "--context",
            "linux/arm64",
        )
        self.assertEqual(code, 0)
        self.assertIn("Blocking findings: baseline 0, candidate 0", stdout)


class CommandLineTest(GateTestCase):
    def test_missing_report_fails(self) -> None:
        code, _, stderr = self.compare(
            "zizmor", fixture("zizmor", "baseline.json"), self.temp.root / "absent.json"
        )
        self.assertEqual(code, 2)
        self.assertIn("cannot read report", stderr)

    def test_empty_report_fails(self) -> None:
        code, _, stderr = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            self.temp.write_text("empty.json", ""),
        )
        self.assertEqual(code, 2)
        self.assertIn("is empty", stderr)

    def test_context_is_rejected_for_other_scanners(self) -> None:
        with self.assertRaises(SystemExit) as raised:
            run(
                [
                    "--scanner",
                    "zizmor",
                    "--baseline",
                    str(fixture("zizmor", "baseline.json")),
                    "--candidate",
                    str(fixture("zizmor", "baseline.json")),
                    "--context",
                    "linux/amd64",
                ]
            )
        self.assertEqual(raised.exception.code, 2)

    def test_reports_are_written_for_regressions(self) -> None:
        summary = self.temp.root / "summary.md"
        payload = self.temp.root / "summary.json"
        code, stdout, _ = self.compare(
            "zizmor",
            fixture("zizmor", "baseline.json"),
            fixture("zizmor", "candidate-added-step.json"),
            "--summary-out",
            str(summary),
            "--json-out",
            str(payload),
        )
        self.assertEqual(code, 1)
        document = json.loads(payload.read_text())
        self.assertEqual(document["result"], "regression")
        self.assertEqual(document["blocking"]["new"], 1)
        self.assertEqual(len(document["new_blocking_findings"]), 1)
        self.assertIn("## New blocking findings", summary.read_text())
        self.assertIn("## New blocking findings", stdout)


if __name__ == "__main__":
    sys.exit(unittest.main())
