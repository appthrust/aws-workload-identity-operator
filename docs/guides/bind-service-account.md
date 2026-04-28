# Bind A ServiceAccount

Every workload namespace has one `AWSWorkloadIdentityConfig/default` that
selects the delivery type and AWS region. `AWSServiceAccountRole` objects in
that namespace bind remote Kubernetes `ServiceAccount` identities to generated
IAM roles.

Copy-paste starting manifests are in [Examples](../examples/README.md).

## SelfHostedIRSA

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityConfig
metadata:
  name: default
  namespace: wlc-a
spec:
  type: SelfHostedIRSA
  region: ap-northeast-1
```

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: aws-load-balancer-controller
  namespace: wlc-a
spec:
  serviceAccount:
    namespace: kube-system
    name: aws-load-balancer-controller
  policyARNs:
    - arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

For OCM, namespace `wlc-a` matches the target
`open-cluster-management.io/cluster-name` label value.

## EKSIRSA

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityConfig
metadata:
  name: default
  namespace: eks-prod
spec:
  type: EKSIRSA
  region: ap-northeast-1
  eksIRSA:
    issuerURL: https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE
    oidcProvider:
      management: External
      arn: arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE
```

Use the same `AWSServiceAccountRole` shape. `EKSIRSA` annotates the remote
ServiceAccount with the generated role ARN and relies on native EKS IRSA.
Workload Pods should use normal `serviceAccountName` references.

## EKS Pod Identity

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityConfig
metadata:
  name: default
  namespace: eks-prod
spec:
  type: EKSPodIdentity
  region: ap-northeast-1
```

Use the same `AWSServiceAccountRole` shape. `EKSPodIdentity` creates no
self-hosted OIDC issuer and no self-hosted webhook runtime.

## Inline Policy Documents

`AWSServiceAccountRole` can use managed policy ARNs or a generated IAM Policy
from `spec.policyDocument`. If your platform needs an allowlist, enforce it with
an admission policy. See [Restrict IAM Policy Inputs](restrict-iam-policy-inputs.md).
