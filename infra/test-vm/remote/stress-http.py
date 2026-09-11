#!/usr/bin/env python3
"""Bounded HTTP load generator, run inside a workload network namespace."""
import concurrent.futures
import json
import math
import sys
import time
import urllib.request


def run(duration, concurrency, timeout, url, marker, requests_per_worker=None):
    started = time.monotonic()
    deadline = started + duration

    def worker(_):
        histogram = {}
        requests = errors = mismatches = 0
        # Workload addresses must be contacted directly, never through a proxy.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None

        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        while (requests_per_worker is None and time.monotonic() < deadline) or (
                requests_per_worker is not None and requests < requests_per_worker):
            before = time.monotonic()
            requests += 1
            try:
                with opener.open(url, timeout=timeout) as response:
                    body = response.read(4096).decode().strip()
                    if response.status != 200 or body != marker:
                        errors += 1
                        mismatches += 1
            except Exception:
                errors += 1
            elapsed = min(60000, math.ceil((time.monotonic() - before) * 1000))
            histogram[elapsed] = histogram.get(elapsed, 0) + 1
        return requests, errors, mismatches, histogram

    requests = errors = mismatches = 0
    buckets = [0] * 60001
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        for n, e, m, histogram in pool.map(worker, range(concurrency)):
            requests += n
            errors += e
            mismatches += m
            for i, count in histogram.items():
                buckets[i] += count
    elapsed = time.monotonic() - started

    def percentile(p):
        target = math.ceil(requests * p)
        total = 0
        for i, count in enumerate(buckets):
            total += count
            if total >= target:
                return i
        return 60000

    return {
        "requests": requests, "errors": errors,
        "codes": {"wrong-content": mismatches}, "seconds": elapsed,
        "requests_per_second": requests / elapsed,
        "p50_ms": percentile(.50), "p95_ms": percentile(.95),
        "p99_ms": percentile(.99), "buckets": buckets,
    }


if __name__ == "__main__":
    if len(sys.argv) not in (6, 7):
        raise SystemExit("usage: stress-http.py DURATION CONCURRENCY TIMEOUT URL MARKER [REQUESTS_PER_WORKER]")
    request_limit = int(sys.argv[6]) if len(sys.argv) == 7 else None
    print(json.dumps(run(float(sys.argv[1]), int(sys.argv[2]),
                         float(sys.argv[3]), sys.argv[4], sys.argv[5], request_limit)))
