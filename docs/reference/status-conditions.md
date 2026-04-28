# Status Conditions

Use status conditions to determine whether the operator has resolved platform
configuration, target cluster access, AWS resources, and delivery-specific
runtime state.

## AWSWorkloadIdentityConfig

| Condition | Meaning |
| --- | --- |
| `OperatorConfigReady` | `AWSWorkloadIdentityOperatorConfig/default` is available. |
| `ClusterProfileResolved` | The namespace target `ClusterProfile` has been resolved. |
| `BucketReady` | The self-hosted issuer bucket is synced by ACK. |
| `OIDCObjectsPublished` | Self-hosted discovery and JWKS objects were written to S3. |
| `IAMProviderReady` | The IAM OpenIDConnectProvider is synced by ACK. |
| `IssuerReady` | Issuer resources for the selected delivery type are ready. |
| `WebhookRuntimeReady` | Self-hosted webhook runtime readiness on the target cluster. `True/NotRequired` for `EKSPodIdentity`. |
| `Ready` | Config reconciliation is complete. |
| `DeletionBlocked` | Config deletion is waiting for role bindings to be removed or for safe remote runtime cleanup. |

## AWSServiceAccountRole

| Condition | Meaning |
| --- | --- |
| `OperatorConfigReady` | Operator config is available. |
| `ConfigResolved` | Namespace `AWSWorkloadIdentityConfig/default` is available. |
| `InventoryResolved` | Namespace target `ClusterProfile` has been resolved. |
| `TrustPolicyReady` | Trust policy inputs are available and rendered. |
| `PolicyReady` | Managed policies or generated IAM Policy are ready. |
| `RoleReady` | Generated IAM Role is synced by ACK. |
| `ServiceAccountAnnotationReady` | Self-hosted remote ServiceAccount annotations are synced. |
| `PodIdentityAssociationReady` | EKS PodIdentityAssociation is synced by ACK. |
| `PodIdentityAgentReady` | EKS Pod Identity Agent readiness signal. |
| `DeliveryReady` | Delivery-specific resources are ready. |
| `Ready` | Role reconciliation is complete. |

If multiple live `AWSServiceAccountRole` objects in the same namespace bind the
same remote ServiceAccount, each conflicting role reports
`DeliveryReady=False` and `Ready=False` with reason `InvalidSpec`. Operators
must remove the duplicate binding state.

## AWSServiceAccountRoleReplicaSet

| Condition | Meaning |
| --- | --- |
| `PlacementResolved` | Same-namespace OCM Placement refs were resolved through owned PlacementDecision objects. |
| `PlacementRolledOut` | OCM rollout planning has selected the clusters that may receive generated child roles. |
| `AWSServiceAccountRolesApplied` | Generated child roles exist or conflicts/failures are recorded. |
| `AWSServiceAccountRolesReady` | Generated children report `Ready=True` for their current generation. |
| `CleanupBlocked` | ReplicaSet deletion is waiting for owned child roles to finish their own deletion cleanup. |
| `Ready` | Placement resolution, child apply, and child readiness are complete. |
