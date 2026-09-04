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

**Fleet Agent**:
An operator-managed compute node that reconciles assigned allocations.
_Avoid_: Host, worker, node

**Domain Binding**:
A public hostname attached to one service port.
_Avoid_: Domain, route
