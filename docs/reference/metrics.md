# Metrics

The controller-runtime metrics endpoint is always enabled on the manager Pod.
The Helm chart renders `--metrics-bind-address=:8080` using
`metrics.containerPort`.

To create a ClusterIP Service for Prometheus scraping:

```yaml
metrics:
  service:
    enabled: true
```

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

`selfhosted-serviceaccount` reconciles only previously annotated
ServiceAccounts for drift repair and drops delete events. Monitor
`awio_predicate_decision_total{controller="selfhosted-serviceaccount"}` for
keep/drop behavior and `awio_remote_delivery_total{reason="index_lookup_failed"}`
or `{reason="channel_full"}` for the annotated ServiceAccount delete enqueue
path.
