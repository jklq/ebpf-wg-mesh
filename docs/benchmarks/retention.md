# Retention storage measurements

Measured on Linux amd64, Go 1.27.1, PostgreSQL 17, and `registry:2`. Three runs
per probe; times below are medians. Raw results: [retention.json](retention.json).

| Probe | Before cleanup | After cleanup | Disk reduction | Cleanup time |
| --- | ---: | ---: | ---: | ---: |
| Source for 20 completed builds | 40.12 MiB | 0 | 100% | 0.24 s |
| 20 image versions, retain current and previous | 21.01 MiB | 3.00 MiB | 85.7% | 1.22 s |

The source probe uses the real SQLStore and file archive backend. It simulates
build completion, with no active retries left, then advances the collection
clock beyond staging protection. Its 40 objects include 20 source archives
with random payloads and 20 small seed objects. The collector removes the
objects from storage while preserving commit metadata. Build execution is
simulated; these times measure cleanup, not compilation.

The image probe uses 20 distinct build repositories, a shared 1 MiB layer, and
a unique 1 MiB layer per version. It deletes 18 manifests using the Distribution
API, stops the registry, runs blob collection, and restarts it. It checks that
both retained manifests still load. The payloads are synthetic; this probe
does not run applications. The time includes stopping and restarting
the container. This measures registry reclamation; the separate integration
tests verify the control plane's selection and deletion queue.

Manifest deletion alone left **all 21.01 MiB of blobs on disk**. Blob collection
is necessary for the measured saving. Shared layers explain why removing 90%
of versions saves 85.7% of bytes in this sample. The actual saving depends on
image sizes and how much content they share. Registry upload remnants and
operator toolchain images have separate storage lifecycles.

These changes bound retained disk data. They do not establish another reduction
in idle RAM or CPU. Cleanup consumes CPU and disk I/O; Go or containerd may keep
freed memory available for reuse. The earlier RAM and CPU measurements remain
in [compact.md](compact.md).

The updated stack also passed the 2 GiB / two-CPU / no-swap smoke test with
PostgreSQL, file logs, a registry, Envoy, containerd, and a service limited to
256 MiB with 192 MiB of touched application data. Median working-set RAM was
**384.2 MiB**, median idle CPU was **0.67% of one core**, and the cgroup recorded
no OOM events. The cgroup peak was 1,060.6 MiB including setup. The control plane
and agent share one test process; the console UI and a separate VM kernel are
excluded. This does not establish a safe VM size below 2 GiB. The difference
from the earlier 390 MiB run is too small to attribute to retention changes.

Reproduce that smoke test with:

```sh
python3 scripts/benchmark-compact-stack.py /tmp/retention-compact-probe
```

Use a new output directory. Raw samples from this run are included in
[retention.json](retention.json).

Reproduce source cleanup against a disposable PostgreSQL instance:

```sh
RETENTION_STORAGE_PROBE=1 \
CONTROLPLANE_TEST_DATABASE_URL='postgresql://postgres:PASSWORD@127.0.0.1:15432/compact?sslmode=disable' \
go test -tags=integration ./internal/controlplane \
  -run '^TestSourceRetentionStorageProbe$' -v -count=3 -timeout=5m
```

The fixture creates its own database. Reproduce registry reclamation with
Docker available:

```sh
python3 scripts/benchmark-image-retention.py
```

The script creates and removes its own container and temporary registry data.
It uses anonymous auth for the disk probe. Separate tests verify the
collector's signed delete scope, namespace restriction, failed deletion
retries, shared images, exact historical source fetches, and helpful failures.

The compact 2 GiB VM can therefore retain one current image per service and a
small rollback window without accumulating every historical image or source
archive. Keep disk space for queued build inputs, active uploads, current
images, PostgreSQL, and backups. Large queues and simultaneous rollouts still
need space for their active work.

Further gains are possible by inspecting registry repositories for images uploaded
by a builder that stopped before reporting completion, bounding stale upload
sessions, limiting the builder's operator toolchain cache, and archiving old
metadata. Retained images could also be reused by commit and recipe before
downloading source again. Those changes need separate ownership and cleanup rules; they are not included
in the measured reclaimed bytes. Removing unused image roots also lets containerd
reclaim snapshots; the actual layer saving depends on what live containers share.
