# Operator Behavior

This page records behavior that users and maintainers may need when operating
or debugging the operator. API shape is in [API Reference](api.md), status
conditions are in [Status Conditions](status-conditions.md), and deletion
ordering is in [Deletion Behavior](deletion-behavior.md).

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
operator manager itself needs AWS credentials that can `s3:GetObject`,
`s3:PutObject`, and `s3:DeleteObject` for those keys because ACK manages the
bucket, not the object contents. `HeadObject` uses the `s3:GetObject`
authorization path.

After the S3 Bucket ACK CR reports synced, every reconcile verifies both issuer
objects with `HeadObject`. The controller compares content type and
operator-owned metadata (`awio-publication-format`, `awio-object-digest`, and
`awio-object-set-digest`) and rewrites any object whose metadata is missing or
stale. It records the last verified desired publication identity in
`AWSWorkloadIdentityConfig.status.selfHostedIssuer.publication`. When the bucket
is not synced, the target bucket remains in
`status.selfHostedIssuer.bucketName` and `publication` is cleared.

`SelfHostedIRSA` does not reconfigure the target kube-apiserver. The target
cluster must already issue bound ServiceAccount tokens whose issuer is this S3
URL, whose signing key matches the SelfHostedIRSA signing Secret, and whose
audience can be `sts.amazonaws.com`. For kubeadm and kind examples, see
[Configure SelfHostedIRSA For kubeadm And kind](../guides/selfhosted-irsa-kubeadm-kind.md).

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

Remote webhook runtime ownership is single-writer. The
`AWSWorkloadIdentityConfig` controller owns the runtime TLS Secrets, RBAC,
Service, Deployment, and MutatingWebhookConfiguration. It also ensures the
target cluster webhook Namespace exists when its remote access identity is
allowed to create it. During cleanup, it deletes managed runtime objects and
leaves the Namespace in place.

When the Helm chart creates OCM remote-permissions `ManifestWork` resources,
that ManifestWork is bootstrap for the remote access identity and namespace. It
is not the owner of the self-hosted webhook runtime objects.

The `selfhosted-webhook-runtime` controller is watch-only for those remote
objects. When they change or disappear, it enqueues the owning
`AWSWorkloadIdentityConfig/default` so the config controller writes the expected
state again.

The ServiceAccount watch path is split by responsibility:

- `selfhosted-role-enqueue` observes annotated remote ServiceAccount delete
  events and enqueues matching hub `AWSServiceAccountRole` objects through an
  indexed lookup.
- Initial annotation delivery is retry-driven by the role controller when the
  role exists before the remote ServiceAccount.
- `selfhosted-serviceaccount` is repair-only and reconciles previously
  annotated ServiceAccounts when their IRSA annotation drifts.

Remote webhook Deployment reconciliation is field-scoped. The operator remains
authoritative for the Deployment selector, operator labels, the named `webhook`
container, and the named `cert` volume. It preserves unrelated labels,
annotations, sidecars, additional volumes, and Pod scheduling or tuning fields.

## EKSIRSA Behavior

`EKSIRSA` uses the native EKS OIDC issuer for the target cluster. Set
`spec.eksIRSA.issuerURL` to the EKS issuer URL, for example:

```text
https://oidc.eks.<region>.amazonaws.com/id/<cluster-id>
```

When `spec.eksIRSA.oidcProvider.management: Managed`, the operator reconciles an
ACK IAM `OpenIDConnectProvider` CR for that issuer. When management is
`External`, the operator does not create or delete an IAM OIDC provider; the
config references the supplied external provider ARN.

Target ServiceAccounts use the normal EKS IRSA annotation contract with the
generated role ARN and STS token settings:

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

`EKSIRSA` does not create a self-hosted S3 issuer, does not write OIDC discovery
or JWKS objects, and does not install the self-hosted webhook runtime. EKS
provides the issuer and native IRSA token projection behavior.

## EKS Pod Identity Behavior

`EKSPodIdentity` creates no self-hosted OIDC issuer. The operator reconciles the
generated IAM role and EKS Pod Identity association through ACK CRs.

Required and optional `ClusterProfile.status.properties` are listed in
[Delivery Types](../concepts/delivery-types.md#eks-pod-identity).

`PodIdentityAgentReady` is marked ready when the target `ClusterProfile`
declares `aws.identity.appthrust.io/eks-auto-mode=true`; otherwise it remains
`Unknown` until a platform-provided readiness signal is available.

## Drift And Idempotency

ACK child resource state is copied into local status under
`status.ackResources`. The operator does not bypass ACK to mutate IAM, S3
bucket, or EKS resources directly.

For remote Kubernetes delivery, remote watch controllers enqueue local owners;
the local owner controller remains responsible for writing desired state and
status.
