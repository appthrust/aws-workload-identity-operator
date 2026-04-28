# API Reference

This page records the API surface at a conceptual level. Field-level schema is
owned by the CRD manifests:

- [`AWSWorkloadIdentityOperatorConfig`](../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml)
- [`AWSWorkloadIdentityConfig`](../../config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml)
- [`AWSServiceAccountRole`](../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml)
- [`AWSServiceAccountRoleReplicaSet`](../../config/crd/bases/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml)

## AWSWorkloadIdentityOperatorConfig

Cluster-scoped platform configuration named `default`.

Key fields:

- `spec.permissionsBoundaryARN`: optional IAM permissions boundary applied to
  generated workload roles.
- `spec.selfHostedIRSA.webhookNamespace`: remote namespace for the
  self-hosted pod identity webhook runtime.

## AWSWorkloadIdentityConfig

Namespace-scoped target-cluster identity configuration named `default`.

Key fields:

- `spec.type`: `SelfHostedIRSA` or `EKSPodIdentity`.
- `spec.region`: AWS region for generated resources and STS fallback.

The namespace maps to the target cluster. For OCM, the namespace matches the
`open-cluster-management.io/cluster-name` label value on the resolved
`ClusterProfile`.

## AWSServiceAccountRole

Namespace-scoped binding from one remote Kubernetes `ServiceAccount` to one
generated IAM role.

Key fields:

- `spec.serviceAccount.namespace`
- `spec.serviceAccount.name`
- `spec.policyARNs`
- `spec.policyDocument`

## AWSServiceAccountRoleReplicaSet

Namespace-scoped fleet binding that resolves OCM `Placement` references and
creates one `AWSServiceAccountRole` child per selected cluster namespace.

Key fields:

- `spec.placementRefs`
- `spec.template.spec`

ReplicaSet child status remains owned by each generated
`AWSServiceAccountRole`; the ReplicaSet does not write child role status.
