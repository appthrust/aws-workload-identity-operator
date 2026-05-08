# AWS Workload Identity Operator Helm Chart

This chart installs the AWS Workload Identity Operator, its CRDs, RBAC, webhook
configuration, and Cluster Inventory access-provider configuration.

## Install

```sh
helm upgrade --install aws-workload-identity-operator \
  oci://ghcr.io/appthrust/helm-charts/aws-workload-identity-operator \
  --version <chart-version> \
  --namespace aws-workload-identity-operator-system \
  --create-namespace
```

Use `./charts/aws-workload-identity-operator` when installing local changes from
a source checkout.

The chart installs CRDs by default and deploys the manager, RBAC,
ClusterProfile provider file ConfigMap, Webhook Service, webhook
configuration, and cert-manager `Issuer`/`Certificate` resources for the
serving certificate Secret.

Install cert-manager CRDs and controller before installing this chart. The
webhook TLS resources use cert-manager APIs and are always rendered.

## Cluster Inventory Access Providers

The Deployment passes `--clusterprofile-provider-file` to the manager and mounts
the file from the generated, release-scoped ConfigMap at
`/etc/cluster-inventory/config.json`. The file content is Cluster Inventory API
access provider config.

Default values:

```yaml
clusterInventory:
  accessProviders: []
  plugins:
    - name: open-cluster-management
      # Do not pin cp-creds by SHA yet; upstream digests change frequently while it stabilizes.
      image: quay.io/open-cluster-management/cp-creds:latest
      mountPath: /plugins
      pullPolicy: IfNotPresent
ocm:
  managedServiceAccount:
    name: aws-workload-identity-operator
```

`clusterInventory.plugins[]` uses the Kubernetes image volume type. The chart
validates that every final provider command lives under one of the plugin
mount paths. The chart always generates the standard OCM `cp-creds` provider
using `ocm.managedServiceAccount.name` for the
`--managed-serviceaccount=<name>` arg. `clusterInventory.accessProviders` is
then merged as additional Cluster Inventory providers.

## OCM ManagedServiceAccount Resources

The operator consumes remote access through Cluster Inventory. The chart can
optionally create OCM `ManagedServiceAccount` and remote-permissions
`ManifestWork` resources for clusters that use OCM `cp-creds`, but those
resources are disabled by default because OCM APIs and managed cluster
namespaces are external prerequisites.

```yaml
ocm:
  managedServiceAccount:
    name: aws-workload-identity-operator
    create: true
    namespaces:
      - wlc-a
```

`ocm.managedServiceAccount.name` is used by the generated OCM access provider
and by chart-created `ManagedServiceAccount` objects. `serviceAccount.*`
remains the local operator Pod identity and does not affect OCM
`ManagedServiceAccount` objects.

`ocm.managedServiceAccount.namespaces` is the explicit list of OCM managed
cluster namespaces where the hub `ManagedServiceAccount` and `ManifestWork`
objects should be created. This is the `metadata.namespace` of each
chart-created `ManagedServiceAccount`; the release namespace and
`namespaceOverride` are never used as defaults for managed cluster namespaces.
`ocm.managedServiceAccount.addonInstallNamespace` must match the
`ManagedClusterAddOn/managed-serviceaccount` install namespace because OCM
creates the synced remote ServiceAccount there. The managed-serviceaccount
Helm chart renders `open-cluster-management-managed-serviceaccount` for its
default `hubDeployMode=Deployment` `targetCluster` template. OCM's generic
add-on default, and managed-serviceaccount `hubDeployMode=AddOnTemplate`, use
`open-cluster-management-agent-addon` instead.

The remote permissions ManifestWork bootstraps the webhook namespace and
least-privilege RBAC needed by the operator's remote access identity. It does
not own the self-hosted webhook runtime objects, and it does not grant
`cluster-admin`; some cluster-wide `list/watch` permissions are still required
by the remote caches.
The remote webhook namespace defaults from
`operatorConfig.spec.selfHostedIRSA.webhookNamespace` when the chart creates
`operatorConfig`, otherwise to `aws-pod-identity-webhook`; override
`ocm.managedServiceAccount.remotePermissions.webhookNamespace` only when those
defaults do not match the platform config.

Changing the OCM managed service account name is disruptive because it changes
the `cp-creds` identity, generated OCM resources, and remote RBAC subject.
For live clusters, follow the sequencing in
[Upgrades](../../docs/operations/upgrades.md#ocm-managedserviceaccount-rename).

## Image Values

```yaml
image:
  registry: ghcr.io
  repository: appthrust/aws-workload-identity-operator
  tag: "0.1.0"
  digest: ""
```

Release automation should keep the default `image.tag` aligned with the chart
version. When `image.tag` is empty, the chart uses `appVersion`.

## Runtime Defaults

The manager always renders liveness and readiness probes. The chart also sets
resource requests and a memory limit by default; it intentionally does not set a
CPU limit.

Pre-release builds may change controller composition. Review
[Upgrades](../../docs/operations/upgrades.md) before rolling back live
clusters.

## Values Validation

Helm validates this chart with `values.schema.json`. Required value shape and
conditional requirements, such as `operatorConfig.create=true` and
`serviceMonitor` prerequisites, are kept in the schema instead of ad hoc
template checks.

`values.test.yaml` exercises non-default chart paths and is rendered in CI with
both `helm lint` and `helm template`.

## Metrics

The controller-runtime metrics endpoint is always enabled on the manager Pod.
The chart renders `--metrics-bind-address=:8080` using
`metrics.containerPort`. To create a ClusterIP Service for Prometheus scraping:

```yaml
metrics:
  service:
    enabled: true
```

The metrics Service exposes a port named `metrics` and targets the manager
container port with the same name.

Metric semantics are documented in
[Metrics](../../docs/reference/metrics.md). Version-specific alert migration
notes belong in [Upgrades](../../docs/operations/upgrades.md).

### ServiceMonitor

Prometheus Operator users can render a `ServiceMonitor`:

```yaml
metrics:
  service:
    enabled: true
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

`serviceMonitor.enabled=true` requires `metrics.service.enabled=true` in
`values.schema.json`. The ServiceMonitor endpoint uses `port: metrics`;
`serviceMonitor.labels`, `annotations`, `interval`, `scrapeTimeout`, `path`,
`scheme`, `honorLabels`, `relabelings`, `metricRelabelings`, and
`namespaceSelector` are supported.

## Logging

The chart exposes OpenTelemetry-oriented logging values and renders them as
manager flags:

```text
--log-level=<logging.level>
--log-exporter=<logging.exporter>
--log-add-source=<logging.addSource>
--log-resource-attributes=<merged resource attributes>
```

Default values use `logging.level=info` and `logging.exporter=otlp`. OTLP log
export is configured with OpenTelemetry environment variables rendered from
`logging.otlp.*`:

```yaml
logging:
  level: info
  exporter: otlp
  otlp:
    logsEndpoint: http://otel-collector.observability.svc:4318/v1/logs
  resource:
    serviceName: aws-workload-identity-operator
    serviceNamespace: appthrust
metrics:
  service:
    enabled: true
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

The chart renders `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`,
`OTEL_LOGS_EXPORTER`, and OTLP endpoint/protocol/header variables when the
corresponding values are set. `logging.resource.attributes` is merged with
`service.name`, `service.namespace`, `service.version`, and
`deployment.environment.name`.

Supported exporters are `otlp`, `console`, and `none`:

```yaml
logging:
  level: debug
  exporter: console
  addSource: true
```

Use `console` for development, smoke tests, or deployments that intentionally
collect Pod stdout. It is opt-in because the OpenTelemetry stdout/console
exporter is not treated as a stable production interchange format; OTLP to an
OpenTelemetry Collector is the recommended production path.

## Webhook TLS

The validating webhook is always enabled. Its service shape and admission
settings are fixed by the chart. TLS is managed by cert-manager; the chart
always creates a self-signed `Issuer` and a `Certificate`, and cert-manager
creates the webhook serving certificate Secret.

## AWS Credentials

ACK controllers need AWS credentials for the managed IAM, S3, and EKS resources.
For `SelfHostedIRSA`, the operator manager also writes and deletes the
`.well-known/openid-configuration` and `keys.json` S3 objects with the S3 API.
Provide manager AWS credentials through your platform identity mechanism or with
`extraEnvVars` / `extraEnvVarsSecret`.

To use an AWS-compatible endpoint for the manager's direct AWS API calls, set:

```yaml
aws:
  endpointURL: https://aws-endpoint.example.com
```

For HTTP endpoints, also set `aws.allowUnsafeEndpointURLs=true`. This overrides
the manager's AWS API endpoint only; the public `SelfHostedIRSA` issuer URL
remains the regional S3 HTTPS URL for the generated bucket.

## Operator Config

The operator config is cluster-scoped and disabled by default. Create
`AWSWorkloadIdentityOperatorConfig/default` separately, or set
`operatorConfig.create=true`, before expecting controllers to report ready.
For `SelfHostedIRSA`, `operatorConfig.spec.selfHostedIRSA.webhookNamespace` is
the source of truth for the remote webhook namespace.

```yaml
operatorConfig:
  create: true
  spec:
    permissionsBoundaryARN: arn:aws:iam::123456789012:policy/appthrust-workload-identity-boundary
    selfHostedIRSA:
      webhookNamespace: aws-pod-identity-webhook
```

`permissionsBoundaryARN` is optional; the operator sets a permissions boundary
only when the value is configured.

## Workload Admission Policy

The chart's validating webhook is separate from any platform policy you use for
workload IAM permissions. This chart does not install Kyverno policies or other
external admission rules.

To restrict `AWSServiceAccountRole.spec.policyARNs` or
`AWSServiceAccountRole.spec.policyDocument`, use an admission policy engine.
See [Restrict IAM Policy Inputs](../../docs/guides/restrict-iam-policy-inputs.md)
for a Kyverno example.
