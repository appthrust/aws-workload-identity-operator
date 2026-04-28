# Troubleshooting

Start with status conditions:

```sh
kubectl get awsworkloadidentityconfig default -n <workload-namespace> -o yaml
kubectl get awsserviceaccountrole <name> -n <workload-namespace> -o yaml
```

Condition meanings are in [Status Conditions](../reference/status-conditions.md).

## Target Cluster Access

If `ClusterProfileResolved` or `InventoryResolved` is false:

- confirm whether the direct `ClusterProfile` path
  `<workload-namespace>/<workload-namespace>` exists,
- for OCM, confirm a reachable `ClusterProfile` has the expected
  `open-cluster-management.io/cluster-name=<workload-namespace>` and
  `x-k8s.io/cluster-manager=open-cluster-management` labels,
- confirm Cluster Inventory access providers can produce a remote
  Kubernetes `rest.Config`,
- do not add remote kubeconfig Secrets for reconcilers to read directly.

## SelfHostedIRSA

If issuer readiness fails, check ACK S3 Bucket and IAM OpenIDConnectProvider CR
conditions in the workload namespace. The manager also needs direct S3
`PutObject` and `DeleteObject` permissions for the OIDC discovery and JWKS
objects.

If delivery readiness fails, check that the target cluster is reachable and that
the remote ServiceAccount exists. Initial annotation delivery is retry-driven by
the role controller when the role exists before the remote ServiceAccount.

## EKS Pod Identity

If `PodIdentityAssociationReady` fails, check the ACK EKS
PodIdentityAssociation CR. If `PodIdentityAgentReady` remains `Unknown`, confirm
whether the target `ClusterProfile` declares
`aws.identity.appthrust.io/eks-auto-mode=true`.

## Duplicate Bindings

If multiple live `AWSServiceAccountRole` objects in the same namespace bind the
same remote ServiceAccount, each conflicting role reports `InvalidSpec`. Delete
or change the duplicate binding; the controller does not choose a winner.
