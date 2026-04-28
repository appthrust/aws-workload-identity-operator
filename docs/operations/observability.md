# Observability

The manager exposes controller-runtime metrics and supports
OpenTelemetry-oriented logging configuration through Helm values.

## Metrics

Enable a metrics Service and optional Prometheus Operator `ServiceMonitor` with
chart values:

```yaml
metrics:
  service:
    enabled: true
serviceMonitor:
  enabled: true
```

Metric details are in [Metrics](../reference/metrics.md). Chart value details
are in the [chart README](../../charts/aws-workload-identity-operator/README.md#metrics).

## Logging

Production deployments should export OTLP logs to an OpenTelemetry Collector:

```yaml
logging:
  level: info
  exporter: otlp
  otlp:
    logsEndpoint: http://otel-collector.observability.svc:4318/v1/logs
```

Use `logging.exporter=console` for development, smoke tests, or deployments
that intentionally collect Pod stdout.
