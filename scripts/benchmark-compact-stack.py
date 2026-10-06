#!/usr/bin/env python3
"""Benchmark the compact stack inside one 2 GiB, two-CPU Linux cgroup.
Requires Docker and Go. Uses a fresh output directory and disposable containers.
The control plane and agent share the Go test process; the console is excluded.
Run: python3 scripts/benchmark-compact-stack.py [NEW_OUTPUT_DIRECTORY]
"""
import json
import pathlib
import subprocess
import sys
import tempfile
import time
import uuid

repo = pathlib.Path(__file__).resolve().parent.parent
output = pathlib.Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else pathlib.Path(tempfile.mkdtemp(prefix="compact-stack-"))
output.mkdir(parents=True, exist_ok=True)
if list(output.iterdir()):
    raise SystemExit("choose an empty output directory")
name = "compact-stack-" + uuid.uuid4().hex[:12]
copy_name = name + "-copy"
fixture = r"""
#!/usr/bin/env bash
set -euo pipefail
apt-get install -y -qq --no-install-recommends postgresql time >/dev/null
version=$(ls /usr/lib/postgresql)
pg_ctlcluster "$version" main stop || true
pg_ctlcluster "$version" main start -o '-c shared_buffers=32MB -c work_mem=1MB -c maintenance_work_mem=16MB -c max_connections=24 -c autovacuum_max_workers=1'
runuser -u postgres -- psql -c "ALTER USER postgres PASSWORD 'compact-test'" -c 'CREATE DATABASE compact' >/dev/null
mkdir -p /var/lib/registry
cat >/tmp/registry.yaml <<'REGISTRY'
version: 0.1
log:
  level: warn
storage:
  filesystem:
    rootdirectory: /var/lib/registry
http:
  addr: 127.0.0.1:5000
REGISTRY
/out/registry serve /tmp/registry.yaml >/tmp/registry.log 2>&1 &
export CONTROLPLANE_TEST_DATABASE_URL='postgresql://postgres:compact-test@127.0.0.1:5432/compact?sslmode=disable'
export COMPACT_BENCH_SERVER=1 COMPACT_BENCH_ENVOY=/out/envoy COMPACT_BENCH_READY_FILE=/out/stack-ready
/out/controlplane.test -test.run '^TestCompactControlPlaneResourceProbe$' -test.v -test.timeout=4m > /out/stack-probe.txt 2>&1
ctr -n compact-bench images pull docker.io/library/busybox:1.36.1 >/tmp/compact-bench-pull.txt 2>&1
export COMPACT_BENCH_SANDBOX=1
for mode in eager on-demand; do
  /usr/bin/time -f 'peak_rss_kib=%M\nuser_cpu_seconds=%U\nsystem_cpu_seconds=%S\nelapsed_seconds=%e' -o "/out/builder-${mode}-process.txt" /out/builder.test -test.run '^$' -test.bench "^BenchmarkHardenedStartup/${mode}\$" -test.benchtime 1000x > "/out/builder-${mode}-run.txt"
done
"""
(output / "start-stack.sh").write_text(fixture)
(output / "start-stack.sh").chmod(0o755)
print(f"Benchmark artifacts: {output}", file=sys.stderr)

def run(args, **kwargs):
    return subprocess.run(args, check=True, cwd=repo, **kwargs)

run(["go", "test", "-c", "-tags=integration", "./internal/controlplane", "-o", str(output / "controlplane.test")])
run(["go", "test", "-c", "./internal/builder", "-o", str(output / "builder.test")])
try:
    for image, source, target in (("registry:2", "/bin/registry", "registry"), ("envoyproxy/envoy:v1.36-latest", "/usr/local/bin/envoy", "envoy")):
        run(["docker", "create", "--name", copy_name, image], stdout=subprocess.DEVNULL)
        run(["docker", "cp", copy_name + ":" + source, str(output / target)])
        run(["docker", "rm", copy_name], stdout=subprocess.DEVNULL)
    with (output / "stack-setup.txt").open("w") as setup_log:
        process = subprocess.Popen(["docker", "run", "--rm", "--name", name, "--privileged", "--cgroupns=private", "--cpus=2", "--memory=2g", "--memory-swap=2g", "--mount", "type=volume,destination=/var/lib/containerd", "-v", str(repo)+":/src:ro", "-v", str(output)+":/out", "-w", "/src", "golang:1.25-bookworm", "bash", "/src/scripts/linux-runtime-container.sh", "/out/start-stack.sh"], stdout=setup_log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 360
        while not (output / "stack-ready").exists():
            if process.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError(f"stack failed or timed out; inspect {output}")
            time.sleep(.2)
        results = json.loads(subprocess.check_output([sys.executable, str(repo / "scripts/compact-container-sample.py"), name], text=True))
        results["limits_events_processes"] = subprocess.check_output(["docker", "exec", name, "bash", "-c", "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.peak /sys/fs/cgroup/memory.events; ps -eo pid,rss,comm"], text=True)
        (output / "stack.json").write_text(json.dumps(results, indent=2) + "\n")
        if process.wait(timeout=120):
            raise RuntimeError(f"runtime probe failed; inspect {output}")
        print(json.dumps(results, indent=2))
finally:
    subprocess.run(["docker", "rm", "--force", copy_name, name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
