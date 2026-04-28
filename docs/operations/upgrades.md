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

## Release Pinning

Published chart versions and image tags are immutable artifacts.
Production-like environments should pin exact chart versions and image digests,
and upgrade notes should be added here whenever an operator action is required.
