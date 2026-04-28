# Documentation

This directory is the primary documentation entry point. The root
[README](../README.md) is the project front door; detailed procedures and
reference material live here.

## New Users

| I want to... | Read |
| --- | --- |
| Understand the shortest happy path | [Quickstart](quickstart.md) |
| Understand the resource model | [Architecture](concepts/architecture.md) |
| Choose `SelfHostedIRSA` or `EKSPodIdentity` | [Delivery Types](concepts/delivery-types.md) |
| Understand the security boundaries | [Security Model](concepts/security-model.md) |

## Platform Operators

| I want to... | Read |
| --- | --- |
| Install the operator | [Install With Helm](guides/install-helm.md) |
| Configure Cluster Inventory and OCM access | [Cluster Inventory And OCM](concepts/cluster-inventory-and-ocm.md) |
| Configure cluster-wide defaults | [Configure Platform Defaults](guides/configure-platform-defaults.md) |
| Bind a workload ServiceAccount | [Bind A ServiceAccount](guides/bind-service-account.md) |
| Fan out bindings with OCM Placement | [Fleet Bindings](guides/fleet-bindings.md) |
| Restrict requested IAM permissions | [Restrict IAM Policy Inputs](guides/restrict-iam-policy-inputs.md) |

## Integrators

| I want to... | Read |
| --- | --- |
| Use a remote ServiceAccount identity from a hub-side process | [Hub-Side Remote IRSA Consumers](guides/remote-irsa-consumers.md) |

## Reference

| I want to... | Read |
| --- | --- |
| Review API objects | [API Reference](reference/api.md) |
| Review controller ownership | [Resource Ownership](concepts/resource-ownership.md) |
| Understand controller behavior | [Operator Behavior](reference/operator-behavior.md) |
| Debug readiness | [Status Conditions](reference/status-conditions.md) |
| Review deletion ordering | [Deletion Behavior](reference/deletion-behavior.md) |
| Scope AWS permissions | [IAM Permissions](reference/iam-permissions.md) |
| Review chart values | [Helm Values](reference/helm-values.md) |
| Review metrics | [Metrics](reference/metrics.md) |

## Operations

| I want to... | Read |
| --- | --- |
| Configure metrics and logs | [Observability](operations/observability.md) |
| Triage common failures | [Troubleshooting](operations/troubleshooting.md) |
| Delete resources safely | [Cleanup And Force Delete](operations/cleanup-and-force-delete.md) |
| Plan disruptive changes | [Upgrades](operations/upgrades.md) |
