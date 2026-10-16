# Platform Domain

The platform turns a project's desired application configuration into isolated, routable workloads on an operator-managed fleet.

## Organization and isolation

**Workspace**:
The access-control container for a group of people and the projects they share. Membership and its role (admin, member, or viewer) apply to every project in the workspace. Usage and billing are accounted per workspace.
_Avoid_: Organization, team, tenant

**Personal Workspace**:
The workspace every user has from their first sign-in, named "My Projects" by default. It has exactly one member and cannot be deleted, left, or shared; collaboration happens in other workspaces.

**Workspace Invitation**:
A pending offer of a workspace role to whoever signs in with a given email address. Accepting it creates the membership.

**Project**:
A named group of environments in one workspace. A project is not itself an access-control, deployment, or network scope; it can move between workspaces.
_Avoid_: App

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
One fleet agent's durable report of an assigned allocation's applied generation and runtime state. Observations belong to the durable agent identity and are ordered across connection sessions. A session fences transport from older process incarnations; it does not reset local observation state. An observation for an older generation cannot establish readiness for a newer assignment.

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

## Billing

**Billing Account**:
The payer for exactly one workspace, holding its plan, payment details, credits, and budget. A resource's payer is fixed when the resource is assigned; managed platform projects have no billing account.
_Avoid_: Customer, subscription

**Billing Period**:
One monthly charge interval of a billing account, anchored on the account's creation day, with its minimum, included usage, and prices fixed at its start.

**Statement**:
The frozen bill for one billing period: exact quantities and prices, allowance, credits, and the amount due. Corrections are new credit entries, never edits to a statement.
_Avoid_: Invoice (the provider document that collects a statement)

**Billing Restriction**:
A durable limit on a workspace's execution derived from its billing state: growth blocked, or compute stopped. Desired state survives it; it is neither deletion nor agent administration.

## Platform operation

**Platform Release**:
One immutable version of the platform's programs and their compatibility contract.
It is independent of an environment release or an application deployment.

**Platform Upgrade**:
One controlled transition of an installation from its applied platform release to
another platform release.

**Alarm**:
One message the platform sends to the operator when automatic operation fails
with service at risk. An alarm names the failed operation and the next
operator action.
_Avoid_: Alert, notification
