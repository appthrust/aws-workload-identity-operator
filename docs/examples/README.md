# Examples

These manifests use concrete sample names so they can be copied into a scratch
environment and then adjusted for your AWS account, cluster names, namespaces,
and policy choices.

| Example | File |
| --- | --- |
| `SelfHostedIRSA` target namespace and role binding | [selfhosted-irsa.yaml](selfhosted-irsa.yaml) |
| `EKSIRSA` with an operator-managed IAM OIDC provider | [eks-irsa-managed-provider.yaml](eks-irsa-managed-provider.yaml) |
| `EKSIRSA` with an externally managed IAM OIDC provider | [eks-irsa-external-provider.yaml](eks-irsa-external-provider.yaml) |
| `EKSPodIdentity` role binding | [eks-pod-identity.yaml](eks-pod-identity.yaml) |
| OCM `Placement` fan-out with `AWSServiceAccountRoleReplicaSet` | [replicaset-placement.yaml](replicaset-placement.yaml) |
| Remote RBAC for `aws-irsa-sidecar` | [aws-irsa-sidecar-rbac.yaml](aws-irsa-sidecar-rbac.yaml) |

The examples keep workload Pods annotation-free. Workloads should reference the
remote Kubernetes `ServiceAccount` with normal `serviceAccountName`; the
operator owns AWS annotation delivery when the delivery type requires it.
