#!/usr/bin/env python3
"""Local signing regression tests; no registry or Sigstore writes."""
import base64
import functools
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import os
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ChartSigningTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.home = Path(cls.temp.name)
        cls.gpg = cls.home / "gpg"
        cls.gpg.mkdir(mode=0o700)
        cls.env = dict(os.environ, GNUPGHOME=str(cls.gpg))
        subprocess.run(["gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "test-passphrase",
                        "--quick-generate-key", "Chart Test <chart@example.invalid>", "rsa2048", "sign", "1d"],
                       env=cls.env, check=True, capture_output=True)
        listing = subprocess.check_output(["gpg", "--with-colons", "--list-secret-keys"], env=cls.env, text=True)
        cls.fingerprint = next(line.split(":")[9] for line in listing.splitlines() if line.startswith("fpr:"))
        private = subprocess.check_output(["gpg", "--batch", "--pinentry-mode", "loopback", "--passphrase", "test-passphrase",
                                           "--export-secret-keys", cls.fingerprint], env=cls.env)
        cls.sign_env = dict(os.environ, HELM_SIGNING_KEY_BASE64=base64.b64encode(private).decode(),
                            HELM_SIGNING_KEY_PASSPHRASE="test-passphrase",
                            HELM_SIGNING_KEY_FINGERPRINT=cls.fingerprint)

    @classmethod
    def tearDownClass(cls):
        subprocess.run(["gpgconf", "--kill", "gpg-agent"], env=cls.env, check=True)
        cls.temp.cleanup()

    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.path = Path(self.work.name)
        for chart in ("karpenter", "karpenter-crd"):
            target = self.path / "charts" / chart
            target.mkdir(parents=True)
            # Small valid charts isolate signing from template/dependency behavior.
            (target / "Chart.yaml").write_text(f"apiVersion: v2\nname: {chart}\nversion: 0.0.0\n")
        self.output = self.path / "output"
        self.output.mkdir()

    def sign(self, env=None):
        return subprocess.run(["bash", str(ROOT / "hack/signing/package-charts.sh"), "1.2.3", "v1.2.3", str(self.output)],
                              cwd=self.path, env=env or self.sign_env, capture_output=True, text=True)

    def test_signs_both_charts_with_encrypted_key(self):
        result = self.sign()
        self.assertEqual(result.returncode, 0, result.stderr)
        public = self.output / "keys" / f"{self.fingerprint}.asc"
        self.assertTrue(public.is_file())
        keyring = self.path / "public.gpg"
        keyring.write_bytes(subprocess.check_output(["gpg", "--batch", "--dearmor"], input=public.read_bytes()))
        for chart in ("karpenter", "karpenter-crd"):
            archive = self.output / f"{chart}-1.2.3.tgz"
            subprocess.run(["helm", "verify", str(archive), "--keyring", str(keyring)], check=True, capture_output=True)
            self.assertIn(f"{archive.name} {self.fingerprint}", (self.output / "signed-charts.txt").read_text())
        self.assertFalse(any(self.output.rglob("*secring*")))

    def test_wrong_fingerprint_publishes_nothing(self):
        result = self.sign(dict(self.sign_env, HELM_SIGNING_KEY_FINGERPRINT="A" * 40))
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(self.output.iterdir()), [])

    def test_tampered_chart_and_wrong_keyring_are_rejected(self):
        result = self.sign()
        self.assertEqual(result.returncode, 0, result.stderr)
        public = self.output / "keys" / f"{self.fingerprint}.asc"
        keyring = self.path / "public.gpg"
        keyring.write_bytes(subprocess.check_output(["gpg", "--batch", "--dearmor"], input=public.read_bytes()))
        archive = self.output / "karpenter-1.2.3.tgz"
        wrong_keyring = self.path / "empty.gpg"
        wrong_keyring.touch()
        result = subprocess.run(["helm", "verify", str(archive), "--keyring", str(wrong_keyring)], capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        with archive.open("ab") as target:
            target.write(b"tampered")
        result = subprocess.run(["helm", "verify", str(archive), "--keyring", str(keyring)], capture_output=True)
        self.assertNotEqual(result.returncode, 0)


class RepositoryHandler(SimpleHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        if getattr(self.server, "fail_requests", False):
            self.send_error(503)
        else:
            super().do_GET()


class ProvenanceRestorationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.remote = self.root / "remote"
        self.local = self.root / "local"
        self.remote.mkdir()
        self.local.mkdir()
        (self.local / "karpenter-1.0.0.tgz").write_bytes(b"old archive")
        handler = functools.partial(RepositoryHandler, directory=str(self.remote))
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self.server = server
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_port}"

    def restore(self):
        return subprocess.run(["python3", str(ROOT / "hack/signing/restore-provenance.py"), self.url, str(self.local)],
                              capture_output=True, text=True)

    def test_preserves_old_provenance_and_public_keys(self):
        fingerprint = "A" * 40
        record = f"karpenter-1.0.0.tgz {fingerprint}\n"
        (self.remote / "signed-charts.txt").write_text(record)
        (self.remote / "karpenter-1.0.0.tgz.prov").write_bytes(b"old provenance")
        (self.remote / "keys").mkdir()
        (self.remote / "keys" / f"{fingerprint}.asc").write_bytes(b"old public key")
        result = self.restore()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.local / "karpenter-1.0.0.tgz.prov").read_bytes(), b"old provenance")
        self.assertEqual((self.local / "keys" / f"{fingerprint}.asc").read_bytes(), b"old public key")
        self.assertEqual((self.local / "signed-charts.txt").read_text(), record)

    def test_unsigned_history_may_have_no_provenance(self):
        result = self.restore()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.local / "karpenter-1.0.0.tgz.prov").exists())

    def test_known_signed_history_cannot_lose_provenance(self):
        (self.remote / "signed-charts.txt").write_text(f"karpenter-1.0.0.tgz {'A' * 40}\n")
        result = self.restore()
        self.assertNotEqual(result.returncode, 0)

    def test_server_error_cannot_be_treated_as_unsigned(self):
        self.server.fail_requests = True
        self.assertNotEqual(self.restore().returncode, 0)

    def test_transport_failure_cannot_be_treated_as_unsigned(self):
        self.url = "http://127.0.0.1:0"
        self.assertNotEqual(self.restore().returncode, 0)


class ImageSigningTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        cosign = self.path / "cosign"
        cosign.write_text("#!/usr/bin/env python3\nimport json, os, sys\nwith open(os.environ['CALLS'], 'a') as f: f.write(json.dumps(sys.argv[1:]) + '\\n')\n")
        cosign.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{self.path}:{os.environ['PATH']}", CALLS=str(self.path / "calls"),
                        GITHUB_OUTPUT=str(self.path / "output"),
                        GITHUB_WORKFLOW_REF="cloudpilot-ai/karpenter-provider-gcp/.github/workflows/release-build.yaml@refs/tags/v1.2.3")

    def sign(self, image):
        return subprocess.run(["bash", str(ROOT / "hack/signing/sign-image.sh"), image], env=self.env,
                              capture_output=True, text=True)

    def test_signs_digest_and_verifies_exact_workflow_identity(self):
        image = "public.ecr.aws/cloudpilotai/gcp/karpenter@sha256:" + "a" * 64
        result = self.sign(image)
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [json.loads(line) for line in (self.path / "calls").read_text().splitlines()]
        self.assertEqual([call[0] for call in calls], ["sign", "verify"])
        self.assertTrue(all(call[-1] == image for call in calls))
        self.assertIn("https://github.com/" + self.env["GITHUB_WORKFLOW_REF"], calls[1])
        self.assertIn("https://token.actions.githubusercontent.com", calls[1])
        self.assertIn("--new-bundle-format=false", calls[0])
        self.assertIn("--new-bundle-format=false", calls[1])

    def test_rejects_mutable_unexpected_or_multiple_references(self):
        valid = "public.ecr.aws/cloudpilotai/gcp/karpenter@sha256:" + "a" * 64
        for image in ("public.ecr.aws/cloudpilotai/gcp/karpenter:latest", "other.example/image@sha256:" + "a" * 64,
                      valid + "\n" + valid, valid + "\nextra output"):
            with self.subTest(image=image):
                self.assertNotEqual(self.sign(image).returncode, 0)
        self.assertFalse((self.path / "calls").exists())


if __name__ == "__main__":
    unittest.main()
