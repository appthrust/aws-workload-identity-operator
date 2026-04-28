# OCM Add-On Framework IRSA Sidecar

This guide is intentionally narrow. It describes how an OCM hosted add-on agent
uses `aws-irsa-sidecar` as the sidecar when the workload runs
outside the managed cluster but needs AWS credentials for a managed-cluster
`ServiceAccount`.

Use this page when all of these are true:

- the workload is an OCM add-on agent rendered by addon-framework, usually in
  hosted mode,
- the add-on framework supplies a kubeconfig for the managed cluster,
- AWIO delivers IRSA annotations to the remote `ServiceAccount`,
- the workload can use AWS shared config profile settings.

This page is not for `aws-remote-irsa-credential-process`. That helper still
uses the hub, Cluster Inventory, and `cp-creds` path described in
[Hub-Side Remote IRSA Consumers](remote-irsa-consumers.md).

## Why This Shape

OCM addon-framework separates the add-on manager from the add-on agent. The
manager renders manifests, and OCM applies those manifests for each
`ManagedClusterAddOn`. In hosted mode, the agent manifests can be placed on a
hosting cluster instead of the managed cluster by using OCM hosted-mode
annotations and labels.

For `aws-irsa-sidecar`, that split means the sidecar should not rediscover
the target cluster through hub APIs. The add-on framework already knows which
managed cluster this agent instance targets, so the sidecar only needs a
mounted kubeconfig for that managed cluster.

The sidecar then performs this direct remote flow:

1. Load `--kubeconfig`.
2. Call `SelfSubjectReview` and infer
   `system:serviceaccount:<namespace>:<name>`.
3. Read that remote `ServiceAccount`.
4. Read `eks.amazonaws.com/role-arn`.
5. Create a remote `serviceaccounts/token` TokenRequest for the STS audience.
6. Write the token file and AWS shared config into a shared volume with secure
   fixed defaults.
7. Refresh from the actual TokenRequest expiration timestamp.

## Add-On Framework Inputs

Render the sidecar with these per-cluster inputs:

| Input | Meaning |
| --- | --- |
| `ManagedKubeConfigSecret` | Addon-framework template value for the Secret containing a `kubeconfig` key for the target managed cluster. The framework default is `<addon name>-managed-kubeconfig`; Helm integrations expose the same value as `managedKubeConfigSecret`. |
| add-on install namespace | The namespace where the hosted agent Pod runs on the hosting cluster. |
| remote ServiceAccount | The managed-cluster ServiceAccount that AWIO annotates and that `aws-irsa-sidecar` will read. |
| AWS region | Passed to the workload container as `AWS_REGION` or `AWS_DEFAULT_REGION`. |

Mount `ManagedKubeConfigSecret` with the addon-framework hosted example's
volume name and path:

```text
volume: managed-kubeconfig-secret
mountPath: /managed/config
file: /managed/config/kubeconfig
```

The sidecar reads:

```text
/managed/config/kubeconfig
```

Do not pass the hub kubeconfig to `--kubeconfig`. Do not mount
`cp-creds`, a ClusterProfile access-provider file, or a hub-side
`AWSServiceAccountRole` lookup path into this sidecar.

## AWIO Binding

The sidecar always uses SelfSubjectReview inference, so bind AWS identity to
the remote identity carried by the kubeconfig. For an addon-framework
ManagedServiceAccount-style identity, that is usually the remote add-on
ServiceAccount:

```yaml
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: hosted-addon
  namespace: <workload-namespace>
spec:
  serviceAccount:
    namespace: <remote-addon-namespace>
    name: <remote-addon-serviceaccount>
  policyDocument:
    Version: "2012-10-17"
    Statement:
      - Effect: Allow
        Action:
          - sts:GetCallerIdentity
        Resource: "*"
```

AWIO owns delivery of the role annotation to that remote ServiceAccount:

- `eks.amazonaws.com/role-arn`

The sidecar intentionally ignores audience, token-expiration, STS endpoint, AWS
profile, token mode, refresh window, and role-session-name customization. The
workload Pod does not need AWS annotations.

## Remote RBAC

Grant RBAC on the managed cluster to the identity in the mounted kubeconfig.
For SelfSubjectReview inference and same-ServiceAccount token minting:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: aws-irsa-sidecar
  namespace: <remote-addon-namespace>
rules:
  - apiGroups: [""]
    resources: ["serviceaccounts"]
    resourceNames: ["<remote-addon-serviceaccount>"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["serviceaccounts/token"]
    resourceNames: ["<remote-addon-serviceaccount>"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: aws-irsa-sidecar
  namespace: <remote-addon-namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aws-irsa-sidecar
subjects:
  - kind: ServiceAccount
    name: <remote-addon-serviceaccount>
    namespace: <remote-addon-namespace>
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: aws-irsa-sidecar-selfsubjectreview
rules:
  - apiGroups: ["authentication.k8s.io"]
    resources: ["selfsubjectreviews"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aws-irsa-sidecar-selfsubjectreview
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: aws-irsa-sidecar-selfsubjectreview
subjects:
  - kind: ServiceAccount
    name: <remote-addon-serviceaccount>
    namespace: <remote-addon-namespace>
```

## Hosted Agent Manifest

For Kubernetes native sidecars, place `aws-irsa-sidecar` in `initContainers`
with `restartPolicy: Always`. If the hosting cluster does not support native
sidecars, run the same container as a normal long-running sidecar.

The `aws-irsa-sidecar:0.1.0` tag below points at the planned first-release
coordinates for AWIO. The first public tag has not been cut yet, so the OCI
artifact at those coordinates is not yet published; the example becomes
pullable once `v0.1.0` lands.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hosted-addon-agent
  namespace: <addon-install-namespace>
  labels:
    addon.open-cluster-management.io/hosted-manifest-location: hosting
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: hosted-addon-agent
  template:
    metadata:
      labels:
        app.kubernetes.io/name: hosted-addon-agent
    spec:
      serviceAccountName: <hosted-agent-serviceaccount>
      securityContext:
        fsGroup: 65532
        runAsGroup: 65532
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile:
          type: RuntimeDefault
      volumes:
        - name: aws-irsa-state
          emptyDir: {}
        - name: managed-kubeconfig-secret
          secret:
            secretName: {{ .ManagedKubeConfigSecret }}
      initContainers:
        - name: aws-irsa-sidecar
          image: ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:0.1.0
          restartPolicy: Always
          command:
            - /aws-irsa-sidecar
          args:
            - --kubeconfig=/managed/config/kubeconfig
            - --token-file=/var/run/aws-irsa/token
            - --aws-config-file=/var/run/aws-irsa/config
          startupProbe:
            exec:
              command:
                - /aws-irsa-sidecar
                - check
                - --token-file=/var/run/aws-irsa/token
                - --aws-config-file=/var/run/aws-irsa/config
            periodSeconds: 2
            failureThreshold: 90
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          volumeMounts:
            - name: aws-irsa-state
              mountPath: /var/run/aws-irsa
            - name: managed-kubeconfig-secret
              mountPath: /managed/config
              readOnly: true
      containers:
        - name: agent
          image: <hosted-addon-agent-image>
          env:
            - name: AWS_CONFIG_FILE
              value: /var/run/aws-irsa/config
            - name: AWS_SDK_LOAD_CONFIG
              value: "1"
            - name: AWS_REGION
              value: <aws-region>
            - name: AWS_DEFAULT_REGION
              value: <aws-region>
          volumeMounts:
            - name: aws-irsa-state
              mountPath: /var/run/aws-irsa
              readOnly: true
```

Do not set `AWS_ROLE_ARN` or `AWS_WEB_IDENTITY_TOKEN_FILE` on the workload
container for this pattern. The generated AWS shared config supplies the
`role_arn` and `web_identity_token_file` values.

## Runtime Contract

The sidecar writes the token file and AWS config atomically. A generated config
looks like this:

```ini
[default]
role_arn = arn:aws:iam::123456789012:role/example
web_identity_token_file = /var/run/aws-irsa/token
role_session_name = awio-addon-system-hosted-addon-agent-7f9d8c1a2b3c
sts_regional_endpoints = regional
```

The sidecar always writes the default profile, always writes
`sts_regional_endpoints = regional`, and always writes a deterministic
`role_session_name` generated from the remote API server and inferred remote
ServiceAccount. It requests a 10 minute remote TokenRequest and schedules
refresh from the server-returned expiration timestamp minus 9 minutes. Token
and AWS config files are written with mode `0600`, so run the sidecar and
workload as the same UID.

## Troubleshooting

Start with the sidecar startup probe:

```sh
aws-irsa-sidecar check \
  --token-file=/var/run/aws-irsa/token \
  --aws-config-file=/var/run/aws-irsa/config
```

Then check these failure points in order:

- `--kubeconfig` points at the managed cluster, not the hub.
- The kubeconfig user is a ServiceAccount.
- The remote identity can `create selfsubjectreviews`.
- The remote identity can `get serviceaccounts` and `create
  serviceaccounts/token` for the target ServiceAccount.
- The target ServiceAccount has `eks.amazonaws.com/role-arn`.
- The workload container has `AWS_CONFIG_FILE` and an AWS region.
- The hosting cluster can reach the managed cluster API server named in the
  mounted kubeconfig.

If the token rotates too quickly, compare sidecar logs with the
`TokenRequest.status.expirationTimestamp` returned by the managed cluster. The
sidecar schedules the next refresh from that server-returned timestamp minus the
fixed refresh window.

## Guardrails

- Do not read OCM `ManagedCluster` or Cluster API `Cluster` objects from this
  sidecar.
- Do not read hub-side remote kubeconfig Secrets in reconcilers.
- Do not use `ClusterProfile`, `cp-creds`, or `--clusterprofile-provider-file`
  for this sidecar.
- Do not inject AWS annotations into workload Pods.
- Keep the workload on ordinary AWS SDK shared config settings.

## Related OCM References

- [OCM Add-on Developer Guide](https://open-cluster-management.io/docs/developer-guides/addon/)
- [OCM managed-serviceaccount add-on](https://open-cluster-management.io/docs/getting-started/integration/managed-serviceaccount/)
