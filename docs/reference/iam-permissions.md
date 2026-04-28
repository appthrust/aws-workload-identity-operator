# IAM Permissions

ACK controllers need AWS credentials for the IAM, S3, and EKS resources they
reconcile from ACK CRs. For `SelfHostedIRSA`, the operator manager also needs a
small direct S3 permission surface to publish and delete issuer objects.

## SelfHostedIRSA Manager Policy

Attach this policy to the IAM role assumed by the operator manager Pod. The
manager itself calls the AWS S3 API to write and delete only these two issuer
objects:

- `.well-known/openid-configuration`
- `keys.json`

The S3 bucket and bucket policy are managed separately through ACK S3 CRs, so
the manager role does not need bucket create, bucket policy, or object read
permissions for this path.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PublishSelfHostedIRSAOIDCIssuerObjects",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:DeleteObject"
      ],
      "Resource": [
        "arn:aws:s3:::awi-wlc-a-ap-northeast-1-*/.well-known/openid-configuration",
        "arn:aws:s3:::awi-wlc-a-ap-northeast-1-*/keys.json"
      ]
    }
  ]
}
```

Replace `wlc-a` and `ap-northeast-1` with the workload namespace and region
used by `AWSWorkloadIdentityConfig/default`. For multiple target namespaces or
regions, add the corresponding bucket object ARNs, or use a broader bucket
prefix such as `arn:aws:s3:::awi-*/keys.json` when that fits your security
model.

## AWS IRSA Sidecar TokenRequest RBAC

`aws-irsa-sidecar` needs Kubernetes RBAC on the target cluster, not IAM
permissions. The mounted kubeconfig identity must be able to identify itself,
read the intended remote `ServiceAccount`, and create a bounded token for that
same `ServiceAccount`.

Grant the namespace-scoped permissions only for the intended target
`ServiceAccount`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: aws-irsa-sidecar-karpenter
  namespace: kube-system
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    resourceNames: ["karpenter"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["karpenter"]
    verbs: ["create"]
```

Bind the Role to the Kubernetes identity in the mounted kubeconfig. This is not
an AWS IAM principal. Replace the subject fields with the exact target-cluster
user, group, or ServiceAccount used by that kubeconfig:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: aws-irsa-sidecar-karpenter
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aws-irsa-sidecar-karpenter
subjects:
  # Replace with the target-cluster identity from the mounted kubeconfig.
  - kind: User
    apiGroup: rbac.authorization.k8s.io
    name: replace-with-target-cluster-user
```

Grant `selfsubjectreviews` at cluster scope so the sidecar can infer the
remote identity carried by the kubeconfig:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aws-irsa-sidecar-selfsubjectreview
rules:
  - apiGroups: ["authentication.k8s.io"]
    resources: ["selfsubjectreviews"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-irsa-sidecar-selfsubjectreview
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aws-irsa-sidecar-selfsubjectreview
subjects:
  # Replace with the same target-cluster identity used above.
  - kind: User
    apiGroup: rbac.authorization.k8s.io
    name: replace-with-target-cluster-user
```

A ServiceAccount-subject variant is available in
[the sidecar RBAC example](../examples/aws-irsa-sidecar-rbac.yaml).

## AWS-Compatible Endpoints

When the manager must call an AWS-compatible endpoint instead of the default AWS
endpoint resolution, configure the chart with `aws.endpointURL`. HTTP endpoints
also require `aws.allowUnsafeEndpointURLs=true`.

This mirrors ACK's AWS API endpoint override. It does not change the public
`SelfHostedIRSA` issuer URL, which remains the regional S3 HTTPS URL for the
generated bucket.

## EKSIRSA IAM Provider

For `EKSIRSA` with `spec.eksIRSA.oidcProvider.management: Managed`, ACK IAM
needs permission to create, tag, update, read, and delete IAM
OpenIDConnectProvider resources. For `management: External`, ACK IAM does not
manage the provider; the operator only uses the supplied provider ARN in trust
policies after checking that its provider path matches `issuerURL`.
