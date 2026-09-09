# Platform Domain

The platform turns a project's desired application configuration into isolated, routable workloads on an operator-managed fleet.

## Organization and isolation

**Project**:
The access-control container shared by a group of users. A project contains environments but is not itself a deployment or network scope.
_Avoid_: Workspace, tenant

**Environment**:
An isolated deployment and private-network scope within a project. Services, volumes, and their private connectivity belong to exactly one environment.
_Avoid_: Stage, namespace

## Desired application

**Service**:
A named desired workload in one environment, including its source, runtime configuration, placement constraints, and routing ports.
_Avoid_: App, workload

**Service Draft**:
The mutable configuration being prepared for a service. Changing a draft does not change serving allocations.
_Avoid_: Current spec, pending spec

**Service Revision**:
An immutable snapshot of a service draft. A deployment realizes exactly one service revision. Persistence currently names its sequence `spec_revision`.

**Environment Release**:
One atomic request to apply every changed service revision in an environment. The resulting service deployments progress independently.
_Avoid_: Deploy environment, batch rollout

## Delivery and runtime

**Deployment**:
One attempt to realize a service revision, including any required build and allocation replacement work. Restart, retry, rollback, and removal are actions on deployments.
_Avoid_: Release, rollout

**Rollout**:
The internal replacement work performed for a deployment. A rollout is not a second user-visible lifecycle or history record.
_Avoid_: Deployment

**Build**:
The production of an immutable image for a deployment from an immutable source snapshot.
_Avoid_: Deployment

**Allocation**:
One concrete replica of a deployment assigned to a fleet agent. An allocation identity is never moved between agents.
_Avoid_: Instance, container

**Allocation Assignment**:
The scheduler-owned desired state for one allocation: its fleet agent, deployment, service revision, rollout generation, addresses, runtime intent, and any drain deadline.

**Allocation Observation**:
One fleet agent's durable report of an assigned allocation's applied generation and runtime state. Observations belong to an agent session and are ordered within that session. An observation for an older generation cannot establish readiness for a newer assignment.

**Agent Administration**:
Operator-owned intent for a fleet agent, including whether it is cordoned, draining, or retired. Runtime connectivity does not alter administrative intent.

**Agent Presence**:
The live owner record for a fleet agent session: its current process incarnation, last contact, readiness, and reachability. Presence is not durable operator intent.

**Fleet Agent**:
An operator-managed compute node that reconciles assigned allocations.
_Avoid_: Host, worker, node

**Deployment History**:
The durable, user-visible sequence of deployment transitions. Runtime reports may cause a transition, but do not rewrite prior transitions.

**Domain Binding**:
A public hostname attached to one service port.
_Avoid_: Domain, route
