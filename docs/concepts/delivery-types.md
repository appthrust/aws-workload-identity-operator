# Delivery Types

The operator exposes the same workload-facing binding API for two delivery
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
