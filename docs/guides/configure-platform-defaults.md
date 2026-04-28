# Configure Platform Defaults

Create `AWSWorkloadIdentityOperatorConfig/default` before creating workload
bindings.

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityOperatorConfig
metadata:
  name: default
spec:
  selfHostedIRSA:
    webhookNamespace: aws-pod-identity-webhook
```

For `SelfHostedIRSA`, `spec.selfHostedIRSA.webhookNamespace` is the source of
truth for the remote webhook namespace.

`permissionsBoundaryARN` is optional. AWS supports IAM roles without a
permissions boundary, and the operator sets one only when the platform
configuration includes this value.

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityOperatorConfig
metadata:
  name: default
spec:
  permissionsBoundaryARN: arn:aws:iam::123456789012:policy/appthrust-workload-identity-boundary
  selfHostedIRSA:
    webhookNamespace: aws-pod-identity-webhook
```

The Helm chart can create this object with `operatorConfig.create=true`, but it
is disabled by default.
