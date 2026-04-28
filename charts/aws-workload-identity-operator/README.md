# AWS Workload Identity Operator Helm Chart

This chart installs the AWS Workload Identity Operator, its CRDs, RBAC, webhook
configuration, and Cluster Inventory access-provider configuration.

## Install

```sh
helm upgrade --install aws-workload-identity-operator ./charts/aws-workload-identity-operator \
  --namespace aws-workload-identity-operator-system \
  --create-namespace
```

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
  accessProvidersConfig:
    providers:
      - name: open-cluster-management
        execConfig:
          apiVersion: client.authentication.k8s.io/v1
          command: /plugins/cp-creds
          args:
            - --managed-serviceaccount=aws-workload-identity-operator
          provideClusterInfo: true
          interactiveMode: Never
  plugins:
    - name: open-cluster-management
      # Do not pin cp-creds by SHA yet; upstream digests change frequently while it stabilizes.
      image: quay.io/open-cluster-management/cp-creds:latest
      mountPath: /plugins
      pullPolicy: IfNotPresent
```

`clusterInventory.plugins[]` uses the Kubernetes image volume type. The chart
validates that every configured provider command lives under one of the plugin
mount paths. The default provider is OCM `cp-creds`; add other Cluster
Inventory access-provider plugins explicitly only when your deployment uses
them.

## Image Values

```yaml
image:
  registry: ghcr.io
  repository: appthrust/aws-workload-identity-operator
  tag: "0.2.0"
  digest: ""
```

Release automation keeps the default `image.tag` aligned with the chart
version. When `image.tag` is empty, the chart uses `appVersion`.

## Runtime Defaults

The manager always renders liveness and readiness probes. The chart also sets
resource requests and a memory limit by default; it intentionally does not set a
CPU limit.

Rollback to a chart before 0.2.0 removes the `selfhosted-role-enqueue`
controller, so annotated ServiceAccount delete events no longer kick the
role-controller retry loop. Existing remote ServiceAccount annotations are not
removed by rollback; remove them manually with
`kubectl annotate sa <name> eks.amazonaws.com/role-arn-` only when the
corresponding role binding is no longer desired.

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

After 0.2.0, `selfhosted-serviceaccount` reconciles only previously annotated
ServiceAccounts for drift repair and drops delete events. Alert rules based on
steady `controller_runtime_reconcile_total{controller="selfhosted-serviceaccount"}`
traffic should be adjusted. Watch
`awio_predicate_decision_total{controller="selfhosted-serviceaccount"}` for
keep/drop behavior and `awio_remote_delivery_total{reason="index_lookup_failed"}`
or `{reason="channel_full"}` for the annotated ServiceAccount delete enqueue
path.

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

For HTTP endpoints, also set `aws.allowUnsafeEndpointURLs=true`. If the S3
endpoint requires path-style addressing, set `s3.usePathStyle=true`. This
overrides the manager's AWS API endpoint only; the public `SelfHostedIRSA`
issuer URL remains the regional S3 HTTPS URL for the generated bucket.

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
The main [README](../../README.md#restrict-iam-policy-inputs) includes a
Kyverno example.
