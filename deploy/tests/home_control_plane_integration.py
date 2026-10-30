#!/usr/bin/env python3
"""Exercise sub2's real Go Home client against an isolated Home v1.1.7.

Example:
  python3 deploy/tests/home_control_plane_integration.py \
      --home-source /path/to/CLIProxyAPIHome \
      --home-binary /path/to/Home --sub2-backend ./backend

No production endpoints or credentials are accepted. All generated state stays
in a private temporary directory. The Home process and rejecting proxy are
always stopped. Omitting --home-binary builds the validated source in that
directory. Public Go dependency downloads may be needed before tests start.
"""

import argparse
import base64
import hashlib
import http.server
import json
import os
from pathlib import Path
import secrets
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid


TEST_NAME = "TestHomeControlPlaneInteroperability"
EXPECTED_TAG = "v1.1.7"


def private_write(path, content):
    with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w") as target:
        target.write(content)
    os.chmod(path, 0o600)


def source_evidence(source):
    def git(*args):
        return subprocess.check_output(["git", "-C", str(source), *args], text=True).strip()
    if git("describe", "--tags", "--exact-match", "HEAD") != EXPECTED_TAG:
        raise RuntimeError("Home source must be checked out at exact tag v1.1.7")
    if git("status", "--porcelain", "--untracked-files=no"):
        raise RuntimeError("Home source has tracked modifications")
    return git("rev-parse", "HEAD")


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_ready(check, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if check():
            return
        time.sleep(0.1)
    raise RuntimeError("isolated Home did not become ready; inspect its private home.log")


def enroll(port, certificate_id, secret, csr):
    args = [b"CERTIFICATE", b"REQUEST", certificate_id.encode(), secret.encode(), csr.encode()]
    wire = b"*5\r\n" + b"".join(b"$%d\r\n" % len(item) + item + b"\r\n" for item in args)
    with socket.create_connection(("127.0.0.1", port), timeout=5) as sock:
        sock.sendall(wire)
        with sock.makefile("rb") as reader:
            line = reader.readline(1024)
            if not line.startswith(b"$"):
                raise RuntimeError("Home refused fixture certificate enrollment")
            size = int(line[1:-2])
            if not 0 < size <= 1024 * 1024:
                raise RuntimeError("invalid fixture enrollment reply size")
            payload = reader.read(size)
            if reader.read(2) != b"\r\n":
                raise RuntimeError("invalid fixture enrollment reply")
    return json.loads(payload)


def run(args, root):
    source, backend = args.home_source.resolve(), args.sub2_backend.resolve()
    revision = source_evidence(source)
    binary = args.home_binary.resolve() if args.home_binary else root / "Home"
    if not args.home_binary:
        subprocess.run([args.go, "build", "-o", str(binary), "./cmd/home"], cwd=source, check=True)
    metadata = subprocess.check_output([args.go, "version", "-m", str(binary)], text=True)
    if "vcs.revision=" + revision not in metadata or "vcs.modified=false" not in metadata:
        raise RuntimeError("Home binary must have clean build metadata matching the source; omit --home-binary to build it")
    port = free_port()
    management_key, service_key = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    blocked_targets = []

    class RejectProxy(http.server.BaseHTTPRequestHandler):
        def do_CONNECT(self):
            blocked_targets.append(self.path)
            self.send_error(502, "Fixture rejects upstream traffic")

        def do_GET(self):
            # Do not retain URLs, which could contain authentication parameters.
            blocked_targets.append("rejected-http-request")
            self.send_error(502, "Fixture rejects upstream traffic")

        def do_POST(self):
            self.do_GET()

        def log_message(self, *unused):
            pass

    proxy = http.server.ThreadingHTTPServer(("127.0.0.1", 0), RejectProxy)
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    proxy_url = "http://127.0.0.1:" + str(proxy.server_port)
    home_env = os.environ.copy()
    for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"):
        home_env[name] = proxy_url
    home_env["NO_PROXY"] = home_env["no_proxy"] = "localhost,127.0.0.1"
    config = {
        "config-version": 8,
        "server": {"host": "127.0.0.1", "port": port, "allow-host": ["127.0.0.1"]},
        "management": {"secret-key": management_key, "allow-remote": False, "disable-control-panel": True},
        "requests": {"proxy-url": proxy_url}, "access": {"api-keys": []},
        "models": {
            "catalog": str(source / "internal/registry/models/models.json"),
            "codex-catalog": str(source / "internal/registry/models/codex_client_models.json"),
            "devin-catalog": str(source / "internal/registry/models/devin_models.json"),
        },
    }
    private_write(root / "config.yaml", json.dumps(config))
    private_write(root / "cluster.yaml", json.dumps({
        "sqlite": {"path": str(root / "home.db")},
        "node": {"external-ip": "127.0.0.1", "port": port, "heartbeat-interval": "1s",
                 "heartbeat-timeout": "20s", "event-poll-interval": "100ms"},
    }))
    (root / "auths").mkdir(mode=0o700)
    process = None
    logfile = open(root / "home.log", "w")
    os.chmod(root / "home.log", 0o600)
    try:
        subprocess.run([str(binary), "-config", str(root / "config.yaml"), "-auth-dir", str(root / "auths"), "-import"],
                       cwd=root, env=home_env, stdout=logfile, stderr=subprocess.STDOUT, check=True, timeout=30)
        process = subprocess.Popen([str(binary), "-config", str(root / "config.yaml"), "-addr", f"127.0.0.1:{port}"],
                                   cwd=root, env=home_env, stdout=logfile, stderr=subprocess.STDOUT)
        base = f"http://127.0.0.1:{port}/v8/management"
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

        def api(path, body=None, method=None):
            request = urllib.request.Request(base + path, data=None if body is None else json.dumps(body).encode(), method=method,
                                             headers={"Authorization": "Bearer " + management_key, "Content-Type": "application/json"})
            try:
                with opener.open(request, timeout=5) as response:
                    return json.load(response)
            except urllib.error.HTTPError as error:
                # Response bodies can contain credentials; retain only status.
                raise RuntimeError(f"fixture management {path} returned HTTP {error.code}") from None

        def ready():
            if process.poll() is not None:
                raise RuntimeError("isolated Home exited before readiness")
            try:
                return bool(api("/capabilities"))
            except (OSError, RuntimeError):
                return False

        wait_ready(ready)

        def encoded(value):
            return base64.urlsafe_b64encode(json.dumps(value).encode()).rstrip(b"=").decode()

        account_id = "fixture-account"
        id_token = encoded({"alg": "none"}) + "." + encoded({
            "exp": 4102444800, "email": "fixture@example.invalid",
            "https://api.openai.com/auth": {"chatgpt_plan_type": "plus", "chatgpt_account_id": account_id},
        }) + ".fixture"
        current, previous = "fixture-current-" + secrets.token_urlsafe(24), "fixture-previous-" + secrets.token_urlsafe(24)
        upload = api("/credentials", {
            "uuid": str(uuid.uuid4()), "type": "codex", "access_token": current,
            "refresh_token": "synthetic-never-forward", "account_id": account_id,
            "email": "fixture@example.invalid", "id_token": id_token,
            "expired": "2100-01-01T00:00:00Z", "proxy_url": proxy_url,
        })
        credential_id = upload["name"].removesuffix(".json")
        group = api("/channel-groups", {"channel_name": "fixture-only"})["channel_group"]["id"]
        api("/channel-group-details", {"channel_group_id": group, "auth_id": credential_id})
        api("/access/api-keys", {"api_key": service_key, "display_name": "isolated fixture", "channels": [group]})
        api("/credentials/" + credential_id + "/concurrency-policy", {"max_in_flight": 3}, "PATCH")

        # Enrollment only: production Go clients must create the subscriptions.
        clients = []
        for name in ("node-a", "node-b"):
            node = root / name
            node.mkdir(mode=0o700)
            issued = api("/certificates/clients", {"node_name": name})
            claims = json.loads(base64.urlsafe_b64decode(issued["home_jwt"].split(".")[1] + "=="))
            subprocess.run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes",
                            "-keyout", str(node / "key.pem"), "-out", str(node / "req.pem"), "-subj", "/CN=" + issued["id"]],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
            os.chmod(node / "key.pem", 0o600)
            signed = enroll(port, issued["id"], claims["enrollment_secret"], (node / "req.pem").read_text())
            if not signed["ok"] or hashlib.sha256(ssl.PEM_cert_to_DER_cert(signed["ca"])).hexdigest() != claims["ca_fingerprint"]:
                raise RuntimeError("fixture enrollment CA fingerprint mismatch")
            private_write(node / "cert.pem", signed["certificate"])
            private_write(node / "ca.pem", signed["ca"])
            clients.append({"Address": f"127.0.0.1:{port}", "ServerName": "127.0.0.1",
                            "CAFile": str(node / "ca.pem"), "CertFile": str(node / "cert.pem"),
                            "KeyFile": str(node / "key.pem"), "APIKey": service_key})

        fixture = {"source_tag": EXPECTED_TAG, "source_commit": revision, "management_url": base,
                   "management_key": management_key, "credential_id": credential_id, "model": "gpt-5.5",
                   "access_token": current, "previous_token": previous, "account_id": account_id, "clients": clients}
        private_write(root / "go-fixture.json", json.dumps(fixture))
        test_env = os.environ.copy()
        test_env["SUB2_HOME_INTEGRATION_CONFIG"] = str(root / "go-fixture.json")
        command = [args.go, "test", "-json", "-count=1", "-tags=homeintegration", "-run", "^" + TEST_NAME + "$",
                   "-timeout=120s", "./internal/homeintegration"]
        completed = subprocess.run(command, cwd=backend, env=test_env, text=True, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, timeout=args.timeout)
        private_write(root / "go-test.jsonl", completed.stdout)
        ran = passed = skipped = False
        for line in completed.stdout.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get("Test") == TEST_NAME:
                ran |= event.get("Action") == "run"
                passed |= event.get("Action") == "pass"
                skipped |= event.get("Action") == "skip"
        if completed.returncode != 0 or not ran or not passed or skipped:
            raise RuntimeError("real homeclient interoperability did not execute and pass; inspect private go-test.jsonl")
        # No token rotation was requested. Any OAuth CONNECT would mean the
        # observed-token path or fixture unexpectedly attempted provider refresh.
        if any(target.startswith("auth.openai.com:") for target in blocked_targets):
            raise RuntimeError("unexpected provider refresh attempt during observed-token interoperability test")
        summary = {"source_tag": EXPECTED_TAG, "source_commit": revision,
                   "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                   "test": TEST_NAME, "test_executed": ran, "test_passed": passed,
                   "port": port, "credential_id": credential_id, "artifacts": str(root),
                   "blocked_upstream_targets": sorted(set(blocked_targets))}
        private_write(root / "summary.json", json.dumps(summary, indent=2))
        print(json.dumps(summary, indent=2))
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        logfile.close()
        proxy.shutdown()
        proxy.server_close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--home-source", type=Path, required=True)
    parser.add_argument("--home-binary", type=Path)
    parser.add_argument("--sub2-backend", type=Path, default=Path(__file__).resolve().parents[2] / "backend")
    parser.add_argument("--go", default="go")
    parser.add_argument("--timeout", type=int, default=300, help="Go command timeout, including compilation, in seconds")
    args = parser.parse_args()
    root = Path(tempfile.mkdtemp(prefix="sub2-home-integration-"))
    os.chmod(root, 0o700)
    try:
        run(args, root)
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.SubprocessError) as error:
        # Exceptions and subprocess representations may carry inputs. Keep
        # synthetic credentials out of the console, even on setup failures.
        print(f"Home integration failed ({type(error).__name__}); private artifacts: {root}")
        if isinstance(error, RuntimeError):
            print(str(error))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
