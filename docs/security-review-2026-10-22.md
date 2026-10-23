Security review, 22 October 2026
================================

This review focused on paths from customer-controlled code and requests to
platform credentials and other customers' environment variables. It covered
source extraction, builds, image resolution and pulls, workload networking,
console authentication, workspace authorization, API tokens, secret encryption,
and dependency advisories. Changes already present in the working tree were
preserved. Severity below is an assessment of potential impact, not a CVSS score.

Findings fixed
--------------

1. **High: source archives could escape the builder's repository directory.**
   Lexical path checks accepted a symlink to `.` followed by another link to
   `alias/../outside`. Later archive links could then write outside the repository
   or hardlink a host file into it. Tests reproduced both effects before the fix.
   Extraction now uses `os.Root` for all filesystem operations and validates the
   completed link graph before build tools can read it. Internal links remain
   supported. See `internal/builder/app_extract.go` and
   `internal/builder/archive_security_test.go`.

2. **High: image pulls could reach private platform destinations.**
   Digest-pinned references returned before the control plane checked the
   registry destination. Tests reproduced acceptance of loopback, private, and
   metadata addresses. Runtime pulls also used an unrestricted HTTP transport.
   The destination check now covers pinned references. Runtime pulls validate
   and pin DNS answers at each connection, including token requests and layer
   redirects, and disable ambient proxies. Pull credentials are returned only
   for the assigned registry host. Private direct-image registries require the
   operator allowlist on both the control plane and agents; private token
   authorities need their own agent entry. See `.env.example`,
   `internal/controlplane/registry/resolve.go`, and
   `internal/agent/runtime_containerd_engine_linux.go`.

3. **High on cloud hosts: workloads could reach cloud credential endpoints.**
   Destinations outside the workload address pools took the generic egress path.
   Metadata endpoints now receive a deny identity in the existing eBPF catalog.
   Builds and workloads share the endpoint list, including AWS instance, ECS,
   and EKS credentials, Alibaba metadata, and GCP IPv6 metadata. Policy tests
   verify the denial entries. This is a specific endpoint denial; it does not
   isolate every private or management network.

4. **Medium: GitHub OAuth credentials appeared in request URLs.**
   Token exchanges put the client secret, authorization code, or refresh token in
   query parameters, exposing them to systems that record request URLs. They now
   use form-encoded POST bodies and refuse redirects. Tests cover both exchange
   and refresh requests.

5. **Critical/high dependency advisories affected the console and runtime code.**
   The initial scans reported 25 reachable Go advisories and console advisories
   including unauthenticated reflected XSS and unsafe deserialization. Updated
   the Go minimum to 1.27.3, moved the containerd client to 2.3.6, and updated
   affected console and Go dependencies. Adapted the OCI PID limit, introspection,
   and server-function validation APIs and kept the Stripe handler server-only.
   CI now runs Go and Bun vulnerability checks.

Validation
----------

- `go test ./...` and `go vet ./...`.
- Race checks for builder, registry, identity, and secret-key packages.
- PostgreSQL integration tests for workspace role and membership checks, API
  tokens, bootstrap-token binding and revocation, encrypted environment-variable
  storage, desired-state delivery, rollback, and environment duplication.
- All 354 console unit tests, TypeScript checking, and production build.
- `govulncheck`: zero reachable advisories. It still reports an advisory for the
  uncalled `golang.org/x/crypto/openpgp` package in a required module.
- `bun audit`: zero advisories in the updated lockfile.

Limits and deployment requirements
----------------------------------

The privileged containerd, network-namespace, and eBPF enforcement tests could not
run in this unprivileged session. Run the privileged CI job before deployment.
Updating the Go client does not update host binaries: provision patched native
containerd and runc artifacts. The example production release now declares
containerd 2.3.6; its placeholder artifacts must be replaced with real verified
artifacts before use.

Environment variables use per-environment AES-GCM keys, with ciphertext bound to
the service revision. The control plane holds the root keyring and can decrypt
secrets. A compromise of that process or its host can therefore still expose
secrets. Protect that boundary and isolate management services from customer
workloads. The audit is evidence about the inspected paths, not proof that the
application has no vulnerabilities or that a Railway-style breach is impossible.

Primary references: [Railway disclosure](https://blog.railway.com/p/security-disclosure-wednesday-october-21st),
[TanStack XSS advisory](https://github.com/TanStack/router/security/advisories/GHSA-qx66-fv34-fjm8),
[containerd image-pull advisory](https://pkg.go.dev/vuln/GO-2026-6597),
[AWS credential endpoints](https://docs.aws.amazon.com/sdkref/latest/guide/feature-container-credentials.html),
[GCP metadata endpoints](https://docs.cloud.google.com/compute/docs/metadata/overview).
