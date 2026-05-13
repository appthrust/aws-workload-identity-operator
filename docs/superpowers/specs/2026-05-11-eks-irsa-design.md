# EKS Native IRSA Delivery Design

Date: 2026-05-11

## Context

The operator currently supports two delivery types:

- `SelfHostedIRSA`, which creates a self-hosted S3-backed OIDC issuer, installs a remote aws-pod-identity-webhook runtime, writes remote ServiceAccount annotations, and lets workloads or hub-side consumers use `AssumeRoleWithWebIdentity`.
- `EKSPodIdentity`, which creates EKS Pod Identity ACK resources and does not use IRSA.

Hub-side remote IRSA consumers need `AssumeRoleWithWebIdentity` even when the target cluster is EKS. EKS also already has a native OIDC issuer and IRSA annotation contract, so EKS should not need the self-hosted issuer or self-hosted webhook runtime.

## Goals

- Add `EKSIRSA` as a first-class delivery type.
- Support normal EKS workload Pods through remote ServiceAccount annotations.
- Support hub-side remote IRSA consumers through `pkg/remoteirsa`.
- Allow either operator-managed or externally managed IAM OIDC providers.
- Keep Cluster Inventory focused on target cluster access and facts; put EKS IRSA delivery inputs in `AWSWorkloadIdentityConfig.spec`.
- Preserve existing deletion safety rules and owner-controller status ownership.

## Non-Goals

- Do not merge EKS IRSA into `EKSPodIdentity`.
- Do not auto-detect EKS OIDC issuer URLs from Cluster Inventory in this change.
- Do not read Cluster API `Cluster` objects or remote kubeconfig Secrets.
- Do not publish AWS credentials through Cluster Inventory access providers.

## API

Add a delivery type:

```yaml
spec:
  type: EKSIRSA
  region: ap-northeast-1
  eksIRSA:
    issuerURL: https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE
    oidcProvider:
      management: Managed
```

External provider example:

```yaml
spec:
  type: EKSIRSA
  region: ap-northeast-1
  eksIRSA:
    issuerURL: https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE
    oidcProvider:
      management: External
      arn: arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE
```

New API structs:

- `AWSWorkloadIdentityConfigSpec.EKSIRSA *EKSIRSAConfig`
- `EKSIRSAConfig.IssuerURL string`
- `EKSIRSAConfig.OIDCProvider EKSIRSAOIDCProviderConfig`
- `EKSIRSAOIDCProviderConfig.Management OIDCProviderManagement`
- `EKSIRSAOIDCProviderConfig.ARN string`

`EKSIRSA` must be a pointer field so `spec.eksIRSA` can be truly absent when the delivery type is not `EKSIRSA`.

New enum values:

- `DeliveryTypeEKSIRSA = "EKSIRSA"`
- `OIDCProviderManagementManaged = "Managed"`
- `OIDCProviderManagementExternal = "External"`

## Validation

Use CRD validation and CEL to enforce the ownership contract:

- `spec.type` enum includes `SelfHostedIRSA`, `EKSPodIdentity`, and `EKSIRSA`.
- `spec.eksIRSA` is required when `spec.type == "EKSIRSA"`.
- `spec.eksIRSA` is forbidden when `spec.type != "EKSIRSA"`.
- `spec.eksIRSA` is immutable after creation.
- `spec.eksIRSA.issuerURL` is required and must be a canonical HTTPS issuer URL without query, fragment, or trailing slash.
- `spec.eksIRSA.oidcProvider.management` is required and must be `Managed` or `External`.
- `management == "Managed"` forbids `arn`.
- `management == "External"` requires `arn`.
- External provider `arn` must match IAM OIDC provider ARN shape.

Use CEL for the `type`/`eksIRSA` presence and `management`/`arn` ownership rules. The controller should also treat invalid normalized issuer input as `Ready=False` with reason `InvalidSpec`, so status remains useful if an older CRD allowed bad input. For `External`, the controller must verify that the OIDC provider ARN path matches the normalized issuer host/path before using it in trust policies.

## Config Controller

`SelfHostedIRSA` keeps the existing behavior:

- create signing Secret,
- create ACK S3 Bucket,
- publish OIDC discovery and JWKS objects with the AWS S3 API,
- create ACK IAM `OpenIDConnectProvider`,
- apply remote self-hosted webhook runtime.

`EKSPodIdentity` keeps the existing behavior:

- no issuer resources,
- no self-hosted webhook runtime,
- config issuer/runtime conditions are `True/NotRequired`.

`EKSIRSA` adds a new config path:

- normalize `spec.eksIRSA.issuerURL` into `status.issuerHostPath` by removing the `https://` scheme and any trailing slash,
- never create signing Secret, S3 Bucket, OIDC objects, or self-hosted webhook runtime,
- for `Managed`, create an ACK IAM `OpenIDConnectProvider` using the EKS issuer URL and STS audience,
- for `Managed`, set `status.oidcProviderARN` from ACK resource metadata only,
- for `External`, do not create ACK `OpenIDConnectProvider`,
- for `External`, set `status.oidcProviderARN` from `spec.eksIRSA.oidcProvider.arn`,
- for `External`, fail readiness with `InvalidSpec` if the ARN provider path does not match `issuerURL`,
- set `IAMProviderReady=True` when the managed provider is ACK synced with an ARN, or immediately for external providers,
- set `IssuerReady=True` when `issuerHostPath` and `oidcProviderARN` are available.

`status.ackResources` for `EKSIRSA Managed` contains the OIDC provider ACK resource. `EKSIRSA External` leaves config ACK resources empty.

If `EKSIRSA Managed` points at an IAM OIDC provider URL that already exists outside the operator, ACK should surface the AWS conflict on the managed ACK resource. Operators should use `External` for pre-existing OIDC providers.

## Role Controller

Treat `SelfHostedIRSA` and `EKSIRSA` as annotation-based IRSA delivery.

Shared annotation-based IRSA behavior:

- create generated IAM Policy ACK CR when `spec.policyDocument` is set,
- create IAM Role ACK CR,
- render `AssumeRoleWithWebIdentity` trust policy from `status.issuerHostPath`, `status.oidcProviderARN`, and `spec.serviceAccount`,
- patch the remote ServiceAccount with `eks.amazonaws.com/role-arn`, `eks.amazonaws.com/audience`, `eks.amazonaws.com/sts-regional-endpoints`, and `eks.amazonaws.com/token-expiration`,
- record `status.deliveryType`,
- record `status.resolvedClusterName` when inventory resolution is ready,
- require remote ServiceAccount annotation readiness and config readiness before reporting role `Ready=True`,
- remove remote ServiceAccount annotations before deleting generated AWS resources.

`EKSPodIdentity` remains separate:

- render EKS Pod Identity trust policy,
- create ACK EKS `PodIdentityAssociation`,
- do not patch remote ServiceAccount annotations,
- clear `status.resolvedClusterName`.

Rename self-hosted-specific role helper concepts where they now represent annotation-based IRSA delivery. This keeps the implementation honest without changing the external annotation keys.

## Remote IRSA Consumer

`pkg/remoteirsa` should allow `SelfHostedIRSA` and `EKSIRSA`.

The credential flow is unchanged:

1. Resolve `AWSWorkloadIdentityConfig/default` and the matching `AWSServiceAccountRole`.
2. Resolve remote Kubernetes access from Cluster Inventory access providers.
3. Request a fresh remote `serviceaccounts/token` for audience `sts.amazonaws.com`.
4. Call STS `AssumeRoleWithWebIdentity` using `AWSServiceAccountRole.status.roleARN`.

`EKSPodIdentity` remains unsupported for `pkg/remoteirsa` and should keep returning `UnsupportedDeliveryType`.

## Deletion

`AWSServiceAccountRole` deletion removes delivery before generated AWS resources:

- `SelfHostedIRSA` and `EKSIRSA` remove remote ServiceAccount annotations when the remote cluster is reachable.
- `EKSPodIdentity` skips annotation cleanup.
- Delete EKS `PodIdentityAssociation`, IAM Role, and generated IAM Policy ACK CRs before removing the role finalizer.

`AWSWorkloadIdentityConfig` deletion remains blocked while roles remain in the namespace unless force-delete is set.

After unblocked config deletion:

- `SelfHostedIRSA` keeps the existing cleanup: remote webhook runtime, IAM OpenIDConnectProvider ACK CR, signing Secret, S3 OIDC objects, and S3 Bucket ACK CR.
- `EKSIRSA Managed` deletes only the operator-owned ACK IAM `OpenIDConnectProvider`.
- `EKSIRSA External` does not delete the external IAM OIDC provider.
- `EKSIRSA` never deletes signing Secrets, S3 issuer objects, S3 Bucket ACK CRs, or self-hosted webhook runtime resources.
- `EKSPodIdentity` keeps no config-owned AWS resources to delete.

Force-delete still means the operator may leave remote delivery resources behind only where the existing safety rules allow it.

## Status

Reuse existing status fields:

- `status.issuerHostPath` records the issuer URL host/path without `https://`.
- `status.oidcProviderARN` records the managed ACK OIDC provider ARN or external provider ARN.
- `status.ackResources` records only operator-owned ACK resources.
- `status.resolvedClusterName` records the annotation cleanup target for `SelfHostedIRSA` and `EKSIRSA`.

Reuse existing conditions:

- `IAMProviderReady`
- `IssuerReady`
- `WebhookRuntimeReady`
- `ServiceAccountAnnotationReady`
- `DeliveryReady`
- `Ready`

For `EKSIRSA`, `WebhookRuntimeReady=True/NotRequired` because the operator does not manage a webhook runtime.

## Documentation

Update:

- API reference for `EKSIRSA` and `spec.eksIRSA`.
- Delivery types concept page.
- Bind ServiceAccount guide.
- Remote IRSA consumer guide to mention `EKSIRSA`.
- Deletion behavior reference.
- Status conditions reference.
- IAM permissions reference for managed OIDC provider behavior.
- Helm chart README and generated CRDs.

## Tests

Add focused tests for:

- CRD enum and CEL validation shape.
- Config reconcile for `EKSIRSA Managed`.
- Config reconcile for `EKSIRSA External`.
- Config deletion for managed versus external OIDC providers.
- Role trust policy rendering for `EKSIRSA`.
- Role annotation delivery and deletion cleanup for `EKSIRSA`.
- `pkg/remoteirsa` accepting `EKSIRSA` and rejecting `EKSPodIdentity`.
- stale `PodIdentityAssociation` ACK resource pruning when switching away from `EKSPodIdentity`.
- docs/chart CRD sync checks.

## Implementation Notes

Prefer small helper predicates instead of delivery-type string checks spread across controllers:

- `isAnnotationBasedIRSADelivery(delivery)`
- `requiresRemoteServiceAccountAnnotations(delivery)`
- `usesSelfHostedIssuer(delivery)`
- `usesManagedConfigOIDCProvider(config)`

Keep ACK CRs as the source of truth for IAM resources. Keep S3 OIDC object writes limited to `SelfHostedIRSA`.
