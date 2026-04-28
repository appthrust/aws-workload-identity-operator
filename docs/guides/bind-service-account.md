# Bind A ServiceAccount

Every workload namespace has one `AWSWorkloadIdentityConfig/default` that
selects the delivery type and AWS region. `AWSServiceAccountRole` objects in
that namespace bind remote Kubernetes `ServiceAccount` identities to generated
IAM roles.

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
