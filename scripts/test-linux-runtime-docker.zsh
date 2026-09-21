#!/usr/bin/env zsh
# Runs the Linux-only agent/firewall runtime tests inside a privileged Linux
# container. These tests need root, containerd, CNI and BPF/TCX, so on a
# non-Linux host they otherwise skip themselves and prove nothing.
#
# Usage: scripts/test-linux-runtime-docker.zsh [go test args...]
#   defaults to the same packages as `make test-linux-runtime`.

emulate -L zsh
setopt errexit nounset pipefail

cd "${0:A:h}/.."

if ! docker info >/dev/null 2>&1; then
	print -u2 "docker is not available; start Docker and retry"
	exit 1
fi

local -a test_cmd
if (( $# > 0 )); then
	test_cmd=("$@")
else
	test_cmd=(bash -c 'go test -count=1 -timeout 20m ./internal/agent && go test -count=1 -timeout 10m ./internal/builder && go test -count=1 -timeout 10m ./internal/firewall')
fi

# Cache the module and build caches across runs; a cold run recompiles the world.
docker volume create ebpf-wg-mesh-gobuild >/dev/null
docker volume create ebpf-wg-mesh-gomod >/dev/null

exec docker run --rm --privileged --cgroupns=private \
	-v "$PWD":/src \
	-v ebpf-wg-mesh-gobuild:/root/.cache/go-build \
	-v ebpf-wg-mesh-gomod:/go/pkg/mod \
	-w /src \
	golang:1.25-bookworm \
	bash /src/scripts/linux-runtime-container.sh "${test_cmd[@]}"
