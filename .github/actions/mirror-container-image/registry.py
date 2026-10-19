"""Small subprocess boundary for a fixed regctl release; never invoke a shell."""

import hashlib
import json
import os
import re
import subprocess
import tempfile
import threading
import time


class MirrorError(RuntimeError):
    pass


def redact(message):
    """Registry redirects can include signed URLs; never archive their query strings."""
    for key in ("ACR_USERNAME", "ACR_PASSWORD"):
        if os.environ.get(key):
            message = message.replace(os.environ[key], "[REDACTED]")
    message = re.sub(r"https?://[^\s<>\"']+", lambda match: match[0].split("?", 1)[0] +
                     ("?[REDACTED]" if "?" in match[0] else ""), message)
    message = re.sub(r"(?i)(authorization[\"']?\s*[:=]\s*)\[[^\]\r\n]*\]", r"\1[REDACTED]", message)
    return re.sub(r"(?i)(authorization[\"']?\s*[:=]\s*[\"']?)[^\r\n,\"']+", r"\1[REDACTED]", message)


class Registry:
    def __init__(self, executable, config, *, anonymous=False, http_localhost=False):
        self.executable = executable
        self.config = str(config)
        self.anonymous = anonymous
        self.http_localhost = http_localhost

    def environment(self):
        env = dict(os.environ, REGCTL_CONFIG=self.config)
        if self.anonymous:
            # regctl must not import an existing publisher's Docker credential helper.
            env.pop("ACR_USERNAME", None)
            env.pop("ACR_PASSWORD", None)
        return env

    def command(self, args):
        cmd = [self.executable]
        if self.http_localhost:
            for arg in args:
                host = arg.split("/", 1)[0]
                if host.startswith(("127.0.0.1:", "localhost:")):
                    cmd.extend(["--host", f"reg={host},tls=disabled"])
        return cmd + args

    def call(self, *args, data=None, retry=True):
        attempts = 3 if retry else 1
        for attempt in range(attempts):
            result = subprocess.run(self.command(list(args)), input=data, capture_output=True,
                                    env=self.environment(), timeout=600, check=False)
            if result.returncode == 0:
                return result.stdout
            error = redact(result.stderr.decode(errors="replace").strip())
            # Only transport failures are retried. Authentication and semantic failures fail closed.
            transient = any(x in error.lower() for x in
                            ("429", "too many requests", "502", "503", "504", "timeout", "connection reset", "unexpected eof"))
            if not transient or attempt == attempts - 1:
                raise MirrorError(f"Registry command failed: {error[-3000:]}")
            time.sleep(2 ** attempt)
        raise MirrorError("Registry command failed")

    def json(self, *args):
        return json.loads(self.call(*args))

    def digest(self, reference):
        return self.call("image", "digest", reference).decode().strip()

    def optional_digest(self, reference):
        try:
            return self.digest(reference)
        except MirrorError as error:
            message = str(error).lower()
            if any(x in message for x in ("[http 404]", "manifest_unknown", "name_unknown", "status code 404", "status: 404")):
                return None
            raise

    def login(self, host):
        username = os.environ.get("ACR_USERNAME", "")
        password = os.environ.get("ACR_PASSWORD", "")
        if not username or not password:
            raise MirrorError("ACR_USERNAME and ACR_PASSWORD must be provided as secret environment variables")
        self.call("registry", "login", host, "--user", username, "--pass-stdin",
                  data=password.encode(), retry=False)

    def verify_blob(self, repository, descriptor):
        digest = hashlib.sha256()
        size = 0
        with tempfile.TemporaryFile() as errors:
            process = subprocess.Popen(self.command(["blob", "get", repository, descriptor["digest"]]),
                                       stdout=subprocess.PIPE, stderr=errors, env=self.environment())
            timer = threading.Timer(600, process.kill)
            timer.start()
            try:
                while chunk := process.stdout.read(1024 * 1024):
                    digest.update(chunk)
                    size += len(chunk)
                code = process.wait()
            finally:
                timer.cancel()
                process.stdout.close()
                if process.poll() is None:
                    process.kill()
                    process.wait()
            if code:
                errors.seek(0)
                raise MirrorError("Anonymous blob download failed: " + redact(errors.read().decode(errors="replace"))[-2000:])
        if f"sha256:{digest.hexdigest()}" != descriptor["digest"] or size != descriptor["size"]:
            raise MirrorError(f"Blob content mismatch: {descriptor['digest']}")
