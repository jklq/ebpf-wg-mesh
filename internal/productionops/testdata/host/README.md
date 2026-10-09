This image runs the real SSH and systemd services used by the opt-in fleet
integration. It is a local test host, with containerd and CNI plugins installed.
The test creates its own network, host keys, TLS roots, object stores and private
host data, and removes only those resources when it finishes.

Build it with:

```sh
docker build -t platform-operations-fixture/host:r43 internal/productionops/testdata/host
```

Provide a complete amd64 release built by `scripts/package-production-release.py`,
the verified builder toolchain OCI layout, and the native MinIO, mc and registry
executables. Then run from the repository root:

```sh
PRODUCTION_FLEET_INTEGRATION=1 \
PRODUCTION_RELEASE_DIRECTORY=/path/to/packaged-release \
PRODUCTION_TOOLCHAIN_OCI=/path/to/toolchain-oci \
MINIO_BINARY=/path/to/minio \
MINIO_CLIENT_BINARY=/path/to/mc \
REGISTRY_BINARY=/path/to/registry \
AWS_BINARY=/path/to/aws \
go test -tags=integration ./internal/productionops \
  -run 'TestNativeSSHSystemd(Production|Recovery|SiteLoss)Fleet$' -count=1 -v -timeout=185m
```

The Docker daemon must support privileged containers, private cgroup namespaces,
IPv6 bridge networks, systemd and the native Linux runtime. The fixture needs
`/dev/shm` capacity for the release, protected artifact copies and database data;
it leaves unrelated Docker resources untouched. The Dockerfile base is pinned.
Native input checksums and image digests come from the packaged release.

The production scenario covers installation, failed upgrade, interruption after
resume and host failure. The recovery and site-loss scenarios each perform a real fresh installation,
then a surviving-fleet restore or complete-site-loss restore. Select
a test by name when investigating a fault without repeating the other.

Measured timings cover this isolated machine and data set. Declared fixture
failure domains and separately configured services do not prove physical
provider/account independence, production throughput or an RPO/RTO guarantee.
