# Install With Helm

Install the chart from GHCR's OCI registry:

```sh
helm upgrade --install aws-workload-identity-operator \
  oci://ghcr.io/appthrust/helm-charts/aws-workload-identity-operator \
  --version 0.1.1 \
  --namespace aws-workload-identity-operator-system \
  --create-namespace
```

Use `./charts/aws-workload-identity-operator` when installing local changes from
a source checkout. When testing an image that is not available from GHCR, use
the [local image override](#local-image-override) below.

The release tag is `v0.1.1`; the Helm chart version is `0.1.1`. Future release
pull requests update this command through tagpr version sync.

The chart installs CRDs, RBAC, the manager Deployment, webhook configuration,
cert-manager serving certificate resources, and the Cluster Inventory
access-provider file by default. Detailed value semantics live in the
[chart README](../../charts/aws-workload-identity-operator/README.md).
Prerequisites are summarized in
[Compatibility And Prerequisites](../reference/compatibility.md).

## Local Image Override

For a local checkout install, build the manager image, make it available to the
target cluster, and override the chart `image` values so the manager Pod pulls
the image you built.

Build the manager image locally:

```sh
make docker-build IMAGE=ghcr.io/appthrust/aws-workload-identity-operator:dev
```

Make the image available to the cluster (for example, `kind load docker-image`
for a kind cluster) and install the local chart with matching `image` values:

```sh
helm upgrade --install aws-workload-identity-operator \
  ./charts/aws-workload-identity-operator \
  --namespace aws-workload-identity-operator-system \
  --create-namespace \
  --set image.registry=ghcr.io \
  --set image.repository=appthrust/aws-workload-identity-operator \
  --set image.tag=dev \
  --set image.pullPolicy=IfNotPresent
```

Use `image.pullPolicy=Never` when the image only exists in the local container
runtime and must not be pulled from a remote registry.

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
