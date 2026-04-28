# Deletion Behavior

Deletion is ordered to remove delivery before generated AWS resources and to let
ACK remain responsible for AWS-side deletion.

## AWSServiceAccountRole

`AWSServiceAccountRole` deletion removes delivery before generated AWS
resources.

For `SelfHostedIRSA` and `EKSIRSA`, remote ServiceAccount annotations are
removed only when the target cluster is reachable. `EKSPodIdentity` does not
use annotation cleanup.

After delivery cleanup, the controller deletes generated ACK children before
removing the role finalizer:

- EKS PodIdentityAssociation CRs for `EKSPodIdentity`.
- IAM Role CRs for all delivery types.
- generated IAM Policy CRs when `spec.policyDocument` created one.

Delete ACK-owned resources by deleting their ACK CRs. ACK remains responsible
for completing AWS-side delete operations and surfacing AWS deletion failures on
the ACK resources.

## AWSWorkloadIdentityConfig

`AWSWorkloadIdentityConfig` deletion is blocked while
`AWSServiceAccountRole` objects remain in the namespace, unless the config has
`aws.identity.appthrust.io/force-delete: "true"`.

After config deletion is unblocked, the self-hosted IAM OpenIDConnectProvider
ACK CR, signing key Secret, S3 OIDC objects, and S3 Bucket ACK CR are removed
before the config finalizer. The IAM OpenIDConnectProvider ACK CR, signing key
Secret, and S3 issuer cleanup branch are independent and may run in parallel.
Inside the S3 branch, the operator deletes `.well-known/openid-configuration`
and `keys.json` with the AWS S3 API before deleting the ACK Bucket CR.

For `EKSIRSA`, deletion removes only operator-owned config resources.
`EKSIRSA` with `management: Managed` deletes the ACK IAM
`OpenIDConnectProvider` CR. `EKSIRSA` with `management: External` does not
delete the external IAM OIDC provider. `EKSIRSA` never deletes signing Secrets,
S3 issuer objects, S3 Bucket ACK CRs, or self-hosted webhook runtime resources.

Remote self-hosted webhook runtime cleanup runs only when the target cluster is
reachable and no other live `AWSWorkloadIdentityConfig` resolves to the same
target cluster. If remote cleanup cannot be completed safely, deletion remains
blocked unless force-delete is set.

## Recorded Role State

`AWSServiceAccountRole.status` records the last resolved delivery type and, for
`SelfHostedIRSA` and `EKSIRSA`, the last ready multicluster-runtime cluster
name. If a config is force-deleted while roles remain, later role deletion uses
that recorded state to remove remote ServiceAccount annotations before deleting
hub ACK children.

If recorded state is absent or the target cluster cannot be reached, the role
finalizer remains until operators restore a cleanup path or perform deliberate
manual cleanup.
