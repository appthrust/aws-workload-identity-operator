# Hub-Side Remote IRSA Consumers

Hub-side consumers can use a `SelfHostedIRSA` binding to obtain AWS temporary
credentials for a remote `ServiceAccount` without running a workload Pod on the
target cluster.

This is an advanced integration path for controllers and tools. Normal
workloads should use `serviceAccountName` on the target cluster and let the
operator deliver AWS identity through the selected delivery type.

## Contract

The consumer should:

- use Cluster Inventory `ClusterProfile.status.accessProviders` only to build
  the remote Kubernetes `rest.Config`,
- request a fresh remote `serviceaccounts/token` with audience
  `sts.amazonaws.com`,
- exchange that token with AWS STS `AssumeRoleWithWebIdentity`,
- use the IAM role recorded on `AWSServiceAccountRole.status.roleARN`.

Do not publish AWS credentials as a `ClusterProfile` access provider.
`ClusterProfile.status.accessProviders` remains Kubernetes cluster access only;
`AWSWorkloadIdentityConfig/default` and `AWSServiceAccountRole` remain the AWS
identity contract.

This flow only applies when `AWSWorkloadIdentityConfig/default` has
`spec.type: SelfHostedIRSA`.

## Go SDK Usage

Construct the provider once, wrap it in the AWS SDK credential cache, and let
each refresh request a new remote web identity token. When `Options.Region` is
not set, the provider resolves the STS region from
`ClusterProfile.status.properties["aws.identity.appthrust.io.aws-region"]` and
then falls back to `AWSWorkloadIdentityConfig/default.spec.region`.

```go
package main

import (
	"context"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

func loadAWSConfig(
	ctx context.Context,
	hubReader client.Reader,
	hubKubeClient kubernetes.Interface,
	accessConfig *access.Config,
) (awssdk.Config, error) {
	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:                  hubReader,
		HubKubeClient:              hubKubeClient,
		ClusterProfileAccessConfig: accessConfig,
		WorkloadNamespace:          "wlc-a",
		ServiceAccount: types.NamespacedName{
			Namespace: "kube-system",
			Name:      "aws-load-balancer-controller",
		},
		SessionName: "aws-load-balancer-controller",
	})
	if err != nil {
		return awssdk.Config{}, err
	}

	return config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(awssdk.NewCredentialsCache(provider)),
	)
}
```

## credential_process Usage

Non-Go consumers can use the helper as an AWS `credential_process`:

```ini
[profile remote-irsa]
region = ap-northeast-1
credential_process = aws-remote-irsa-credential-process \
  --namespace wlc-a \
  --service-account kube-system/aws-load-balancer-controller \
  --session-name aws-load-balancer-controller \
  --clusterprofile-provider-file /etc/clusterprofile-provider-file.json
```

For OCM ManagedServiceAccount credential sync, use the same Cluster Inventory
provider file that the chart writes for `cp-creds`. The provider name must match
`ClusterProfile.status.accessProviders[].name`, and the managed service account
name must match the synced OCM `ManagedServiceAccount`.

```json
{
  "providers": [
    {
      "name": "open-cluster-management",
      "execConfig": {
        "apiVersion": "client.authentication.k8s.io/v1",
        "command": "/plugins/cp-creds",
        "args": ["--managed-serviceaccount=aws-workload-identity-operator"],
        "provideClusterInfo": true,
        "interactiveMode": "Never"
      }
    }
  ]
}
```

## Cluster Facts

Remote IRSA can use `aws.identity.appthrust.io.aws-region` from
`ClusterProfile.status.properties` as the STS region. Publish cluster facts
through the normal Cluster Inventory and OCM path described in
[Cluster Inventory And OCM](../concepts/cluster-inventory-and-ocm.md#cluster-facts).

## Out-Of-Cluster Consumer

```go
package main

import (
	"context"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

func loadAWSConfigFromOCMManagedServiceAccount(
	ctx context.Context,
	hubKubeconfigPath string,
	providerFilePath string,
) (awssdk.Config, error) {
	hubRESTConfig, err := clientcmd.BuildConfigFromFlags("", hubKubeconfigPath)
	if err != nil {
		return awssdk.Config{}, err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return awssdk.Config{}, err
	}
	if err := identityv1.AddToScheme(scheme); err != nil {
		return awssdk.Config{}, err
	}
	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		return awssdk.Config{}, err
	}

	hubReader, err := client.New(hubRESTConfig, client.Options{Scheme: scheme})
	if err != nil {
		return awssdk.Config{}, err
	}
	hubKubeClient, err := kubernetes.NewForConfig(hubRESTConfig)
	if err != nil {
		return awssdk.Config{}, err
	}
	accessConfig, err := access.NewFromFile(providerFilePath)
	if err != nil {
		return awssdk.Config{}, err
	}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:                  hubReader,
		HubKubeClient:              hubKubeClient,
		ClusterProfileAccessConfig: accessConfig,
		WorkloadNamespace:          "wlc-a",
		ServiceAccount: types.NamespacedName{
			Namespace: "kube-system",
			Name:      "aws-load-balancer-controller",
		},
		SessionName: "aws-load-balancer-controller",
	})
	if err != nil {
		return awssdk.Config{}, err
	}

	return config.LoadDefaultConfig(ctx,
		config.WithCredentialsProvider(awssdk.NewCredentialsCache(provider)),
	)
}
```

## Required Remote RBAC

Grant the hub consumer's remote access identity narrow permission to create a
token only for the intended target `ServiceAccount`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: remote-irsa-tokenrequest
  namespace: kube-system
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["aws-load-balancer-controller"]
    verbs: ["create"]
```

## Security Notes

Do not log or persist the Kubernetes web identity token, AWS secret access key,
or AWS session token. Cache only STS credentials through the AWS SDK credential
cache or an equivalent short-lived credential cache.
