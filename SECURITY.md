# Security Policy

`aws-workload-identity-operator` brokers AWS IAM identity for Kubernetes
workloads across a fleet of clusters. Defects in the operator, its Helm chart,
or the hub-side remote IRSA helpers can have cross-tenant or cross-cluster
impact, so we treat security reports as a priority.

## Reporting a Vulnerability

Please report suspected vulnerabilities privately. Do not open a public GitHub
issue, discussion, or pull request for a suspected vulnerability.

Use GitHub's private vulnerability reporting for this repository:

- <https://github.com/appthrust/aws-workload-identity-operator/security/advisories/new>

If GitHub private reporting is unavailable to you, open a minimal public issue
that asks for a private channel without describing the vulnerability, and a
maintainer will follow up.

A useful report typically includes:

- The affected version, commit, or release tag.
- The affected component (operator manager, Helm chart, `remote-irsa-tools`,
  `aws-irsa-sidecar`, or webhook runtime).
- Reproduction steps or a proof of concept.
- The observed impact and, where possible, the expected behavior.
- The configuration that triggers the issue: delivery type (`SelfHostedIRSA`,
  `EKSIRSA`, `EKSPodIdentity`), Cluster Inventory access provider, and any
  relevant ACK controllers.

## Scope

In scope for this policy:

- The operator manager image and Go module in this repository.
- The Helm chart under `charts/aws-workload-identity-operator`.
- Hub-side remote IRSA helper binaries and images (`remote-irsa-tools`,
  `aws-irsa-sidecar`) and the `pkg/remoteirsa` Go API.
- The self-hosted IRSA webhook runtime that the operator manages on target
  clusters.
- The generated CRDs, validating webhook, and admission behavior.

Out of scope:

- Vulnerabilities in upstream dependencies (AWS Controllers for Kubernetes,
  Open Cluster Management, cert-manager, controller-runtime, AWS SDK,
  Kubernetes itself). Please report those upstream. If the vulnerability is
  exposed specifically by how this operator uses an upstream component,
  it is in scope.
- AWS account, IAM policy, or S3 bucket configuration issues that exist
  independently of the operator's generated resources. See
  [`docs/guides/restrict-iam-policy-inputs.md`](docs/guides/restrict-iam-policy-inputs.md)
  and [`docs/reference/iam-permissions.md`](docs/reference/iam-permissions.md)
  for the platform-side controls.

## Supported Versions

`aws-workload-identity-operator` has not yet shipped a public release. Until
the first tag is cut, security fixes land on `main` and there is no supported
release line. Once the first public release is published, the latest minor
release line will be supported for security fixes; older release lines will
be considered case by case.

## Coordinated Disclosure

We aim to acknowledge new reports within five business days and to keep the
reporter informed as triage, fix, and release work proceeds. We prefer
coordinated disclosure: please give us a reasonable window to ship a fix and
publish an advisory before any public write-up. When a fix ships, the
corresponding GitHub Security Advisory will credit the reporter unless they
ask to remain anonymous.

## Related Documents

- [Security model](docs/concepts/security-model.md)
- [IAM permissions](docs/reference/iam-permissions.md)
- [Restrict IAM policy inputs](docs/guides/restrict-iam-policy-inputs.md)
