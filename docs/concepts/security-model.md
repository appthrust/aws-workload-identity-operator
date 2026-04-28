# Security Model

The operator separates Kubernetes cluster access from AWS identity.

- Cluster Inventory access providers produce remote Kubernetes `rest.Config`
  values.
- `AWSWorkloadIdentityConfig/default` and `AWSServiceAccountRole` are the AWS
  identity contract.
- ACK CRs are the source of truth for IAM, S3 bucket, and EKS resources.
- S3 OIDC discovery and JWKS objects are written with the AWS S3 API because ACK
  does not manage individual S3 objects.

Workloads remain annotation-free. They use normal `serviceAccountName`
references, and the operator owns AWS annotation delivery when the selected
delivery type requires it.

Admission policy for requested IAM permissions is a platform concern. See
[Restrict IAM Policy Inputs](../guides/restrict-iam-policy-inputs.md).
