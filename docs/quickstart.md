# Quickstart

This page shows the shortest path for one target cluster namespace. It assumes
the Helm chart is installed, ACK controllers are running, and Cluster Inventory
already publishes a reachable `ClusterProfile` for the target cluster.

## 1. Install

> **Status:** Pre-release. The OCI chart referenced from the install guide is
> not published yet. Until tagpr cuts `v0.1.0`, install from a local checkout
> (`./charts/aws-workload-identity-operator`); the `0.1.0` / `v0.1.0` strings
> describe the targeted first release, not a published artifact.

Install from a local chart checkout today, or with the future published chart
version once the first release ships. For the command, chart values, and OCM
access-provider configuration, see [Install With Helm](guides/install-helm.md).

## 2. Configure Platform Defaults

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityOperatorConfig
metadata:
  name: default
spec:
  selfHostedIRSA:
    webhookNamespace: aws-pod-identity-webhook
```

## 3. Configure One Target Namespace

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityConfig
metadata:
  name: default
  namespace: wlc-a
spec:
  type: SelfHostedIRSA
  region: ap-northeast-1
```

## 4. Bind One Remote ServiceAccount

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: aws-load-balancer-controller
  namespace: wlc-a
spec:
  serviceAccount:
    namespace: kube-system
    name: aws-load-balancer-controller
  policyARNs:
    - arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess
```

Watch `AWSWorkloadIdentityConfig.status.conditions` and
`AWSServiceAccountRole.status.conditions`. Condition meanings are in
[Status Conditions](reference/status-conditions.md).
