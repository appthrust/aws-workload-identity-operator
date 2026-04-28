# Cleanup And Force Delete

Prefer normal deletion. Force delete is for deliberate cleanup tradeoffs when
operators accept that remote runtime resources may be left behind.

## Normal Role Deletion

Delete `AWSServiceAccountRole` objects before deleting
`AWSWorkloadIdentityConfig/default`.

For `SelfHostedIRSA`, the controller removes remote ServiceAccount annotations
only when the target cluster is reachable. It then deletes generated ACK CRs and
removes the role finalizer.

## Config Deletion

`AWSWorkloadIdentityConfig` deletion is blocked while
`AWSServiceAccountRole` objects remain in the namespace unless the config has
`aws.identity.appthrust.io/force-delete: "true"`.

After role deletion is complete, config cleanup removes the self-hosted IAM
OpenIDConnectProvider ACK CR, signing key Secret, S3 OIDC objects, S3 Bucket ACK
CR, and remote webhook runtime objects when safe. Detailed ordering is in
[Deletion Behavior](../reference/deletion-behavior.md).

## Force Delete

Set `aws.identity.appthrust.io/force-delete: "true"` only when you deliberately
accept the remaining cleanup risk:

```sh
kubectl annotate awsworkloadidentityconfig default \
  -n <workload-namespace> \
  aws.identity.appthrust.io/force-delete=true
```

If remote runtime cleanup cannot be completed safely, force-delete lets config
deletion continue and may leave remote runtime resources behind.
