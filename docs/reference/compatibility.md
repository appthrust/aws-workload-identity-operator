# Compatibility And Prerequisites

`v0.1.0` is the first public release line. The chart version is `0.1.0`, and
the release tag is `v0.1.0`.

## Platform Matrix

| Component | Requirement | Notes |
| --- | --- | --- |
| Kubernetes | `>=1.35` | Enforced by the Helm chart `kubeVersion`. Target clusters also need TokenRequest support for IRSA sidecar flows. |
| Helm | Helm 3 with OCI registry support | Install from `oci://ghcr.io/appthrust/helm-charts/aws-workload-identity-operator`. |
| cert-manager | Installed before the chart | The chart always renders `Issuer` and `Certificate` resources for the validating webhook serving certificate. |
| Cluster Inventory API | Installed on the hub | Controllers resolve `ClusterProfile` objects and use access providers as the source of remote `rest.Config`. |
| OCM ManagedServiceAccount | Required for the chart-generated OCM `cp-creds` provider | The operator namespace should need only a normal `ManagedClusterSetBinding`. The chart can create `ManagedServiceAccount` and remote-permissions `ManifestWork` objects when enabled. |
| ACK IAM controller | Required for all delivery types | Owns IAM Role and generated IAM Policy CRs. Also owns IAM OpenIDConnectProvider CRs for `SelfHostedIRSA` and managed-provider `EKSIRSA`. |
| ACK S3 controller | Required for `SelfHostedIRSA` | Owns the S3 Bucket and bucket policy CRs. The manager verifies, writes, and deletes only discovery and JWKS objects with the AWS S3 API. |
| ACK EKS controller | Required for `EKSPodIdentity` | Owns EKS PodIdentityAssociation CRs. |
| AWS credentials | Required for ACK controllers; manager S3 object permissions for `SelfHostedIRSA` | Scope examples are in [IAM Permissions](iam-permissions.md). |

## Delivery Prerequisites

| Delivery type | Required target facts | Required remote access |
| --- | --- | --- |
| `SelfHostedIRSA` | `ClusterProfile` reachability and AWS region from `spec.region` or cluster facts for consumers | Manager can apply the self-hosted webhook runtime and annotate remote ServiceAccounts. |
| `EKSIRSA` | `spec.eksIRSA.issuerURL`; provider management choice and optional external provider ARN | Manager can annotate remote ServiceAccounts. |
| `EKSPodIdentity` | `aws.identity.appthrust.io/eks-cluster-name`, `aws.identity.appthrust.io/eks-cluster-arn`, and `aws.identity.appthrust.io/aws-account-id` in `ClusterProfile.status.properties` | ACK EKS can reconcile PodIdentityAssociation resources. |

Optional `EKSPodIdentity` facts:

| Property | Effect |
| --- | --- |
| `aws.identity.appthrust.io/aws-organization-id` | Adds an `aws:SourceOrgId` condition to generated trust policies. |
| `aws.identity.appthrust.io/eks-auto-mode` | When `true`, lets the operator report `PodIdentityAgentReady=True` for EKS Auto Mode clusters. |

## Versioned Artifacts

| Artifact | `v0.1.0` reference |
| --- | --- |
| Helm chart | `oci://ghcr.io/appthrust/helm-charts/aws-workload-identity-operator --version 0.1.0` |
| Operator image tag default | `ghcr.io/appthrust/aws-workload-identity-operator:0.1.0` |
| Remote IRSA tools examples | `ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:v0.1.0` |
| AWS IRSA sidecar examples | `ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:v0.1.0` |
