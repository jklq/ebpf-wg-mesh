#!/usr/bin/env python3
"""Measure container working-set RAM and CPU time, without Docker's percent rounding.
Run: python3 scripts/compact-container-sample.py NAME [NAME ...] > results.json
Only samples named existing containers. Does not start or change them.
"""
import json
import statistics
import subprocess
import sys
import time


def sample(name):
    raw = subprocess.check_output([
        "docker", "exec", name, "cat", "/sys/fs/cgroup/cpu.stat",
        "/sys/fs/cgroup/memory.stat", "/sys/fs/cgroup/memory.current",
    ], text=True).splitlines()
    counters = dict(line.split() for line in raw[:-1])
    return {
        "cpu_us": int(counters["usage_usec"]),
        "working_set_bytes": int(raw[-1]) - int(counters["inactive_file"]),
    }


results = {}
for name in sys.argv[1:]:
    measurements = []
    for _ in range(3):
        start = sample(name)
        at = time.monotonic()
        time.sleep(5)
        end = sample(name)
        elapsed = time.monotonic() - at
        measurements.append({
            "ram_mib": end["working_set_bytes"] / 2**20,
            "cpu_seconds": (end["cpu_us"] - start["cpu_us"]) / 1e6,
            "elapsed_seconds": elapsed,
            "cpu_percent_one_core": (end["cpu_us"] - start["cpu_us"]) / elapsed / 10000,
        })
    results[name] = {"samples": measurements,
                     "median_ram_mib": statistics.median(m["ram_mib"] for m in measurements),
                     "median_cpu_percent_one_core": statistics.median(m["cpu_percent_one_core"] for m in measurements)}
print(json.dumps(results, indent=2))
