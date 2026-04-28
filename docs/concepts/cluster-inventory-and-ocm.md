# Cluster Inventory And OCM

The operator uses Cluster Inventory API `ClusterProfile` objects to resolve
target cluster facts and remote Kubernetes access.

## OCM Resolution

With OCM, `ClusterProfile` objects are resolved by the
`open-cluster-management.io/cluster-name` label. The operator namespace should
only need a normal `ManagedClusterSetBinding`.

Operators rely on the target cluster label and manager identity. The namespace
shown here is the namespace where Cluster Inventory projects the object in this
example; OCM resolution uses the
`open-cluster-management.io/cluster-name` label, not operator-namespace
ownership.

```yaml
apiVersion: multicluster.x-k8s.io/v1alpha1
kind: ClusterProfile
metadata:
  name: wlc-a
  namespace: <cluster-inventory-namespace>
  labels:
    open-cluster-management.io/cluster-name: wlc-a
    x-k8s.io/cluster-manager: open-cluster-management
spec:
  displayName: wlc-a
  clusterManager:
    name: open-cluster-management
```

The following `status` shape is observed output from Cluster Inventory and OCM.
Do not apply or patch this status directly:

```yaml
status:
  conditions:
    - type: ControlPlaneHealthy
      status: "True"
      observedGeneration: 1
      lastTransitionTime: "2026-04-30T00:00:00Z"
      reason: Healthy
      message: target cluster API server is reachable
  version:
    kubernetes: v1.36.0
  properties: []
  accessProviders:
    - name: open-cluster-management
      cluster:
        server: https://wlc-a-api.example.com:6443
        certificate-authority-data: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCg==
```

## Access Providers

Cluster Inventory access providers are Kubernetes cluster access only. Do not
publish AWS credentials through `ClusterProfile.status.accessProviders`.

For OCM ManagedServiceAccount credential sync, the Helm chart can create
`ManagedServiceAccount` and remote-permissions `ManifestWork` resources. The
generated provider uses `ocm.managedServiceAccount.name` for the
`--managed-serviceaccount=<name>` argument.

Hosted consumers use the same provider-file path as other Cluster Inventory
consumers. The Helm chart can generate a provider file that points at the OCM
`cp-creds` exec plugin, and `aws-remote-irsa-credential-process` consumes that
file with `--clusterprofile-provider-file`.

## Cluster Facts

For OCM clusters, publish cluster facts through the normal spoke path. Create
`ClusterProperty` objects on the spoke cluster, commonly by delivering them from
the hub with `ManifestWork`. OCM then syncs those claims into
`ClusterProfile.status.properties`.

Consumers and controllers should not patch `ClusterProfile.status` directly.
