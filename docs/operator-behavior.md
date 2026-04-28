# Operator Behavior Reference

The main [README](../README.md) owns the public overview: user-facing API
examples, delivery-type diagrams, install instructions, and policy restriction
guidance. This page records behavior that users and maintainers may need when
operating or debugging the operator: delivery-specific details, controller
ownership, idempotency, and status semantics. Implementation guardrails for
coding agents are kept in [AGENTS.md](../AGENTS.md).

## Self-Hosted IRSA Behavior

The issuer URL is the regional S3 HTTPS endpoint for the generated bucket:

```text
https://<bucket>.s3.<region>.amazonaws.com
```

Only these public S3 object keys are written:

```text
.well-known/openid-configuration
keys.json
```

The bucket policy grants public `s3:GetObject` only for those two keys. The
operator manager itself needs AWS credentials that can `s3:PutObject` and
`s3:DeleteObject` for those keys, because ACK manages the bucket but not the
object contents.

The controller records the published signing key ID in
`AWSWorkloadIdentityConfig.status.publishedKeyID`. While the bucket reports
synced and the recorded key ID matches the active signing Secret, the operator
skips re-uploading the issuer objects. The recorded key ID is cleared whenever
the bucket condition transitions back to not-synced so that ACK-driven bucket
recreation triggers a fresh upload.

Target ServiceAccounts are patched with annotations consumed by
aws-pod-identity-webhook:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: app
  namespace: default
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/generated-role
    eks.amazonaws.com/audience: sts.amazonaws.com
    eks.amazonaws.com/sts-regional-endpoints: "true"
    eks.amazonaws.com/token-expiration: "86400"
```

## EKS Pod Identity Behavior

Required ClusterProfile properties:

```yaml
status:
  properties:
    - name: aws.identity.appthrust.io/eks-cluster-name
      value: prod-a
    - name: aws.identity.appthrust.io/eks-cluster-arn
      value: arn:aws:eks:ap-northeast-1:123456789012:cluster/prod-a
    - name: aws.identity.appthrust.io/aws-account-id
      value: "123456789012"
```

Optional properties:

```yaml
status:
  properties:
    - name: aws.identity.appthrust.io/aws-organization-id
      value: o-example
    - name: aws.identity.appthrust.io/eks-auto-mode
      value: "true"
```

`aws-organization-id` adds an `aws:SourceOrgId` condition to the trust policy.
`eks-auto-mode=true` lets the operator mark `PodIdentityAgentReady=True`; without
it, `PodIdentityAgentReady` remains `Unknown` until an external readiness signal
is added.

## Controller Ownership

| Controller | Responsibility |
| --- | --- |
| AWSWorkloadIdentityConfig controller | single writer for operator config resolution, ClusterProfile resolution, signing Secret, S3 issuer bucket, OIDC objects, IAM OIDC provider, remote webhook runtime objects, and config status |
| AWSServiceAccountRole controller | single writer for namespace config resolution, IAM policy, IAM role, EKS PodIdentityAssociation, remote ServiceAccount annotation delivery, and role status |
| self-hosted webhook runtime controller | watch-only remote controller; when managed aws-pod-identity-webhook objects change or disappear, it enqueues the owning AWSWorkloadIdentityConfig and does not write runtime objects or local status |
| self-hosted ServiceAccount controller | remote ServiceAccount annotation drift repair; does not write local API object status |

## Deletion Behavior

`AWSServiceAccountRole` deletion removes delivery before generated AWS
resources. For `SelfHostedIRSA`, remote ServiceAccount annotations are removed
only when the target cluster is reachable. The controller then deletes the ACK
PodIdentityAssociation, IAM Role, and generated IAM Policy CRs before removing
the role finalizer.

`AWSWorkloadIdentityConfig` deletion is blocked while
`AWSServiceAccountRole` objects remain in the namespace, unless the config has
`aws.identity.appthrust.io/force-delete: "true"`. Force-delete also lets config
deletion continue when remote runtime cleanup cannot be completed safely, such
as when the target cluster cannot be reached or remote deletion fails. This
deliberately accepts leaving remote runtime resources behind.

`AWSServiceAccountRole.status` records the last resolved delivery type and, for
`SelfHostedIRSA`, the last ready multicluster-runtime cluster name. If a config
is force-deleted while roles remain, later role deletion uses that recorded
state to remove remote ServiceAccount annotations before deleting hub ACK
children. If that recorded state is absent or the target cluster cannot be
reached, the role finalizer remains until operators restore a cleanup path or
perform deliberate manual cleanup.

For `SelfHostedIRSA`, remote webhook runtime cleanup keeps the remote safety
guardrails: it runs only when the target cluster is reachable and no other live
config resolves to the same target cluster. If the target cluster is
unavailable or cleanup otherwise fails, deletion remains blocked unless
force-delete is set.

Hub-side self-hosted child cleanup is independent from remote runtime cleanup.
The IAM OpenIDConnectProvider ACK CR, signing key Secret, and S3 issuer cleanup
branch may be deleted in parallel. Inside the S3 branch, the operator deletes
the `.well-known/openid-configuration` and `keys.json` objects with the AWS S3
API before deleting the ACK Bucket CR. ACK-owned resources are removed by
deleting their ACK CRs; ACK remains responsible for completing AWS-side deletes
and surfacing AWS deletion failures on those CRs.

## Status Conditions

`AWSWorkloadIdentityConfig` emits these conditions:

| Condition | Meaning |
| --- | --- |
| `OperatorConfigReady` | `AWSWorkloadIdentityOperatorConfig/default` is available |
| `ClusterProfileResolved` | namespace ClusterProfile has been resolved |
| `BucketReady` | self-hosted issuer bucket is synced by ACK |
| `OIDCObjectsPublished` | self-hosted discovery and JWKS objects were written to S3 |
| `IAMProviderReady` | IAM OpenIDConnectProvider is synced by ACK |
| `IssuerReady` | issuer resources for the selected delivery type are ready |
| `WebhookRuntimeReady` | self-hosted webhook runtime readiness on the target cluster. `True` means runtime resources are synced, the Deployment is Available, and the MutatingWebhookConfiguration points at the current Service and CA bundle. `True/NotRequired` for `EKSPodIdentity`. |
| `Ready` | config reconciliation is complete |
| `DeletionBlocked` | config deletion is waiting for role bindings to be removed or for safe remote runtime cleanup |

`AWSServiceAccountRole` emits these conditions:

| Condition | Meaning |
| --- | --- |
| `OperatorConfigReady` | operator config is available |
| `ConfigResolved` | namespace `AWSWorkloadIdentityConfig/default` is available |
| `InventoryResolved` | namespace ClusterProfile has been resolved |
| `TrustPolicyReady` | trust policy inputs are available and rendered |
| `PolicyReady` | managed policies or generated IAM Policy are ready |
| `RoleReady` | generated IAM Role is synced by ACK |
| `ServiceAccountAnnotationReady` | self-hosted remote ServiceAccount annotations are synced |
| `PodIdentityAssociationReady` | EKS PodIdentityAssociation is synced by ACK |
| `PodIdentityAgentReady` | EKS Pod Identity Agent readiness signal |
| `DeliveryReady` | delivery-specific resources are ready |
| `Ready` | role reconciliation is complete |

ACK child resource state is also copied into `status.ackResources`.

If multiple live `AWSServiceAccountRole` objects in the same namespace bind the
same remote ServiceAccount, each conflicting role reports `DeliveryReady=False`
and `Ready=False` with reason `InvalidSpec`. The controller does not choose a
winner, delete generated AWS resources, or remove remote annotations for one of
the roles automatically; operators must remove the duplicate binding state.
