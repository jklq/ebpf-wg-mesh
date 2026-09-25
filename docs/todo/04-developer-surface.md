# 4 — Multi-service apps

A typical app is more than one service: a web process, a worker, a database, a migration step. These items make that shape work without copying hostnames or running migrations by hand. 4.2 is in the design-partner gate.

Config-as-code (4.1), monorepo watch paths (4.3), and PR environments (4.5) are parked in [freeze.md](freeze.md).

## 4.2 Reference variables

Was: 8.7
Status: open
Depends on: 1.2 so referenced secrets stay expressions, not plaintext copies. Volume mount-path references wait for 6.1.

Prompt:

```text
Let a variable reference another service's variables in the same environment, e.g. ${{postgres.DATABASE_URL}}, and provide platform variables every service gets: its private hostname, port, public domain if any, and project/environment/service names. Resolve references when a deployment is assembled, store the expression (not the resolved value) in configuration, and never copy sealed secret plaintext into the referencing service's config or API output. A missing target or a cycle fails the deploy with an error naming the reference. Test cross-service references, a reference to a sealed secret, a missing target, a cycle, and rollback. Do not add cross-environment references, staged dependent changes on rename, or replica/region metadata.
```

## 4.4 Pre-deploy jobs

Was: 8.6
Status: open
Depends on: 2.1.

Prompt:

```text
Add an optional pre-deploy command per service, run once from the newly built image with the service's variables and no public ingress or volume. Success lets the rollout proceed; failure or timeout leaves the running deployment untouched and shows the job's logs and exit code on the deployment. Never retry automatically, because migrations may not be idempotent. A control-plane restart during the job must not run it twice. Test success, failure, timeout, and cancellation.
```
