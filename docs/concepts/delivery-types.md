# Delivery Types

The operator exposes the same workload-facing binding API for three delivery
mechanisms.

## SelfHostedIRSA

Use `SelfHostedIRSA` for Kubernetes clusters that should use AWS web identity
federation without relying on the EKS OIDC issuer lifecycle.

The operator prepares:

- a static S3-backed OIDC issuer,
- IAM OIDC provider and generated trust policy through ACK IAM CRs,
- remote aws-pod-identity-webhook runtime resources,
- remote `ServiceAccount` annotations for bound workloads.

The operator writes OIDC discovery and JWKS objects to S3 directly with the AWS
S3 API. ACK manages the S3 bucket and IAM OIDC provider, not the individual S3
objects.

## EKSIRSA

Use `EKSIRSA` for EKS clusters that should use native EKS OIDC issuer based
IRSA instead of EKS Pod Identity.

The operator prepares:

- the IAM OIDC provider through an ACK IAM CR when
  `spec.eksIRSA.oidcProvider.management: Managed`,
- no operator-managed IAM OIDC provider; the config references an external
  provider ARN,
- generated IAM role and optional generated IAM policy through ACK IAM CRs,
- remote `ServiceAccount` annotations for bound workloads.

`EKSIRSA` does not create a self-hosted S3 issuer, does not write OIDC
discovery or JWKS objects, and does not install the self-hosted webhook runtime.
EKS provides the token issuer and native IRSA annotation contract.

Managed provider example:

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

## EKS Pod Identity

Use `EKSPodIdentity` for EKS clusters that use EKS Pod Identity associations.

The operator prepares:

- the generated IAM role through ACK IAM CRs,
- the EKS `PodIdentityAssociation` through ACK EKS CRs.

`EKSPodIdentity` does not create a self-hosted OIDC issuer or install the
self-hosted webhook runtime.

Required `ClusterProfile.status.properties`:

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
`eks-auto-mode=true` lets the operator mark `PodIdentityAgentReady=True`.
