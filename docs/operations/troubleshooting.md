# Troubleshooting

Start from conditions and reasons. The controller writes stable condition
reasons on `AWSWorkloadIdentityConfig`, `AWSServiceAccountRole`, and
`AWSServiceAccountRoleReplicaSet`; those reasons also feed the
`awio_condition_transition_total` metric.

```sh
kubectl get awsworkloadidentityconfig default -n <workload-namespace> -o yaml
kubectl get awsserviceaccountrole <name> -n <workload-namespace> -o yaml
kubectl get awsserviceaccountrolereplicaset <name> -n <namespace> -o yaml
```

For a compact view:

```sh
kubectl get awic,awsar,awsarrs -A
kubectl describe awsworkloadidentityconfig default -n <workload-namespace>
kubectl describe awsserviceaccountrole <name> -n <workload-namespace>
```

Condition meanings are in [Status Conditions](../reference/status-conditions.md).

## `OperatorConfigUnavailable`

Likely signal:

- `OperatorConfigReady=False` on config or role objects.

Check:

```sh
kubectl get awsworkloadidentityoperatorconfig default -o yaml
```

Fix:

- Create `AWSWorkloadIdentityOperatorConfig/default`.
- For `SelfHostedIRSA`, confirm `spec.selfHostedIRSA.webhookNamespace` matches
  the remote webhook namespace you expect.

## `ClusterProfileNotFound` Or `ResolverError`

Likely signal:

- `ClusterProfileResolved=False` on `AWSWorkloadIdentityConfig`.
- `InventoryResolved=False` on `AWSServiceAccountRole`.

Check:

```sh
kubectl get clusterprofile -A \
  -l open-cluster-management.io/cluster-name=<workload-namespace>
kubectl get clusterprofile -A \
  -l x-k8s.io/cluster-manager=open-cluster-management
kubectl get managedclustersetbinding -n aws-workload-identity-operator-system
```

Fix:

- Make sure the target `ClusterProfile` has
  `open-cluster-management.io/cluster-name=<workload-namespace>`.
- For OCM, bind the operator namespace to the normal `ManagedClusterSet`.
- Fix Cluster Inventory access-provider configuration. Reconciler code should
  not read Cluster API `Cluster` objects or remote kubeconfig Secrets directly.

## `InventoryUnavailable` Or `RemoteClusterUnavailable`

Likely signal:

- The Cluster Inventory provider resolved a target but multicluster-runtime
  could not build or use the remote `rest.Config`.
- Deletion may be blocked if safe remote cleanup cannot run.

Check:

```sh
kubectl get clusterprofile -A \
  -l open-cluster-management.io/cluster-name=<workload-namespace> -o yaml
kubectl logs -n aws-workload-identity-operator-system deploy/aws-workload-identity-operator \
  --since=30m | grep -E 'InventoryUnavailable|RemoteClusterUnavailable|cluster-inventory'
```

Fix:

- Repair the Cluster Inventory access provider, such as the OCM `cp-creds`
  ManagedServiceAccount path.
- Confirm the remote API server is reachable from the manager Pod.
- Do not unblock deletion by removing finalizers manually. Use
  `aws.identity.appthrust.io/force-delete: "true"` only when you deliberately
  accept leaving remote runtime resources behind.

## `WaitingForACK` Or `ACKResourceWaiting`

Likely signal:

- `BucketReady`, `IAMProviderReady`, `PolicyReady`, `RoleReady`, or
  `PodIdentityAssociationReady` is false.
- `status.ackResources` shows ACK child conditions.

Check:

```sh
kubectl get awsworkloadidentityconfig default -n <workload-namespace> \
  -o jsonpath='{range .status.ackResources[*]}{.kind}{" "}{.namespace}/{.name}{"\n"}{end}'
kubectl get awsserviceaccountrole <name> -n <workload-namespace> \
  -o jsonpath='{range .status.ackResources[*]}{.kind}{" "}{.namespace}/{.name}{"\n"}{end}'
kubectl get roles.iam.services.k8s.aws,policies.iam.services.k8s.aws,\
openidconnectproviders.iam.services.k8s.aws,buckets.s3.services.k8s.aws,\
podidentityassociations.eks.services.k8s.aws -n <workload-namespace>
```

Fix:

- Check the relevant ACK controller logs and AWS credentials.
- Delete or fix only ACK CRs owned by the operator. ACK remains responsible for
  AWS-side create and delete completion.
- For `EKSIRSA` with `management: Managed`, use `External` when an IAM OIDC
  provider for the same issuer already exists outside the operator.

## `OIDCObjectsPublishFailed` Or `IssuerReconcileFailed`

Likely signal:

- Self-hosted issuer readiness fails after the S3 bucket exists.

Check:

```sh
kubectl get awsworkloadidentityconfig default -n <workload-namespace> \
  -o jsonpath='{.status.selfHostedIssuer.bucketName}{"\n"}{.status.selfHostedIssuer.publication.objectSetDigest}{"\n"}{.status.issuerHostPath}{"\n"}'
kubectl logs -n aws-workload-identity-operator-system deploy/aws-workload-identity-operator \
  --since=30m | grep -E 'OIDCObjects|IssuerReconcileFailed|s3'
```

Fix:

- Give the manager IAM role `s3:GetObject`, `s3:PutObject`, and
  `s3:DeleteObject` only for `.well-known/openid-configuration` and
  `keys.json` in generated issuer buckets.
- Keep S3 bucket and bucket policy ownership in ACK S3 CRs.

## `RemoteDeliveryPending`

Likely signal:

- `ServiceAccountAnnotationReady=False` for `SelfHostedIRSA` or `EKSIRSA`.

Check:

```sh
kubectl get awsserviceaccountrole <name> -n <workload-namespace> \
  -o jsonpath='{.spec.serviceAccount.namespace}{"/"}{.spec.serviceAccount.name}{"\n"}'
kubectl logs -n aws-workload-identity-operator-system deploy/aws-workload-identity-operator \
  --since=30m | grep -E 'ServiceAccountAnnotationReady|RemoteDeliveryPending'
```

Fix:

- Create the remote `ServiceAccount` named by `spec.serviceAccount`.
- Confirm the remote cluster is reachable through Cluster Inventory.
- Keep workload Pods annotation-free; use normal `serviceAccountName`
  references and let the operator deliver AWS annotations when the delivery
  type requires it.

## `InvalidSpec`

Likely signal:

- Duplicate live `AWSServiceAccountRole` objects bind the same remote
  `ServiceAccount`.
- `EKSIRSA` external provider ARN does not match the issuer URL.
- Required delivery-specific cluster facts are missing or malformed.

Check:

```sh
kubectl get awsserviceaccountrole -n <workload-namespace> -o wide
kubectl get awsworkloadidentityconfig default -n <workload-namespace> -o yaml
kubectl get clusterprofile -A \
  -l open-cluster-management.io/cluster-name=<workload-namespace> -o yaml
```

Fix:

- Delete or change duplicate role bindings.
- For `EKSIRSA`, make
  `spec.eksIRSA.oidcProvider.arn` match the issuer host and path.
- For `EKSPodIdentity`, publish required EKS facts through
  `ClusterProfile.status.properties`.

## `PodIdentityAgentReady=Unknown`

Likely signal:

- `EKSPodIdentity` reconciliation cannot prove that the EKS Pod Identity Agent
  is present.

Check:

```sh
kubectl get clusterprofile -A \
  -l open-cluster-management.io/cluster-name=<workload-namespace> \
  -o jsonpath='{range .items[*].status.properties[*]}{.name}={.value}{"\n"}{end}'
```

Fix:

- Publish `aws.identity.appthrust.io/eks-auto-mode=true` for EKS Auto Mode
  clusters.
- Otherwise install and validate the EKS Pod Identity Agent on the target
  cluster.

## ReplicaSet Rollout Reasons

Likely signals:

- `PlacementResolved=False` with `PlacementUnavailable`.
- `AWSServiceAccountRolesApplied=False` with `ChildApplyFailed` or
  `ChildConflict`.
- `Ready=False` with `ChildrenPending` or `RolloutTimedOut`.

Check:

```sh
kubectl get placement,placementdecision -n <namespace>
kubectl get awsserviceaccountrolereplicaset <name> -n <namespace> -o yaml
kubectl get awsserviceaccountrole -A \
  -l aws.identity.appthrust.io/replicaset-uid=<replicaset-uid>
```

Fix:

- Repair same-namespace OCM Placement and PlacementDecision availability.
- Remove foreign same-name child roles that are not owned by the ReplicaSet, or
  rename the ReplicaSet.
- Inspect each child role status; child readiness remains owned by the
  `AWSServiceAccountRole` controller.

## IRSA Sidecar

Likely signal:

- A hosted controller using `aws-irsa-sidecar` cannot assume the role.

Check:

```sh
aws-irsa-sidecar check \
  --token-file=/var/run/aws-irsa/token \
  --aws-config-file=/var/run/aws-irsa/config
```

Fix:

- Mount a kubeconfig for the managed cluster, not the hub.
- Grant the remote identity `get serviceaccounts`, `create
  serviceaccounts/token`, and `create selfsubjectreviews`.
- Set `AWS_CONFIG_FILE` and an AWS region on the workload container. Some SDKs
  also need `AWS_SDK_LOAD_CONFIG=1`.
