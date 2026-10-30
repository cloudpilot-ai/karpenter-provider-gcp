#!/usr/bin/env python3
"""Project private Ginkgo JSON into text-free native JSON/JUnit and bounded counts."""
import argparse
import json
import math
import os
import re
import stat
import xml.etree.ElementTree as ET
from pathlib import Path

LABELS = {'suite:' + name for name in (
    'repair expiration nested-virtualization gc kubelet_config customtype interruption '
    'consolidation local-ssd-extended local-ssd storage channel-image-selection drift gpu '
    'provisioning hugepages networking confidential').split()}
STATES = {'passed', 'failed', 'timedout', 'panicked', 'interrupted', 'aborted', 'skipped', 'pending'}
NODES = {'It', 'BeforeSuite', 'AfterSuite', 'SynchronizedBeforeSuite', 'SynchronizedAfterSuite',
         'ReportBeforeSuite', 'ReportAfterSuite', 'DeferCleanup (Suite)'}
MAX_DURATION = 86400 * 10**9  # Ginkgo time.Duration is JSON nanoseconds, not seconds.


def read_json(path, limit):
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= limit:
            raise ValueError('invalid evidence file')
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError('oversized evidence')
    return json.loads(data)


def duration(value):
    if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= MAX_DURATION:
        raise ValueError('invalid native duration')
    return int(value)


def project(private, public, sha, preset):
    if not re.fullmatch('[a-f0-9]{40}', sha) or preset not in ('standard', 'gpu', 'full'):
        raise ValueError('invalid independent invocation')
    runtime = read_json(private / 'runtime.json', 1024)
    if (not isinstance(runtime, dict) or set(runtime) != {'source_sha', 'test_exit', 'cleanup_ok'}
            or runtime['source_sha'] != sha or type(runtime['cleanup_ok']) is not bool
            or not (runtime['test_exit'] is None or
                    type(runtime['test_exit']) is int and 0 <= runtime['test_exit'] <= 255)):
        raise ValueError('invalid runtime evidence')
    reports = read_json(private / 'results.json', 32 << 20)
    if not isinstance(reports, list) or not 1 <= len(reports) <= 16:
        raise ValueError('invalid native suites')
    counts = dict(executed=0, passed=0, failed=0, skipped=0)
    safe, total, spec_count, suites_ok = [], 0, 0, True
    xml = ET.Element('testsuites')
    for index, report in enumerate(reports, 1):
        if not isinstance(report, dict) or type(report.get('SuiteSucceeded')) is not bool:
            raise ValueError('invalid native suite')
        elapsed = duration(report.get('RunTime'))
        total += elapsed
        if total > MAX_DURATION:
            raise ValueError('excessive total duration')
        suites_ok = suites_ok and report['SuiteSucceeded']
        specs = report.get('SpecReports')
        if not isinstance(specs, list):
            raise ValueError('missing native specs')
        safe_specs = []
        suite_xml = ET.SubElement(xml, 'testsuite', name=f'suite-{index}', time=str(elapsed / 1e9))
        for spec in specs:
            spec_count += 1
            if spec_count > 10000 or not isinstance(spec, dict) or spec.get('State') not in STATES:
                raise ValueError('invalid native spec')
            if spec.get('LeafNodeType') not in NODES:
                raise ValueError('invalid native node type')
            state = spec['State']
            elapsed_spec = duration(spec.get('RunTime'))
            if spec.get('LeafNodeType') != 'It':
                suites_ok = suites_ok and state in ('passed', 'skipped', 'pending')
                continue
            skipped = state in ('skipped', 'pending')
            counts['skipped' if skipped else 'executed'] += 1
            if not skipped:
                counts['passed' if state == 'passed' else 'failed'] += 1
            labels = spec.get('LeafNodeLabels', [])
            containers = spec.get('ContainerHierarchyLabels', [])
            labels = [] if labels is None else labels
            containers = [] if containers is None else containers
            if (not isinstance(labels, list) or not isinstance(containers, list)
                    or any(group is not None and not isinstance(group, list) for group in containers)):
                raise ValueError('invalid labels')
            known = sorted({label for group in [labels, *containers] for label in (group or [])
                            if isinstance(label, str) and label in LABELS})
            name = f'spec-{spec_count}'
            safe_specs.append(dict(LeafNodeType='It', LeafNodeText=name, State=state,
                                   RunTime=elapsed_spec, LeafNodeLabels=known))
            case = ET.SubElement(suite_xml, 'testcase', name=name, time=str(elapsed_spec / 1e9))
            if skipped:
                ET.SubElement(case, 'skipped')
            elif state != 'passed':
                ET.SubElement(case, 'failure', message='spec failed')
        safe.append(dict(SuiteDescription=f'suite-{index}', SuitePath='unavailable',
                         SuiteSucceeded=report['SuiteSucceeded'], RunTime=elapsed, SpecReports=safe_specs))
    summary = dict(source_sha=sha, preset=preset, **counts, duration=total / 1e9,
                   tests_ok=(suites_ok and counts['executed'] > 0 and counts['failed'] == 0
                             and runtime['test_exit'] == 0), cleanup_ok=runtime['cleanup_ok'])
    public.mkdir(mode=0o700)  # Never reuse stale public evidence.
    (public / 'results.json').write_text(json.dumps(safe, separators=(',', ':')))
    ET.ElementTree(xml).write(public / 'junit.xml', encoding='utf-8', xml_declaration=True)
    (public / 'summary.json').write_text(json.dumps(summary, separators=(',', ':')))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--private', type=Path, required=True)
    parser.add_argument('--public', type=Path, required=True)
    parser.add_argument('--source-sha', required=True)
    parser.add_argument('--preset', required=True)
    args = parser.parse_args()
    try:
        project(args.private, args.public, args.source_sha, args.preset)
    except (ValueError, OSError, TypeError, KeyError):
        raise SystemExit('Native evidence unavailable or invalid; no public report produced')
