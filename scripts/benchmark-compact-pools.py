#!/usr/bin/env python3
"""Run the compiled pool probe and sample one disposable PostgreSQL container.
CONTROLPLANE_TEST_DATABASE_URL must point at that container. Pass TEST_BINARY CONTAINER.
"""
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import time

sampler = pathlib.Path(__file__).with_name("compact-container-sample.py")
results = {}
with tempfile.TemporaryDirectory() as directory:
    for mode in ("legacy", "compact"):
        marker = pathlib.Path(directory) / mode
        env = os.environ | {"COMPACT_BENCH_POOL": mode, "COMPACT_BENCH_READY_FILE": str(marker)}
        command = subprocess.Popen([sys.argv[1], "-test.run", "^TestCompactPoolResourceProbe$", "-test.v"],
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=env)
        try:
            deadline = time.monotonic() + 30
            while not marker.exists():
                if command.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("pool probe failed or timed out: " + command.communicate()[0])
                time.sleep(0.1)
            sample = json.loads(subprocess.check_output([sys.executable, str(sampler), sys.argv[2]], text=True))
            output, _ = command.communicate(timeout=30)
            if command.returncode:
                raise RuntimeError(output)
            results[mode] = {"sample": sample, "probe": output}
        finally:
            if command.poll() is None:
                command.terminate()
                command.wait(timeout=10)
print(json.dumps(results, indent=2))
