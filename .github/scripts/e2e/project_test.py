import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('project', Path(__file__).with_name('project.py'))
project = importlib.util.module_from_spec(spec)
spec.loader.exec_module(project)
SHA = 'a' * 40


class ProjectionTest(unittest.TestCase):
    def test_native_text_is_not_public(self):
        with tempfile.TemporaryDirectory() as root:
            private, public = Path(root) / 'private', Path(root) / 'public'
            private.mkdir()
            (private / 'runtime.json').write_text(json.dumps(dict(source_sha=SHA, test_exit=0, cleanup_ok=True)))
            report = [dict(SuiteSucceeded=True, RunTime=1.5, SpecReports=[dict(
                State='passed', RunTime=0.2, LeafNodeType='It', LeafNodeText='SECRET', ContainerHierarchyLabels=[None],
                ContainerHierarchyTexts=['SECRET'], LeafNodeLabels=['gpu', 'SECRET'],
                Failure=dict(Message='SECRET'), CapturedStdOutErr='SECRET')])]
            (private / 'results.json').write_text(json.dumps(report))
            project.project(private, public, SHA, 'standard')
            self.assertNotIn('SECRET', ''.join(p.read_text() for p in public.iterdir()))
            summary = json.loads((public / 'summary.json').read_text())
            self.assertEqual(summary['executed'], 1)
            self.assertTrue(summary['tests_ok'])

    def test_zero_failed_cleanup_exit_and_corrupt_evidence_never_pass(self):
        for case in ('zero', 'failed', 'cleanup', 'exit', 'missing', 'corrupt', 'source', 'symlink', 'state', 'duration', 'runtime-bool', 'node'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as root:
                private, public = Path(root) / 'private', Path(root) / 'public'
                private.mkdir()
                runtime = dict(source_sha=SHA, test_exit=0, cleanup_ok=True)
                report = [dict(SuiteSucceeded=True, RunTime=1000000000, SpecReports=[dict(
                    State='passed', RunTime=200000000, LeafNodeType='It', LeafNodeLabels=None,
                    ContainerHierarchyLabels=[None, ['suite:storage', 'SECRET']])])]
                if case == 'node':
                    report[0]['SpecReports'].append(dict(State='passed', RunTime=1, LeafNodeType='SECRET'))
                if case == 'zero':
                    report[0]['SpecReports'] = []
                if case == 'failed':
                    report[0]['SpecReports'][0]['State'] = 'failed'
                if case == 'cleanup':
                    runtime['cleanup_ok'] = False
                if case == 'exit':
                    runtime['test_exit'] = None
                if case == 'source':
                    runtime['source_sha'] = 'b' * 40
                if case == 'state':
                    report[0]['SpecReports'][0]['State'] = 'SECRET'
                if case == 'duration':
                    report[0]['RunTime'] = float('nan')
                if case == 'runtime-bool':
                    runtime['test_exit'] = False
                (private / 'runtime.json').write_text(json.dumps(runtime))
                if case != 'missing':
                    (private / 'results.json').write_text('SECRET' if case == 'corrupt' else json.dumps(report))
                if case == 'symlink':
                    (private / 'results.json').unlink()
                    (private / 'results.json').symlink_to(private / 'runtime.json')
                if case in ('missing', 'corrupt', 'source', 'symlink', 'state', 'duration', 'runtime-bool', 'node'):
                    with self.assertRaises((ValueError, OSError)):
                        project.project(private, public, SHA, 'standard')
                    self.assertFalse(public.exists())
                else:
                    project.project(private, public, SHA, 'standard')
                    summary = json.loads((public / 'summary.json').read_text())
                    self.assertFalse(summary['tests_ok'] and summary['cleanup_ok'])



if __name__ == '__main__':
    unittest.main()
