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

## AWS-Compatible Endpoints

When the manager must call an AWS-compatible endpoint instead of the default AWS
endpoint resolution, configure the chart with `aws.endpointURL`. HTTP endpoints
also require `aws.allowUnsafeEndpointURLs=true`.

This mirrors ACK's AWS API endpoint override. It does not change the public
`SelfHostedIRSA` issuer URL, which remains the regional S3 HTTPS URL for the
generated bucket.
