# Fleet Bindings

Use `AWSServiceAccountRoleReplicaSet` when an OCM `Placement` should create one
`AWSServiceAccountRole` child per selected cluster namespace.

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRoleReplicaSet
metadata:
  name: aws-load-balancer-controller
  namespace: platform-workloads
spec:
  placementRefs:
    - apiGroup: cluster.open-cluster-management.io
      kind: Placement
      name: prod-clusters
  template:
    spec:
      serviceAccount:
        namespace: kube-system
        name: aws-load-balancer-controller
      policyARNs:
        - arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

The ReplicaSet only fans out hub-side child role objects. IAM resources, remote
ServiceAccount annotation delivery, and child role status remain owned by each
per-cluster `AWSServiceAccountRole` controller.

Child ownership is label-based because children live in selected cluster
namespaces, not necessarily the parent namespace. A same-name child without the
expected ownership labels is reported as a conflict and is not patched or
deleted.
