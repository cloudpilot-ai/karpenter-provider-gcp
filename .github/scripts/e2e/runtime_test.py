"""Exercise fixed deployment shell at its external crane/Helm process boundary."""
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).with_name('runtime.sh').resolve()
SHA = 'a' * 40


class RuntimeShellTest(unittest.TestCase):
    def test_deploy_records_only_exact_successful_image_and_removes_helper_config(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            tools = root / 'bin'
            tools.mkdir()
            (tools / 'crane').write_text('''#!/usr/bin/env bash
set -euo pipefail
[[ $1 == push && $4 == --image-refs ]]
printf '%s' "$DOCKER_CONFIG" > "$TEST_AUTH_PATH"
grep -q '"credHelpers":{"us-central1-docker.pkg.dev":"gcloud"}' "$DOCKER_CONFIG/config.json"
[[ ${TEST_BAD_DIGEST:-} != yes ]] || { printf '%s@sha256:SECRET\n@sha256:%064d' "${3%:*}" 0 > "$5"; exit 0; }
printf '%s@sha256:%064d' "${3%:*}" 0 > "$5"
''')
            (tools / 'helm').write_text('''#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$TEST_HELM_LOG"
[[ ${TEST_HELM_FAIL:-} != yes ]]
''')
            for tool in tools.iterdir():
                tool.chmod(0o700)
            inputs = [root / name for name in ['controller.tar', 'chart.tgz', 'crd.tgz']]
            for file in inputs:
                file.write_text('process fixture')
            env = dict(PATH=f'{tools}:/usr/bin:/bin', HOME=str(root), TMPDIR=str(root),
                       E2E_PROJECT_ID='project', E2E_LOCATION='us-central1-a', E2E_REGION='us-central1',
                       E2E_CLUSTER_NAME='e2e-cluster', E2E_IMAGE_REPOSITORY='us-central1-docker.pkg.dev/project/images/karpenter',
                       E2E_CONTROLLER_SERVICE_ACCOUNT='controller@project.iam.gserviceaccount.com',
                       E2E_NODE_SERVICE_ACCOUNT='node@project.iam.gserviceaccount.com',
                       TEST_AUTH_PATH=str(root / 'auth-path'), TEST_HELM_LOG=str(root / 'helm-log'))
            for case, extra, success in [('ok', {}, True), ('bad', {'TEST_BAD_DIGEST': 'yes'}, False),
                                         ('helm-fail', {'TEST_HELM_FAIL': 'yes'}, False)]:
                results = root / case
                results.mkdir()
                completed = subprocess.run(['bash', str(SCRIPT), 'deploy', *map(str, inputs), SHA, str(results)],
                                           env={**env, **extra}, capture_output=True)
                self.assertEqual(completed.returncode == 0, success, (case, completed.stdout, completed.stderr))
                self.assertFalse(Path((root / 'auth-path').read_text()).exists())
                if success:
                    self.assertEqual((results / 'controller-image.txt').read_text(),
                                     env['E2E_IMAGE_REPOSITORY'] + '@sha256:' + '0' * 64 + '\n')
                    self.assertIn('controller.image.digest=sha256:' + '0' * 64, (root / 'helm-log').read_text())
                else:
                    self.assertFalse((results / 'controller-image.txt').exists())

    def test_preauth_uses_suite_vcs_and_cli_module_version_without_running_pr_binaries(self):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            tools = root / 'bin'
            tools.mkdir()
            for name in ('crane', 'helm', 'e2e-runner'):
                (tools / name).write_text('#!/usr/bin/env bash\nexit 0\n')
            (tools / 'go').write_text('''#!/usr/bin/env bash
set -euo pipefail
[[ $1 == version && $2 == -m ]]
printf '\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n'
if [[ $3 == */e2e.test ]]; then
  printf '\tdep\tgithub.com/onsi/ginkgo/v2\tv2.33.0\n\tbuild\tvcs.revision=%s\n\tbuild\tvcs.modified=false\n' "$SOURCE_SHA"
else
  printf '\tmod\tgithub.com/onsi/ginkgo/v2\t%s\n' "${TEST_CLI_VERSION:-v2.33.0}"
fi
''')
            for tool in tools.iterdir():
                tool.chmod(0o700)
            for name in ('ginkgo', 'e2e.test'):
                directory = root / 'prepared' / name
                directory.mkdir(parents=True)
                (directory / name).write_text('not executable; must never be launched before WIF')
            env = dict(PATH=f'{tools}:/usr/bin:/bin', HOME=str(root), RUNNER_TEMP=str(root), SOURCE_SHA=SHA)
            for version, success in [('v2.33.0', True), ('v2.32.0', False)]:
                completed = subprocess.run(['bash', str(SCRIPT.with_name('preauth.sh'))],
                                           env={**env, 'TEST_CLI_VERSION': version}, capture_output=True)
                self.assertEqual(completed.returncode == 0, success, completed.stderr)


if __name__ == '__main__':
    unittest.main()
