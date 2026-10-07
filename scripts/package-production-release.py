#!/usr/bin/env python3
"""Build the complete deployment release; native inputs are files or closed trees."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import yaml

ROOT = Path(__file__).resolve().parents[1]


def run(argv, cwd=ROOT, env=None):
    subprocess.run(argv, cwd=cwd, env=env, check=True)


def bundle(target, tree, entry):
    entry_path = tree / entry[0]
    if not entry_path.is_file() or not os.access(entry_path, os.X_OK):
        raise ValueError(f"bundle entry is not executable: {entry_path}")
    for path in tree.rglob("*"):
        if path.is_symlink():
            resolved = path.resolve(strict=True)
            if path.readlink().is_absolute() or not resolved.is_relative_to(tree.resolve()):
                raise ValueError(f"bundle dependency escapes its supplied tree: {path}")
    header = """#!/bin/sh
set -eu
umask 077
cache=${PLATFORM_RECOVERY_WORKSPACE:-/var/lib/ebpf-wg-mesh/bundles}
digest=$(sha256sum "$0")
digest=${digest%% *}
dir="$cache/$digest"
mkdir -p "$cache"
# An interrupted extraction can be retried; only a complete directory is
# published under its content-addressed name.
stage=$(mktemp -d "$cache/.unpack-XXXXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
if ! test -f "$dir/.complete"; then
 line=$(awk '/^__PLATFORM_ARCHIVE__$/ {print NR+1;exit}' "$0")
 tail -n +"$line" "$0" | tar -xz -C "$stage"
 touch "$stage/.complete"
 if ! mv -T "$stage" "$dir" 2>/dev/null; then
  test -f "$dir/.complete"
 fi
fi
"""
    command = 'exec "$dir/' + entry[0].replace('"', '\\"') + '"'
    for arg in entry[1:]:
        if arg.startswith("@bundle/"):
            command += ' "$dir/' + arg[len("@bundle/"):].replace('"', '\\"') + '"'
        else:
            command += " " + shlex.quote(arg)
    header += 'rm -rf "$stage"\ntrap - EXIT HUP INT TERM\n' + command + ' "$@"\n__PLATFORM_ARCHIVE__\n'
    with target.open("wb") as output:
        output.write(header.encode())
        with gzip.GzipFile(fileobj=output, mode="wb", mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.PAX_FORMAT) as archive:
                for path in sorted(tree.iterdir()):
                    archive.add(path, arcname=path.name)
    target.chmod(0o755)


def verify_native(path, architecture):
    with path.open("rb") as binary:
        header = binary.read(20)
    expected = {"amd64": 62, "arm64": 183}[architecture]
    if header[:4] != b"\x7fELF" or len(header) < 20 or header[5] != 1 or int.from_bytes(header[18:20], "little") != expected:
        raise ValueError(f"native input must be a Linux {architecture} ELF executable: {path}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--id", required=True)
    parser.add_argument("--url-base", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--native-inputs", required=True, type=Path,
                        help="JSON: {name: {amd64: {path, entry?}, arm64: ...}}; names: cockroachdb, envoy, registry, bun, aws, skopeo")
    parser.add_argument("--architectures", default="amd64,arm64")
    parser.add_argument("--skip-console-build", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_.-]*", args.id):
        parser.error("release id must be a filename-safe identity")
    if not args.url_base.startswith("https://"):
        parser.error("published artifact base must use HTTPS")
    architectures = args.architectures.split(",")
    if not architectures or len(set(architectures)) != len(architectures) or any(a not in ("amd64", "arm64") for a in architectures):
        parser.error("architectures must be distinct amd64/arm64 values")
    inputs = json.loads(args.native_inputs.read_text())
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    release = yaml.safe_load((ROOT / "infra/production/examples/releases/r43.yaml").read_text())
    release["id"] = args.id
    release["tools"] = {name: {} for name in ("operations", "platformctl", "console-admin", "cockroachdb", "aws", "skopeo")}
    if not args.skip_console_build:
        run(["bun", "run", "build"], cwd=ROOT / "console")
    for role in release["programs"].values():
        role["artifacts"] = {}
    release["images"] = {}  # Component processes are protected native artifacts.
    for arch in architectures:
        env = os.environ | {"GOOS": "linux", "GOARCH": arch, "CGO_ENABLED": "0"}
        for name in ("operations", "platformctl", "controlplane", "agent", "builder"):
            run(["go", "build", "-trimpath", "-o", str(output / f"{name}-{arch}"), f"./cmd/{name}"], env=env)
        target = "bun-linux-x64" if arch == "amd64" else "bun-linux-arm64"
        run(["bun", "build", "--compile", f"--target={target}", "tooling/production-admin.ts", "--outfile", str(output / f"console-admin-{arch}")], cwd=ROOT / "console")
        with tempfile.TemporaryDirectory(prefix="platform-release-") as tmp:
            tmp = Path(tmp)
            for name in ("cockroachdb", "envoy", "registry", "aws", "skopeo"):
                specification = inputs[name][arch]
                source = Path(specification["path"]).resolve(strict=True)
                artifact = output / f"{name}-{arch}"
                if source.is_file():
                    verify_native(source, arch)
                    shutil.copy2(source, artifact)
                    artifact.chmod(0o755)
                else:
                    entry = specification["entry"]
                    if any("\n" in arg for arg in entry) or Path(entry[0]).is_absolute() or ".." in Path(entry[0]).parts:
                        raise ValueError("bundle entry must stay inside the native dependency tree")
                    verify_native(source / entry[0], arch)
                    bundle(artifact, source, entry)
            console_tree = tmp / "console"
            console_tree.mkdir()
            shutil.copytree(ROOT / "console/.output", console_tree / ".output", symlinks=False)
            bun = Path(inputs["bun"][arch]["path"]).resolve(strict=True)
            verify_native(bun, arch)
            shutil.copy2(bun, console_tree / "bun")
            (console_tree / "bun").chmod(0o755)
            bundle(output / f"console-{arch}", console_tree, ["bun", "@bundle/.output/server/index.mjs"])
        for name, role in release["programs"].items():
            artifact = output / f"{name}-{arch}"
            role["artifacts"][arch] = {"url": f"{args.url_base.rstrip('/')}/{artifact.name}", "sha256": hashlib.sha256(artifact.read_bytes()).hexdigest()}
        for name, artifacts in release["tools"].items():
            artifact = output / f"{name}-{arch}"
            artifacts[arch] = {"url": f"{args.url_base.rstrip('/')}/{artifact.name}", "sha256": hashlib.sha256(artifact.read_bytes()).hexdigest()}
    # JSON is a supported release format, and keeps a build independent of YAML
    # serializer ordering and aliases. No placeholder hashes survive packaging.
    (output / "release.json").write_text(json.dumps(release, indent=2) + "\n")
    (output / "SHA256SUMS").write_text("".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n" for path in sorted(output.iterdir()) if path.is_file() and path.name != "SHA256SUMS"))
    print(output / "release.json")


if __name__ == "__main__":
    main()
