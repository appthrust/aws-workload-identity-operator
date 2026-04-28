# AWS Workload Identity Operator Architecture

`aws-workload-identity-operator` binds Kubernetes ServiceAccounts to AWS IAM
roles across clusters managed from a hub cluster.

The operator discovers target clusters from Cluster Inventory API
`ClusterProfile` objects. AWS control-plane resources are reconciled on the hub
through ACK custom resources, while target-cluster Kubernetes resources are
written through multicluster-runtime remote clients.

## Workload API

Workloads keep using normal Kubernetes `serviceAccountName` references. Platform
users create one `AWSServiceAccountRole` in the namespace that represents the
target cluster:

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: app
  namespace: wlc-a
spec:
  serviceAccount:
    namespace: default
    name: app
  policyARNs:
    - arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

The operator creates a generated IAM role for `default/app`, attaches the
requested managed or generated inline policy, renders the trust policy for the
selected delivery type, and delivers the target-cluster integration.

## Delivery Types

| Type | Target clusters | Hub resources | Target-cluster resources |
| --- | --- | --- | --- |
| `SelfHostedIRSA` | Kubernetes clusters that use AWS web identity federation | S3 OIDC issuer bucket, OIDC discovery/JWKS objects, IAM OIDC provider, IAM role, optional IAM policy | aws-pod-identity-webhook runtime and workload ServiceAccount annotations |
| `EKSPodIdentity` | Managed EKS clusters | IAM role, optional IAM policy, EKS PodIdentityAssociation | EKS Pod Identity Agent, installed outside this operator |

`AWSWorkloadIdentityConfig.spec.type` and `spec.region` are immutable after
creation.

## Namespace Model

One namespace maps to one target cluster. The namespace contains the
ClusterProfile, the target-cluster config, and all ServiceAccount bindings for
that cluster:

```text
namespace: wlc-a
ClusterProfile: wlc-a/wlc-a
AWSWorkloadIdentityConfig: wlc-a/default
AWSServiceAccountRole: wlc-a/*
```

`AWSWorkloadIdentityConfig` and `AWSServiceAccountRole` do not carry an explicit
cluster reference. The namespace boundary is the cluster boundary.

## Platform Config

The cluster-scoped `AWSWorkloadIdentityOperatorConfig/default` stores operator
wide defaults:

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityOperatorConfig
metadata:
  name: default
spec:
  selfHostedIRSA:
    webhookNamespace: aws-pod-identity-webhook
```

`spec.permissionsBoundaryARN` is optional. When configured, generated IAM roles
use that permissions boundary.

For `SelfHostedIRSA`, `spec.selfHostedIRSA.webhookNamespace` in
`AWSWorkloadIdentityOperatorConfig/default` is the source of truth for the
remote webhook namespace.

## Self-Hosted IRSA Flow

`SelfHostedIRSA` provides an IRSA-compatible path for non-EKS Kubernetes
clusters.

```text
AWSWorkloadIdentityConfig/default
  -> creates or reuses a signing key Secret on the hub
  -> creates an ACK S3 Bucket for the issuer
  -> writes S3 objects .well-known/openid-configuration and keys.json
  -> creates an ACK IAM OpenIDConnectProvider
  -> keeps the remote aws-pod-identity-webhook runtime installed

AWSServiceAccountRole/default/app
  -> creates an ACK IAM Policy when spec.policyDocument is set
  -> creates an ACK IAM Role with a generated web identity trust policy
  -> patches the remote ServiceAccount with eks.amazonaws.com/* annotations
  -> Pods use projected tokens to call STS AssumeRoleWithWebIdentity
```

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

## EKS Pod Identity Flow

`EKSPodIdentity` uses native EKS Pod Identity instead of a self-hosted OIDC
issuer.

```text
AWSWorkloadIdentityConfig/default
  -> resolves EKS cluster identity from ClusterProfile.status.properties

AWSServiceAccountRole/default/app
  -> creates an ACK IAM Role with a generated pods.eks.amazonaws.com trust policy
  -> creates an ACK EKS PodIdentityAssociation
  -> EKS Pod Identity Agent serves credentials to Pods
```

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

## Boundaries

- The operator does not read Cluster API `Cluster` objects directly.
- The operator does not read remote kubeconfig Secrets directly in reconcilers.
- The operator does not write `ClusterProfile.status`.
- With OCM, the operator resolves `ClusterProfile` objects by the
  `open-cluster-management.io/cluster-name` label, so the operator namespace
  only needs a normal `ManagedClusterSetBinding`.
- Cluster Inventory access providers produce the remote `rest.Config` used by
  multicluster-runtime.
- ACK CRs are the source of truth for IAM, S3 bucket, and EKS resources.
- S3 OIDC discovery and JWKS objects are written with the AWS S3 API, not ACK.
- Workloads do not write AWS annotations themselves; they use normal
  `serviceAccountName` references.

## IAM Policy Inputs

`AWSServiceAccountRole.spec.policyARNs` and `spec.policyDocument` describe the
IAM permissions a workload requests. The operator validates the API shape and
trust policy, but it does not carry a platform-specific allowlist or inspect the
semantics of inline policy documents.

Use an admission policy engine such as Kyverno to restrict which managed policy
ARNs are allowed, to block inline policies, or to inspect inline policy content.
The main [README](../README.md#restrict-iam-policy-inputs) includes a Kyverno
example.

## Controllers

| Controller | Responsibility |
| --- | --- |
| AWSWorkloadIdentityConfig controller | operator config resolution, ClusterProfile resolution, signing Secret, S3 issuer bucket, OIDC objects, IAM OIDC provider, remote webhook runtime, config status |
| AWSServiceAccountRole controller | namespace config resolution, IAM policy, IAM role, EKS PodIdentityAssociation, remote ServiceAccount annotation delivery |
| self-hosted webhook runtime controller | remote aws-pod-identity-webhook drift bridge that enqueues the owning Config |
| self-hosted ServiceAccount controller | remote ServiceAccount annotation drift repair |

Only the owner controller writes the status of its local API object. Remote
controllers update remote resources and enqueue the local reconciliation paths as
needed.

## Deletion

`AWSServiceAccountRole` deletion removes delivery first, then generated AWS
resources:

1. Remove remote ServiceAccount annotations for `SelfHostedIRSA` when the remote
   cluster is still reachable.
2. Delete EKS PodIdentityAssociation, IAM Role, and generated IAM Policy.
3. Remove the finalizer.

`AWSWorkloadIdentityConfig` deletion is blocked while `AWSServiceAccountRole`
objects remain in the namespace, unless the platform administrator sets
`aws.identity.appthrust.io/force-delete: "true"` on the config.

After it is unblocked, deletion removes:

1. IAM OpenIDConnectProvider.
2. S3 OIDC discovery and JWKS objects.
3. S3 bucket.
4. Signing key Secret.
5. Finalizer.

ACK-owned resources are deleted by issuing delete requests for their ACK CRs.
ACK remains responsible for completing the AWS-side delete operation and for
surfacing any AWS deletion failures on the ACK resources. The remote self-hosted
webhook runtime is removed by the Config finalizer when the target cluster is
reachable and no other live `AWSWorkloadIdentityConfig` resolves to the same
target cluster. If the target cluster is unavailable, deletion is blocked unless
operators set the force-delete annotation and deliberately accept leaving remote
runtime resources behind.

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
| `DeletionBlocked` | config deletion is waiting for role bindings to be removed |

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

## Packaging

The Helm chart mounts Cluster Inventory access provider plugins with Kubernetes
image volumes:

```yaml
volumes:
  - name: open-cluster-management
    image:
      # Do not pin cp-creds by SHA yet; upstream digests change frequently while it stabilizes.
      reference: quay.io/open-cluster-management/cp-creds:latest
      pullPolicy: IfNotPresent
```

Clusters that do not support image volumes are outside the default chart target.
