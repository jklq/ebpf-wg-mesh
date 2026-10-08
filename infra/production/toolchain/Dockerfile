# Build this release-owned toolchain with the verified native buildctl and
# railpack files for TARGETARCH in the build context. The packaging workflow
# supplies a digest-pinned Linux base image; no tools are fetched at runtime.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
COPY --chmod=0755 buildctl railpack /usr/local/bin/
RUN buildctl --version && railpack --version
WORKDIR /build
