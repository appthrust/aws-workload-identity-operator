# Metrics

The manager exposes controller-runtime metrics on `/metrics`. The Helm chart
renders `--metrics-bind-address=:8080` from `metrics.containerPort` and can
create a Service and Prometheus Operator `ServiceMonitor`.

```yaml
metrics:
  service:
    enabled: true
serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack
```

The endpoint is authenticated and authorized by controller-runtime. Scrapers
must present a bearer token whose Kubernetes identity can `get` the
non-resource URL `/metrics`.

## AWIO Metrics

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `awio_child_apply_total` | Counter | `controller`, `child_kind`, `operation`, `result` | Hub-side child create-or-update operations for generated ACK CRs, signing Secrets, and ReplicaSet children. |
| `awio_condition_transition_total` | Counter | `kind`, `condition`, `status`, `reason` | Status condition transitions recorded after a status patch succeeds. |
| `awio_remote_delivery_total` | Counter | `delivery_type`, `resource`, `result`, `reason` | Remote-cluster delivery operations and enqueue paths. |
| `awio_predicate_decision_total` | Counter | `controller`, `decision` | Bounded keep/drop decisions made by predicates. |

## Label Bounds

Unrecognized or unsafe label values are coerced to `other`; empty values are
coerced to `unknown`. Condition messages and provider errors are intentionally
not used as metric labels.

| Label | Bounded values |
| --- | --- |
| `controller` | `AWSWorkloadIdentityConfig`, `AWSServiceAccountRole`, `AWSServiceAccountRoleReplicaSet`, `selfhosted-role-enqueue`, `selfhosted-serviceaccount`, `selfhosted-webhook-runtime` |
| `child_kind` | Observed generated kinds such as `Bucket`, `OpenIDConnectProvider`, `Role`, `Policy`, `PodIdentityAssociation`, `Secret`, and `AWSServiceAccountRole`; other valid short labels pass through only if they are under 64 characters. |
| `operation` | `created`, `updated`, `updatedStatus`, `updatedStatusOnly`, `unchanged`, `unknown`, or `other`. |
| `result` | `success`, `error`, or `skipped` for remote delivery; child apply uses `success` or `error`. |
| `delivery_type` | `SelfHostedIRSA`, `EKSIRSA`, `EKSPodIdentity`, `unknown`, or `other`. |
| `resource` | Observed remote resources such as `ServiceAccount`, `WebhookRuntime`, `Namespace`, `Deployment`, `Secret`, `MutatingWebhookConfiguration`, and webhook RBAC resources; other valid short labels pass through only if they are under 64 characters. |
| `decision` | `kept` or `dropped`. |
| `condition` | Kubernetes condition type strings under 64 safe characters, otherwise `other`. |
| `status` | `True`, `False`, `Unknown`, or `other`. |
| `reason` | Stable API condition reasons plus remote delivery reasons: `waiting_inventory`, `not_self_hosted`, `cluster_unavailable`, `no_inventory_namespace`, `apply_failed`, `index_lookup_failed`, `enqueued`, `channel_full`, and `stale_cluster_event`. |

Remote apply successes use the controller-runtime operation result as the
`reason`, for example `created` or `unchanged`.

## PromQL Examples

Remote delivery errors by reason:

```promql
sum by (delivery_type, resource, reason) (
  rate(awio_remote_delivery_total{result="error"}[5m])
)
```

Configs or roles transitioning away from ready:

```promql
sum by (kind, condition, reason) (
  increase(awio_condition_transition_total{condition="Ready",status!="True"}[15m])
)
```

ReplicaSet child apply failures:

```promql
sum by (child_kind, operation) (
  rate(awio_child_apply_total{controller="AWSServiceAccountRoleReplicaSet",result="error"}[5m])
)
```

Dropped ServiceAccount drift-repair events:

```promql
sum by (decision) (
  rate(awio_predicate_decision_total{controller="selfhosted-serviceaccount"}[5m])
)
```

Annotated ServiceAccount delete enqueue backpressure:

```promql
sum by (reason) (
  rate(awio_remote_delivery_total{resource="ServiceAccount",reason=~"index_lookup_failed|channel_full"}[5m])
)
```
