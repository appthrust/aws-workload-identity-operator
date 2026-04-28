# API Reference

This page summarizes the public API in
`aws.identity.appthrust.io/v1alpha1`. The CRD schemas remain the source of
truth for validation details:

- [`AWSWorkloadIdentityOperatorConfig`](../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml)
- [`AWSWorkloadIdentityConfig`](../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml)
- [`AWSServiceAccountRole`](../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml)
- [`AWSServiceAccountRoleReplicaSet`](../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml)

## Resource Summary

| Kind | Scope | Required name | Purpose |
| --- | --- | --- | --- |
| `AWSWorkloadIdentityOperatorConfig` | Cluster | `default` | Platform-wide defaults such as the IAM permissions boundary and self-hosted webhook namespace. |
| `AWSWorkloadIdentityConfig` | Namespace | `default` | Target-cluster delivery configuration for one workload namespace. |
| `AWSServiceAccountRole` | Namespace | User selected | Binding from one remote Kubernetes `ServiceAccount` to one generated IAM role. |
| `AWSServiceAccountRoleReplicaSet` | Namespace | User selected | OCM Placement fan-out that creates one `AWSServiceAccountRole` child per selected cluster namespace. |

The workload namespace is the target-cluster boundary. With OCM, the operator
resolves `ClusterProfile` objects by the
`open-cluster-management.io/cluster-name=<namespace>` label.

## AWSWorkloadIdentityOperatorConfig

Cluster-scoped platform configuration. Create exactly one object named
`default`.

| Field | Required | Mutable | Notes |
| --- | --- | --- | --- |
| `metadata.name` | Yes | No | Must be `default`. |
| `spec.permissionsBoundaryARN` | No | Yes | IAM permissions boundary ARN applied to generated workload roles. |
| `spec.selfHostedIRSA.webhookNamespace` | No | No | Remote namespace for the self-hosted webhook runtime. Defaults to `aws-pod-identity-webhook`. |

This kind has no status subresource. Controllers read it as input only.

## AWSWorkloadIdentityConfig

Namespace-scoped target-cluster configuration. Create one object named
`default` in each workload namespace.

| Field | Required | Mutable | Notes |
| --- | --- | --- | --- |
| `metadata.name` | Yes | No | Must be `default`. |
| `spec.type` | Yes | No | `SelfHostedIRSA`, `EKSIRSA`, or `EKSPodIdentity`. |
| `spec.region` | Yes | No | AWS region for generated resources and STS fallback. |
| `spec.eksIRSA` | Only for `EKSIRSA` | No | Must be present exactly when `spec.type` is `EKSIRSA`. |
| `spec.eksIRSA.issuerURL` | For `EKSIRSA` | No | Canonical EKS OIDC issuer URL. |
| `spec.eksIRSA.oidcProvider.management` | For `EKSIRSA` | No | `Managed` creates an ACK IAM provider CR. `External` uses a supplied provider ARN. |
| `spec.eksIRSA.oidcProvider.arn` | For `External` | No | Required for `External`, forbidden for `Managed`; the ARN path must match `issuerURL`. |

Status fields:

| Field | Meaning |
| --- | --- |
| `status.observedGeneration` | Last reconciled object generation. |
| `status.conditions` | Readiness and deletion state. See [Status Conditions](status-conditions.md#awsworkloadidentityconfig). |
| `status.ackResources` | Status copied from operator-owned ACK children. See [ACK Resource Status](#ack-resource-status). |
| `status.selfHostedIssuer.bucketName` | Current desired self-hosted issuer S3 bucket name. |
| `status.selfHostedIssuer.publication` | Last self-hosted issuer S3 object publication identity verified by the controller. Empty until S3 objects are verified. |
| `status.selfHostedIssuer.publication.bucketName` | Bucket where the verified self-hosted issuer objects were observed. |
| `status.selfHostedIssuer.publication.issuerURL` | HTTPS issuer URL rendered into the verified discovery document. |
| `status.selfHostedIssuer.publication.signingKeyID` | Signing key ID rendered into the verified JWKS document. |
| `status.selfHostedIssuer.publication.objectSetDigest` | `sha256:<hex>` digest of the verified discovery and JWKS object set. |
| `status.issuerHostPath` | Host and path for the public issuer URL. |
| `status.oidcProviderARN` | IAM OIDC provider ARN used by generated trust policies. |
| `status.resolvedClusterName` | Latest ready multicluster-runtime cluster identifier resolved for annotation-based IRSA delivery (`SelfHostedIRSA` and `EKSIRSA`). Empty for `EKSPodIdentity`. |
| `status.webhookRuntimeNamespace` | Remote namespace where the self-hosted webhook runtime is installed. |
| `status.webhookRuntimeCertNotAfter` | Expiration timestamp for the self-hosted webhook serving certificate. |

## AWSServiceAccountRole

Namespace-scoped binding from one remote Kubernetes `ServiceAccount` to one IAM
role.

| Field | Required | Mutable | Notes |
| --- | --- | --- | --- |
| `spec.serviceAccount.namespace` | Yes | No | Remote ServiceAccount namespace. |
| `spec.serviceAccount.name` | Yes | No | Remote ServiceAccount name. |
| `spec.policyARNs` | Exactly one of `policyARNs` or `policyDocument` | Yes | Up to 10 IAM managed policy ARNs attached to the generated role. |
| `spec.policyDocument` | Exactly one of `policyARNs` or `policyDocument` | Yes | Inline IAM policy document as a JSON object. The operator serializes it to compact JSON, enforces AWS's 6144-character customer-managed policy size limit, and creates an ACK IAM Policy for it. |

Status fields:

| Field | Meaning |
| --- | --- |
| `status.observedGeneration` | Last reconciled object generation. |
| `status.conditions` | Input, AWS, delivery, and aggregate readiness state. See [Status Conditions](status-conditions.md#awsserviceaccountrole). |
| `status.ackResources` | Status copied from operator-owned ACK Role, Policy, or PodIdentityAssociation children. |
| `status.roleARN` | Generated IAM role ARN. |
| `status.generatedPolicyARN` | Generated IAM policy ARN when `spec.policyDocument` is used. |
| `status.deliveryType` | Last resolved delivery strategy. Used during deletion if the config was force-deleted. |
| `status.resolvedClusterName` | Last ready multicluster-runtime cluster identifier used for remote ServiceAccount annotation cleanup on annotation-based IRSA delivery (`SelfHostedIRSA` and `EKSIRSA`). Empty for `EKSPodIdentity`. |

## AWSServiceAccountRoleReplicaSet

Namespace-scoped fleet binding. It resolves same-namespace OCM `Placement`
objects and creates one `AWSServiceAccountRole` child in each selected cluster
namespace.

| Field | Required | Mutable | Notes |
| --- | --- | --- | --- |
| `spec.placementRefs` | Yes | Yes | One to 16 same-namespace OCM `Placement` refs. Multiple refs are unioned by cluster identity. |
| `spec.placementRefs[].name` | Yes | Yes | OCM `Placement` name. |
| `spec.placementRefs[].rolloutStrategy` | No | Yes | OCM rollout strategy. Defaults to `type: All`. |
| `spec.template.metadata.labels` | No | Yes | Labels copied to generated children. Operator-reserved label keys are rejected. |
| `spec.template.metadata.annotations` | No | Yes | Annotations copied to generated children. Operator-reserved annotation keys are rejected. |
| `spec.template.spec` | Yes | Partly | `AWSServiceAccountRoleSpec` copied to children. `template.spec.serviceAccount` is immutable. |

Status fields:

| Field | Meaning |
| --- | --- |
| `status.observedGeneration` | Last reconciled object generation. |
| `status.selectedClusterCount` | Clusters selected by resolved PlacementDecision objects before rollout gating. |
| `status.desiredClusterCount` | Clusters that should currently have children after rollout gating. |
| `status.appliedClusterCount` | Children successfully applied. |
| `status.readyClusterCount` | Children reporting `Ready=True`. |
| `status.staleClusterCount` | Previously applied clusters no longer in the desired set. |
| `status.conflictCount` | Desired children blocked by foreign same-name objects or ownership mismatch. |
| `status.failureCount` | Failed or timed-out cluster entries. Conflicts are tracked separately by `status.conflictCount`. |
| `status.placements` | Per-Placement selected count, available OCM decision groups, rollout summary, and conditions. |
| `status.rollout` | Aggregate OCM rollout summary. See [ReplicaSet Rollout Status](#replicaset-rollout-status). |
| `status.failedClusters` | Bounded list of failed cluster fan-out paths. |
| `status.clusters` | Bounded per-cluster child summary. |
| `status.conditions` | Placement, child apply, child readiness, cleanup, and aggregate readiness. |

ReplicaSet controllers do not write child role status. Each generated
`AWSServiceAccountRole` remains status-owned by the role controller in its
target namespace.

## ACK Resource Status

`status.ackResources` is a copied view of operator-owned ACK child status. It
lets operators inspect AWS reconciliation without discovering generated child
names manually.

| Field | Meaning |
| --- | --- |
| `apiVersion` | ACK API group and version. |
| `kind` | ACK kind such as `Role`, `Policy`, `Bucket`, `OpenIDConnectProvider`, or `PodIdentityAssociation`. |
| `namespace` | Hub namespace containing the ACK CR. |
| `name` | ACK CR name. |
| `conditions[]` | ACK condition type, status, transition time, reason, and message. |

ACK CRs remain the source of truth for AWS-side create, update, and delete
operations. Delete AWS-owned resources by deleting the ACK CRs, not by editing
AWS directly.

## ReplicaSet Rollout Status

`status.rollout` and `status.placements[].rollout` mirror OCM rollout progress.

| Field | Meaning |
| --- | --- |
| `total` | Total clusters considered by the rollout plan. |
| `updating` | Clusters currently allowed to update and not yet complete. |
| `succeeded` | Clusters whose generated child role is ready for the current generation. |
| `failed` | Clusters with apply, conflict, or readiness failures. |
| `timedOut` | Clusters that exceeded rollout timeout. |
| `conditions` | Rollout conditions, including `Progressing`. |

Per-cluster summaries use phases `Pending`, `Ready`, `Conflict`, `Failed`, and
`TimedOut`.
