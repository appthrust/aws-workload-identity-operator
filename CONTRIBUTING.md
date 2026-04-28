# Contributing

This repository contains a Go Kubernetes operator, Helm chart, Kustomize
manifests, and kest-based end-to-end tests.

## Development Environment

The Makefile runs Go commands with `GOWORK=off` so this module builds
independently from any surrounding workspace.

Common checks:

```sh
make test
make lint
make generate
make verify-generated
make manifests
make helm-lint
make helm-template
make verify-static
make test-kest
```

`make verify-static` checks integration boundaries that should stay true for the
project: product code does not depend on kro, does not import Cluster API types,
and does not construct kubeconfigs directly in reconcilers.

`make generate` regenerates CRDs with `controller-gen` and syncs the Helm chart
CRD copies. `make verify-generated` reruns generation and fails if committed
CRDs are stale.

## Formatting

Format Go code with:

```sh
make fmt
```

Run Go linting with:

```sh
make lint
```

## Helm

Render the default chart:

```sh
make helm-template
```

Render the non-default test values:

```sh
make helm-template-test
```

Lint both value sets:

```sh
make helm-lint
```

The chart lives in `charts/aws-workload-identity-operator`. `values.test.yaml`
exercises non-default chart paths and should remain valid in CI.

## Container Image

Build the manager image locally:

```sh
make docker-build IMAGE=ghcr.io/appthrust/aws-workload-identity-operator:dev
```

GitHub Actions verifies the multi-arch image build on pull requests and pushes
to `main` without publishing to GHCR. Image publishing is owned by the release
workflow. The image build uses released Go modules only; workflows should not
depend on repositories outside this module or extra Docker build contexts.

## Release Automation

Merges to `main` run tagpr. When tagpr creates a version tag, the release
workflow publishes the multi-arch manager image to GHCR and pushes the Helm
chart to `oci://ghcr.io/<owner>/helm-charts/aws-workload-identity-operator`.
The chart version and app version are derived from the release tag without the
leading `v`.

## Dependency Updates

Renovate is configured by `renovate.json`. It tracks GitHub Actions, Go modules,
Node dependencies, Dockerfile base images, and image references embedded in the
Helm chart values.

## Tests

Use Go tests for API, webhook, controller, policy, and rendering units:

```sh
make test
```

Use kest for local end-to-end scenarios:

```sh
make test-kest
```

Use the AWS-gated self-hosted IRSA e2e to exercise the full path against real
AWS STS:

```sh
AWS_PROFILE=your-profile-name AWS_REGION=ap-northeast-1 make e2e-selfhosted-irsa
```

The e2e creates an isolated kind hub and installs OCM cluster-proxy,
managed-serviceaccount, CAPD, ACK IAM/S3 controllers, and the operator. It
creates real AWS IAM/S3 resources, validates `AssumeRoleWithWebIdentity`, then
runs an AWS CLI canary Job through the remote pod identity webhook. It also
verifies ServiceAccount recreation and annotation repair. It requires a local
Docker unix socket, resolved from `DOCKER_HOST`, the current Docker context, or
`/var/run/docker.sock`. It cleans up Kubernetes and AWS resources by default;
if AWIO custom resources cannot be deleted safely, it keeps the hub and
workload clusters for inspection. Generated hub and workload cluster contexts
are merged into the first path in `KUBECONFIG`, or `~/.kube/config` when
`KUBECONFIG` is unset, so tools such as k9s can switch to them while the e2e is
running. Omit `AWS_PROFILE` when using environment-provided AWS credentials or
the AWS CLI's default credential chain.

The non-AWS suite should clean up Kubernetes resources. AWS-gated tests must
also assert cleanup of managed AWS resources and avoid leaving IAM roles, IAM
OIDC providers, or S3 issuer buckets behind.

## Validation Status

Validate these areas in a real environment before production use:

- ClusterProfile-producing registration flow such as OCM/CAPI/CAPD.
- Real ACK controller sync against AWS.
- End-to-end S3 OIDC issuer object publishing and STS validation in a real AWS
  environment.
- EKS Pod Identity Agent readiness and STS canary Jobs.
