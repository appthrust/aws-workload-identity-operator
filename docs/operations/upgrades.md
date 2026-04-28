# Upgrades

This page collects operational sequencing that should not be duplicated across
README files.

## OCM ManagedServiceAccount Rename

Changing `ocm.managedServiceAccount.name` is disruptive because it changes the
`cp-creds` identity, generated OCM resources, and remote RBAC subject.

For live clusters:

1. Create and wait for the new ManagedServiceAccount credential.
2. Roll the operator with the matching Cluster Inventory provider argument.
3. Confirm remote access is healthy.
4. Remove the old ManagedServiceAccount and RBAC only after workload identity
   finalizers no longer need the old credential.

## `awio_remote_delivery_total` Reason Rename

The `reason` label value on `awio_remote_delivery_total` was renamed from
`not_self_hosted` to `delivery_type_mismatch`. The previous name framed every
non-matching delivery type through the self-hosted-only lens and disagreed with
the `delivery_type` label whenever the rejected config was `EKSIRSA` or
`EKSPodIdentity`. The new value reflects what the counter actually records:
the controller's expected delivery type did not match the matched config's
`Spec.Type`.

Before upgrading the operator:

1. Update Prometheus alerts, recording rules, and PromQL dashboards that match
   `reason="not_self_hosted"` to match `reason="delivery_type_mismatch"`
   instead.
2. Update any external tooling (runbooks, log queries, CI assertions) that
   pinned the old string.

The series for the old value will simply stop incrementing after the upgrade.
No new series is emitted under the old name, so stale alerts referencing
`not_self_hosted` will silently never fire.

## Release Pinning

Published chart versions and image tags are immutable artifacts.
Production-like environments should pin exact chart versions and image digests,
and upgrade notes should be added here whenever an operator action is required.
