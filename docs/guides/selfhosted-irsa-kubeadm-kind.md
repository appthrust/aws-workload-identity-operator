# Configure SelfHostedIRSA For kubeadm And kind

Use this guide when the target cluster is self-managed Kubernetes, such as a
kubeadm or kind cluster. `SelfHostedIRSA` can publish the OIDC issuer, create IAM
resources, install the remote webhook runtime, and annotate remote
`ServiceAccount` objects, but it does not reconfigure the target
kube-apiserver.

The target kube-apiserver must issue bound ServiceAccount tokens with the same
issuer URL and signing key that `SelfHostedIRSA` publishes to S3. Configure this
before creating the target cluster, or during a controlled control-plane
reconfiguration.

## Inputs

Choose the workload namespace and AWS region first. The namespace must match the
target cluster name label used by Cluster Inventory and OCM.

```sh
WORKLOAD_NAMESPACE=wlc-a
AWS_REGION=ap-northeast-1
```

Derive the issuer bucket name with the same naming rules used by the operator:

```sh
safe_dns_part() {
  value="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9-]+/-/g; s/^-+//; s/-+$//')"
  if [ -z "$value" ]; then
    value=x
  fi
  printf '%s' "$value"
}

name_suffix() {
  printf '%s/%s' "$1" "$2" | sha256sum | awk '{print substr($1, 1, 10)}'
}

bucket_name() {
  namespace="$1"
  region="$2"
  base="awi-$(safe_dns_part "$namespace")-$(safe_dns_part "$region")"
  tail="$(name_suffix "$namespace" "$region")"
  full="${base}-${tail}"
  if [ "${#full}" -le 63 ]; then
    printf '%s' "$full"
    return
  fi
  prefix="$(printf '%s' "$full" | cut -c 1-$((63 - ${#tail} - 1)) | sed -E 's/-+$//')"
  printf '%s-%s' "$prefix" "$tail"
}

BUCKET_NAME="$(bucket_name "$WORKLOAD_NAMESPACE" "$AWS_REGION")"
ISSUER_URL="https://${BUCKET_NAME}.s3.${AWS_REGION}.amazonaws.com"
printf '%s\n' "$ISSUER_URL"
```

Create the ServiceAccount token signing key pair. The private key is used by the
target kube-apiserver, and the public key is published in the SelfHostedIRSA JWKS
document.

```sh
openssl genrsa -out sa.key 2048
openssl rsa -in sa.key -pubout -out sa.pub
chmod 0600 sa.key
```

## kubeadm

Copy the keys to the target control-plane host before `kubeadm init`:

```sh
sudo install -m 0600 sa.key /etc/kubernetes/pki/awio-sa.key
sudo install -m 0644 sa.pub /etc/kubernetes/pki/awio-sa.pub
```

Add the issuer, signing key, public key, and STS audience to the kubeadm
`ClusterConfiguration` used for the target cluster:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: service-account-issuer
      value: https://<bucket>.s3.<region>.amazonaws.com
    - name: service-account-signing-key-file
      value: /etc/kubernetes/pki/awio-sa.key
    - name: service-account-key-file
      value: /etc/kubernetes/pki/awio-sa.pub
    - name: api-audiences
      value: https://<bucket>.s3.<region>.amazonaws.com,https://kubernetes.default.svc,sts.amazonaws.com
  extraVolumes:
    - name: awio-sa-key
      hostPath: /etc/kubernetes/pki/awio-sa.key
      mountPath: /etc/kubernetes/pki/awio-sa.key
      pathType: File
      readOnly: true
    - name: awio-sa-pub
      hostPath: /etc/kubernetes/pki/awio-sa.pub
      mountPath: /etc/kubernetes/pki/awio-sa.pub
      pathType: File
      readOnly: true
```

Replace every `https://<bucket>.s3.<region>.amazonaws.com` placeholder with the
derived `ISSUER_URL`.

## kind

Use an absolute host path for the generated keys. kind mounts those files into
the control-plane node, and kubeadm mounts them from the node filesystem into
the kube-apiserver static Pod.

kind applies these patches to its generated kubeadm configuration. Use the
kubeadm `v1beta3` map shape in `kubeadmConfigPatches`; copying the standalone
kubeadm `v1beta4` list shape from the previous section into kind leaves the
default ServiceAccount issuer settings in place.

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /absolute/path/to/sa.key
        containerPath: /etc/kubernetes/pki/awio-sa.key
        readOnly: true
      - hostPath: /absolute/path/to/sa.pub
        containerPath: /etc/kubernetes/pki/awio-sa.pub
        readOnly: true
kubeadmConfigPatches:
  - |
    apiVersion: kubeadm.k8s.io/v1beta3
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
        service-account-issuer: https://<bucket>.s3.<region>.amazonaws.com
        service-account-signing-key-file: /etc/kubernetes/pki/awio-sa.key
        service-account-key-file: /etc/kubernetes/pki/awio-sa.pub
        api-audiences: https://<bucket>.s3.<region>.amazonaws.com,https://kubernetes.default.svc,sts.amazonaws.com
      extraVolumes:
        - name: awio-sa-key
          hostPath: /etc/kubernetes/pki/awio-sa.key
          mountPath: /etc/kubernetes/pki/awio-sa.key
          pathType: File
          readOnly: true
        - name: awio-sa-pub
          hostPath: /etc/kubernetes/pki/awio-sa.pub
          mountPath: /etc/kubernetes/pki/awio-sa.pub
          pathType: File
          readOnly: true
```

Create the target kind cluster with that config:

```sh
kind create cluster --name "$WORKLOAD_NAMESPACE" --config kind-selfhosted-irsa.yaml
```

## Hub Signing Secret

Create the matching signing key Secret in the hub workload namespace before
creating `AWSWorkloadIdentityConfig/default`. The Secret name is derived from
the required config name `default`.

```sh
kubectl --context <hub-context> create namespace "$WORKLOAD_NAMESPACE" \
  --dry-run=client -o yaml | kubectl --context <hub-context> apply -f -

kubectl --context <hub-context> -n "$WORKLOAD_NAMESPACE" \
  create secret generic awi-signing-key-default \
  --from-file=sa.key=./sa.key \
  --from-file=sa.pub=./sa.pub \
  --dry-run=client -o yaml | kubectl --context <hub-context> apply -f -
```

If this Secret is absent, the operator generates a key pair for the S3 JWKS
document. That generated key will not match the target kube-apiserver key, so
AWS STS rejects the projected token.

## Continue Installation

After the target cluster is created with the issuer settings above, register it
through Cluster Inventory and OCM, install the operator, and create the
SelfHostedIRSA resources:

- [Install With Helm](install-helm.md)
- [Cluster Inventory And OCM](../concepts/cluster-inventory-and-ocm.md)
- [Quickstart](../quickstart.md)
- [Bind A ServiceAccount](bind-service-account.md)

Useful references:

- [Operator Behavior](../reference/operator-behavior.md#self-hosted-irsa-behavior)
- [Troubleshooting](../operations/troubleshooting.md)
- [kube-apiserver flags](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/)
- [kubeadm v1beta4 config](https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/)
- [kind configuration](https://kind.sigs.k8s.io/docs/user/configuration/)
