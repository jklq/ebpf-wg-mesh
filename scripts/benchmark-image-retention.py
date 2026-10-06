#!/usr/bin/env python3
"""Measure Distribution disk reclamation for 20 versions retaining the last two.

Uses an isolated registry:2 container and disposable data. Output is JSON.
The registry is stopped during blob GC; no live uploads can race collection.
"""

import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def digest(body):
    return "sha256:" + hashlib.sha256(body).hexdigest()


def run():
    name = "retention-bench-" + uuid.uuid4().hex[:8]
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    base = f"http://127.0.0.1:{port}"
    with tempfile.TemporaryDirectory(prefix="registry-retention-") as root:
        root = Path(root)
        data = root / "data"
        data.mkdir()
        config = root / "config.yml"
        config.write_text("""version: 0.1
log:
  level: error
storage:
  delete:
    enabled: true
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: :5000
""")

        def request(method, url, body=None, content_type="application/octet-stream"):
            if url.startswith("/"):
                url = base + url
            req = urllib.request.Request(url, data=body, method=method)
            req.add_header("Content-Type", content_type)
            with urllib.request.urlopen(req, timeout=15) as response:
                return response.status, dict(response.headers), response.read()

        def upload(repo, body):
            _, headers, _ = request("POST", f"/v2/{repo}/blobs/uploads/", b"")
            url = headers["Location"]
            request("PUT", url + ("&" if "?" in url else "?") + "digest=" + digest(body), body)
            return {"digest": digest(body), "size": len(body)}

        def blob_bytes():
            return sum(path.stat().st_size for path in data.rglob("data") if path.is_file())

        try:
            docker("run", "-d", "--name", name, "--user", f"{os.getuid()}:{os.getgid()}",
                   "--memory=128m", "--cpus=2", "-p", f"127.0.0.1:{port}:5000",
                   "-v", f"{data}:/var/lib/registry", "-v", f"{config}:/etc/distribution/config.yml:ro",
                   "registry:2", "serve", "/etc/distribution/config.yml")
            for _ in range(100):
                try:
                    request("GET", "/v2/")
                    break
                except (urllib.error.URLError, ConnectionError):
                    time.sleep(0.1)
            common = os.urandom(1 << 20)
            manifests = []
            manifest_type = "application/vnd.docker.distribution.manifest.v2+json"
            layer_type = "application/vnd.docker.image.rootfs.diff.tar.gzip"
            for version in range(20):
                repo = f"mesh/project/environment/build-{version}/service"
                config_blob = upload(repo, json.dumps({"version": version}).encode())
                config_blob["mediaType"] = "application/vnd.docker.container.image.v1+json"
                layers = [upload(repo, common), upload(repo, os.urandom(1 << 20))]
                for layer in layers:
                    layer["mediaType"] = layer_type
                manifest = json.dumps({"schemaVersion": 2, "mediaType": manifest_type,
                                       "config": config_blob, "layers": layers}).encode()
                request("PUT", f"/v2/{repo}/manifests/git-version", manifest, manifest_type)
                manifests.append((repo, digest(manifest)))
            before = blob_bytes()
            started = time.monotonic()
            for repo, manifest_digest in manifests[:-2]:
                status, _, _ = request("DELETE", f"/v2/{repo}/manifests/{manifest_digest}")
                assert status == 202
            after_delete = blob_bytes()
            docker("stop", name)
            gc_output = docker("run", "--rm", "--user", f"{os.getuid()}:{os.getgid()}",
                               "--memory=128m", "--cpus=2", "-v", f"{data}:/var/lib/registry",
                               "-v", f"{config}:/etc/distribution/config.yml:ro", "registry:2",
                               "garbage-collect", "/etc/distribution/config.yml")
            after_gc = blob_bytes()
            docker("start", name)
            for repo, manifest_digest in manifests[-2:]:
                for attempt in range(100):
                    try:
                        request("GET", f"/v2/{repo}/manifests/{manifest_digest}")
                        break
                    except (urllib.error.URLError, ConnectionError):
                        if attempt == 99:
                            raise
                        time.sleep(0.1)
            result = {"versions_before": 20, "versions_after": 2,
                      "blob_bytes_before": before, "blob_bytes_after_manifest_delete": after_delete,
                      "blob_bytes_after_gc": after_gc,
                      "reduction_percent": 100 * (before - after_gc) / before,
                      "cleanup_and_restart_seconds": time.monotonic() - started,
                      "gc_summary": gc_output.splitlines()[-1]}
            print(json.dumps(result, indent=2))
        finally:
            subprocess.run(["docker", "rm", "-f", name], capture_output=True, check=False)


if __name__ == "__main__":
    run()
