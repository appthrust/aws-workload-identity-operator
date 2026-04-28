# Install With Helm

Install the first public chart from GHCR's OCI registry:

```sh
helm upgrade --install aws-workload-identity-operator \
  oci://ghcr.io/appthrust/helm-charts/aws-workload-identity-operator \
  --version 0.1.0 \
  --namespace aws-workload-identity-operator-system \
  --create-namespace
```

Use `./charts/aws-workload-identity-operator` when installing local changes from
a source checkout.

The release tag is `v0.1.0`; the Helm chart version is `0.1.0`. Future release
pull requests update this command through tagpr version sync.

The chart installs CRDs, RBAC, the manager Deployment, webhook configuration,
cert-manager serving certificate resources, and the Cluster Inventory
access-provider file by default. Detailed value semantics live in the
[chart README](../../charts/aws-workload-identity-operator/README.md).
Prerequisites are summarized in
[Compatibility And Prerequisites](../reference/compatibility.md).

## OCM Access Provider

The chart generates the OCM `cp-creds` Cluster Inventory access provider and
merges it with any additional `clusterInventory.accessProviders`.

```yaml
clusterInventory:
  accessProviders: []
  plugins:
    - name: open-cluster-management
      image: quay.io/open-cluster-management/cp-creds:latest
      mountPath: /plugins
ocm:
  managedServiceAccount:
    name: aws-workload-identity-operator
```

The `cp-creds` image intentionally uses `latest` while the upstream image is
still stabilizing.

## Chart-Created ManagedServiceAccount Resources

When enabling chart-created OCM `ManagedServiceAccount` resources, the generated
OCM provider automatically uses the same name:

```yaml
ocm:
  managedServiceAccount:
    name: custom-awio
    create: true
    namespaces:
      - wlc-a
```

Changing the managed service account name is disruptive. Plan the sequence in
[Upgrades](../operations/upgrades.md).
