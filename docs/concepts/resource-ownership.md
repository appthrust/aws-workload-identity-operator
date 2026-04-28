# Resource Ownership

Only the owner controller writes the status of its local API object. Remote
controllers update remote resources and enqueue the local reconciliation paths
as needed.

| Controller | Responsibility |
| --- | --- |
| `AWSWorkloadIdentityConfig` controller | Config resolution, `ClusterProfile` resolution, self-hosted signing Secret, self-hosted S3 issuer bucket CR, self-hosted OIDC objects, managed IAM OIDC provider CRs, remote webhook runtime objects for `SelfHostedIRSA`, and config status. |
| `AWSServiceAccountRole` controller | Namespace config resolution, IAM policy CR, IAM role CR, EKS PodIdentityAssociation CR, remote ServiceAccount annotation delivery, and role status. |
| `AWSServiceAccountRoleReplicaSet` controller | OCM Placement resolution, generated child role apply/prune, and ReplicaSet status. It never writes child role status. |
| self-hosted webhook runtime controller | Watch-only remote controller that enqueues the owning `AWSWorkloadIdentityConfig` when managed runtime objects change or disappear. |
| self-hosted ServiceAccount controller | Remote ServiceAccount annotation drift repair. It does not write local API object status. |

ACK-owned resources are created, updated, and deleted by deleting or patching
their ACK CRs. ACK remains responsible for AWS-side reconciliation.
