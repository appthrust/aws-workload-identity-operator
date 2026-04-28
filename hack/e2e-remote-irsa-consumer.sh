#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="$(date -u +%Y%m%d%H%M%S)-$$"
AWS_REGION="${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
TIMEOUT_SECONDS="1200"
HELM_TIMEOUT="15m"

HUB_KIND_CLUSTER_NAME="awio-hub-${RUN_ID}"
DOCKER_SOCK=""
KUBECONFIG_MERGE_TARGET=""
HUB_KUBECONFIG=""
MANAGED_KUBECONFIG=""
HUB_CONTEXT=""
MANAGED_CONTEXT=""
HUB_READY="0"
MANAGED_READY="0"

ACK_NAMESPACE="ack-system"
AWIO_NAMESPACE="awio-system"
AWIO_RELEASE="awio"
AWIO_FULLNAME="${AWIO_RELEASE}-aws-workload-identity-operator"
AWIO_IMAGE="ghcr.io/appthrust/aws-workload-identity-operator:e2e-${RUN_ID}"
REMOTE_HELPER_IMAGE="ghcr.io/appthrust/aws-remote-irsa-tools:e2e-${RUN_ID}"
AWIO_IMAGE_REGISTRY=""
AWIO_IMAGE_REPOSITORY=""
AWIO_IMAGE_TAG=""

CERT_MANAGER_VERSION="v1.20.2"
CLUSTER_API_PROVIDER_VERSION="v1.13.1"
CAPD_PROVIDER="docker:${CLUSTER_API_PROVIDER_VERSION}"
CAPD_FLAVOR="development"
CAPD_KUBERNETES_VERSION=""
CNI_MANIFEST_URL="https://raw.githubusercontent.com/projectcalico/calico/v3.32.0/manifests/calico.yaml"
OCM_CLUSTER_PROXY_CHART_VERSION="0.10.0"
OCM_MANAGED_SERVICEACCOUNT_CHART_VERSION="0.10.0"

WLC_NAMESPACE="wlc-${RUN_ID}"
APP_NAMESPACE="irsa-${RUN_ID}"
APP_SERVICE_ACCOUNT="sts-canary"
ROLE_RESOURCE_NAME="sts-canary"
TOKEN_FILE_ROLE_RESOURCE_NAME="sts-canary-token-file"
OCM_ACCESS_PROVIDER="open-cluster-management"
OCM_CLUSTER_NAME_LABEL="open-cluster-management.io/cluster-name"
OCM_CLUSTER_MANAGER_LABEL="x-k8s.io/cluster-manager"
MANAGED_SERVICE_ACCOUNT="aws-workload-identity-operator"
OCM_REMOTE_PERMISSIONS_NAME="awio-e2e-operator-remote-permissions"
AWS_CLI_IMAGE="public.ecr.aws/aws-cli/aws-cli:2.31.11"
POD_IDENTITY_WEBHOOK_IMAGE="public.ecr.aws/eks/amazon-eks-pod-identity-webhook:v0.6.15"
CP_CREDS_IMAGE="quay.io/open-cluster-management/cp-creds:latest"
AWS_IRSA_SIDECAR_PERMISSIONS_NAME="awio-e2e-aws-irsa-sidecar"
AWS_IRSA_SIDECAR_JOB_NAME="aws-irsa-sidecar"
IRSA_SIDECAR_MANAGED_KUBECONFIG_SECRET_NAME="aws-irsa-sidecar-managed-kubeconfig"
REMOTE_IRSA_CLUSTER_PROPERTIES_NAME="awio-e2e-cluster-properties"
AWS_REGION_CLUSTER_PROPERTY="aws.identity.appthrust.io.aws-region"
DENIED_SERVICE_ACCOUNT="sts-denied"
MISMATCH_SERVICE_ACCOUNT="sts-mismatch"
REMOTE_ACCESS_SUBJECT=""
REMOTE_ACCESS_NAMESPACE=""

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/awio-e2e.XXXXXX")"
HELPER_BINARY="${WORK_DIR}/bin/aws-remote-irsa-credential-process"
IRSA_SIDECAR_BINARY="${WORK_DIR}/bin/aws-irsa-sidecar"
PROVIDER_FILE="${WORK_DIR}/clusterprofile-provider-file.json"
PATH="${WORK_DIR}/bin:${PATH}"
export PATH

ROLE_ARN=""
TOKEN_FILE_ROLE_ARN=""
GENERATED_POLICY_ARN=""
TOKEN_FILE_GENERATED_POLICY_ARN=""
OIDC_PROVIDER_ARN=""
BUCKET_NAME=""
CLUSTERPROFILE_NAMESPACE=""
CLUSTERPROFILE_NAME=""

log() {
  printf '[e2e] %s\n' "$*" >&2
}

die() {
  log "ERROR: $*"
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

require_python_yq() {
  local err_file

  need yq
  err_file="${WORK_DIR}/yq-preflight.err"
  if ! printf 'apiVersion: v1\nkind: List\n' | yq -y --explicit-start --arg expected List '.kind == $expected' >/dev/null 2>"$err_file"; then
    die "Python yq (kislyuk/yq) is required. This script uses yq -y --explicit-start with jq filter syntax; install kislyuk/yq or put it before other yq binaries on PATH: $(file_summary "$err_file")"
  fi
}

write_cluster_proxy_post_renderer() {
  local post_renderer="${WORK_DIR}/cluster-proxy-post-renderer"

  cat >"$post_renderer" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

yq -y --explicit-start '
  if .apiVersion == "proxy.open-cluster-management.io/v1alpha1"
    and .kind == "ManagedProxyConfiguration"
  then
    del(.spec.proxyAgent.additionalValues)
  else
    .
  end
'
EOF
  chmod +x "$post_renderer"
  printf '%s' "$post_renderer"
}

write_klusterlet_values_file() {
  local values_file="${WORK_DIR}/klusterlet-values.yaml"

  cat >"$values_file" <<'EOF'
podSecurityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
EOF
  printf '%s' "$values_file"
}

patch_cert_manager_rotation_policy() {
  local raw_manifest="$1"
  local patched_manifest="$2"

  yq -y --explicit-start '
    if .apiVersion == "cert-manager.io/v1"
      and .kind == "Certificate"
    then
      .spec.privateKey.rotationPolicy = "Always"
    else
      .
    end
  ' "$raw_manifest" >"$patched_manifest"
}

install_cluster_api_provider() {
  local flag="$1"
  local provider="$2"
  local name="$3"
  local raw_manifest="${WORK_DIR}/${name}.raw.yaml"
  local patched_manifest="${WORK_DIR}/${name}.yaml"

  CLUSTER_TOPOLOGY=true clusterctl generate provider "$flag" "$provider" >"$raw_manifest"
  patch_cert_manager_rotation_policy "$raw_manifest" "$patched_manifest"
  hub_kubectl apply --server-side --force-conflicts --field-manager=awio-e2e-cluster-api -f "$patched_manifest"
}

file_summary() {
  local file="$1"
  tr '\n' ' ' <"$file" | sed -E 's/[[:space:]]+/ /g; s/^ //; s/ $//'
}

aws_profile_hint() {
  if [[ -n "${AWS_PROFILE:-}" ]]; then
    printf ' for AWS_PROFILE=%s' "$AWS_PROFILE"
  fi
}

die_aws_error() {
  local message="$1"
  local err_file="$2"
  local aws_message
  local profile_hint

  aws_message="$(file_summary "$err_file")"
  profile_hint="$(aws_profile_hint)"
  if [[ -n "$aws_message" ]]; then
    die "${message}${profile_hint}: ${aws_message}"
  fi
  die "${message}${profile_hint}"
}

default_kubeconfig_merge_target() {
  local kubeconfig_list="${KUBECONFIG:-}"
  local path
  local paths

  if [[ -n "$kubeconfig_list" ]]; then
    IFS=: read -r -a paths <<<"$kubeconfig_list"
    for path in "${paths[@]}"; do
      if [[ -n "$path" ]]; then
        printf '%s' "$path"
        return 0
      fi
    done
  fi

  printf '%s/.kube/config' "$HOME"
}

resolve_docker_sock() {
  local context_host

  case "${DOCKER_HOST:-}" in
    unix://*)
      printf '%s' "${DOCKER_HOST#unix://}"
      return 0
      ;;
    "")
      ;;
    *)
      die "hub kind cluster requires a local Docker unix socket; DOCKER_HOST=${DOCKER_HOST} is not a unix socket"
      ;;
  esac

  context_host="$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null | head -n 1 || true)"
  case "$context_host" in
    unix://*)
      printf '%s' "${context_host#unix://}"
      return 0
      ;;
  esac

  if [[ -S /var/run/docker.sock ]]; then
    printf '%s' /var/run/docker.sock
    return 0
  fi

  die "hub kind cluster requires a local Docker unix socket; set DOCKER_HOST=unix:///path/to/docker.sock or switch to a unix-socket Docker context"
}

prepare_kubeconfig_merge_target() {
  local target_dir

  if [[ -z "$KUBECONFIG_MERGE_TARGET" ]]; then
    KUBECONFIG_MERGE_TARGET="$(default_kubeconfig_merge_target)"
  fi

  target_dir="$(dirname "$KUBECONFIG_MERGE_TARGET")"
  mkdir -p "$target_dir"
  if [[ -e "$KUBECONFIG_MERGE_TARGET" && ! -f "$KUBECONFIG_MERGE_TARGET" ]]; then
    die "kubeconfig merge target ${KUBECONFIG_MERGE_TARGET} is not a file"
  fi
  if [[ -e "$KUBECONFIG_MERGE_TARGET" && ! -w "$KUBECONFIG_MERGE_TARGET" ]]; then
    die "kubeconfig merge target ${KUBECONFIG_MERGE_TARGET} is not writable"
  fi
  if [[ ! -e "$KUBECONFIG_MERGE_TARGET" ]]; then
    : >"$KUBECONFIG_MERGE_TARGET"
    chmod 600 "$KUBECONFIG_MERGE_TARGET" 2>/dev/null || true
  fi

  log "kubeconfig merge target: ${KUBECONFIG_MERGE_TARGET}"
}

merge_kubeconfig_for_k9s() {
  local source="$1"
  local label="$2"
  local previous_context
  local merged_file

  [[ -f "$source" ]] || die "${label} kubeconfig ${source} does not exist"

  previous_context="$(kubectl --kubeconfig "$KUBECONFIG_MERGE_TARGET" config current-context 2>/dev/null || true)"
  merged_file="${WORK_DIR}/${label}-merged-kubeconfig"
  KUBECONFIG="${source}:${KUBECONFIG_MERGE_TARGET}" kubectl config view --raw --flatten >"$merged_file"
  cp "$merged_file" "$KUBECONFIG_MERGE_TARGET"
  chmod 600 "$KUBECONFIG_MERGE_TARGET" 2>/dev/null || true

  if [[ -n "$previous_context" ]]; then
    kubectl --kubeconfig "$KUBECONFIG_MERGE_TARGET" config use-context "$previous_context" >/dev/null 2>&1 || true
  fi

  log "${label} context merged into ${KUBECONFIG_MERGE_TARGET}"
}

hub_kubeconfig_ready() {
  [[ "$HUB_READY" == "1" && -n "${HUB_KUBECONFIG:-}" && -f "$HUB_KUBECONFIG" ]]
}

managed_kubeconfig_ready() {
  [[ "$MANAGED_READY" == "1" && -n "${MANAGED_KUBECONFIG:-}" && -f "$MANAGED_KUBECONFIG" ]]
}

hub_kubectl() {
  kubectl --kubeconfig "$HUB_KUBECONFIG" "$@"
}

managed_kubectl() {
  kubectl --kubeconfig "$MANAGED_KUBECONFIG" "$@"
}

hub_helm() {
  HELM_CACHE_HOME="${WORK_DIR}/helm/cache" \
    HELM_CONFIG_HOME="${WORK_DIR}/helm/config" \
    HELM_DATA_HOME="${WORK_DIR}/helm/data" \
    helm --kubeconfig "$HUB_KUBECONFIG" "$@"
}

clusteradm_cmd() {
  local kubeconfigs="$HUB_KUBECONFIG"
  if [[ -n "${MANAGED_KUBECONFIG:-}" ]]; then
    kubeconfigs="${kubeconfigs}:${MANAGED_KUBECONFIG}"
  fi
  KUBECONFIG="$kubeconfigs" clusteradm "$@"
}

safe_dns_part() {
  local value
  value="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9-]+/-/g; s/^-+//; s/-+$//')"
  if [[ -z "$value" ]]; then
    value="x"
  fi
  printf '%s' "$value"
}

name_suffix() {
  printf '%s/%s' "$1" "$2" | sha256sum | awk '{print substr($1, 1, 10)}'
}

bucket_name() {
  local namespace="$1"
  local region="$2"
  local base tail full prefix

  base="awi-$(safe_dns_part "$namespace")-$(safe_dns_part "$region")"
  tail="$(name_suffix "$namespace" "$region")"
  full="${base}-${tail}"
  if ((${#full} <= 63)); then
    printf '%s' "$full"
    return
  fi

  prefix="${full:0:$((63 - ${#tail} - 1))}"
  prefix="$(printf '%s' "$prefix" | sed -E 's/-+$//')"
  printf '%s-%s' "$prefix" "$tail"
}

wait_for_json_condition_hub() {
  local namespace="$1"
  local resource="$2"
  local name="$3"
  local condition_type="$4"
  local timeout="$5"
  local json start status reason message

  start="$(date +%s)"
  while true; do
    if json="$(hub_kubectl -n "$namespace" get "$resource" "$name" -o json 2>/dev/null)"; then
      status="$(jq -r --arg type "$condition_type" '.status.conditions[]? | select(.type == $type) | .status' <<<"$json")"
      reason="$(jq -r --arg type "$condition_type" '.status.conditions[]? | select(.type == $type) | .reason // ""' <<<"$json")"
      message="$(jq -r --arg type "$condition_type" '.status.conditions[]? | select(.type == $type) | .message // ""' <<<"$json")"
      if [[ "$status" == "True" ]]; then
        log "${resource}/${name} condition ${condition_type}=True"
        return 0
      fi
    fi

    if (( $(date +%s) - start > timeout )); then
      log "${resource}/${name} condition ${condition_type} did not become True"
      log "last status=${status:-<missing>} reason=${reason:-<missing>} message=${message:-<missing>}"
      hub_kubectl -n "$namespace" get "$resource" "$name" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_nonempty_hub_jsonpath() {
  local description="$1"
  local timeout="$2"
  shift 2
  local start value

  start="$(date +%s)"
  while true; do
    value="$("$@" 2>/dev/null || true)"
    if [[ -n "$value" ]]; then
      log "$description is populated"
      return 0
    fi

    if (( $(date +%s) - start > timeout )); then
      log "$description was not populated"
      return 1
    fi

    sleep 3
  done
}

select_clusterprofile_json() {
  jq -cer --arg provider "$OCM_ACCESS_PROVIDER" '
    .items
    | sort_by(.metadata.namespace, .metadata.name)
    | map(select(any(.status.accessProviders[]?; .name == $provider)))
    | first // empty
  '
}

resolve_clusterprofile_json() {
  local json selected selector

  for selector in \
    "${OCM_CLUSTER_NAME_LABEL}=${WLC_NAMESPACE},${OCM_CLUSTER_MANAGER_LABEL}=${OCM_ACCESS_PROVIDER}" \
    "${OCM_CLUSTER_NAME_LABEL}=${WLC_NAMESPACE}"; do
    json="$(hub_kubectl get clusterprofile -A -l "$selector" -o json 2>/dev/null || true)"
    [[ -z "$json" ]] && continue
    selected="$(select_clusterprofile_json <<<"$json" 2>/dev/null || true)"
    if [[ -n "$selected" ]]; then
      printf '%s' "$selected"
      return 0
    fi
  done

  return 1
}

wait_for_clusterprofile() {
  local start json ready provider_count

  start="$(date +%s)"
  while true; do
    if json="$(resolve_clusterprofile_json)"; then
      CLUSTERPROFILE_NAMESPACE="$(jq -r '.metadata.namespace' <<<"$json")"
      CLUSTERPROFILE_NAME="$(jq -r '.metadata.name' <<<"$json")"
      ready="$(jq -r '.status.conditions[]? | select(.type == "ControlPlaneHealthy") | .status' <<<"$json")"
      provider_count="$(jq -r --arg provider "$OCM_ACCESS_PROVIDER" '[.status.accessProviders[]? | select(.name == $provider)] | length' <<<"$json")"
      if [[ "$ready" == "True" && "$provider_count" != "0" ]]; then
        log "ClusterProfile ${CLUSTERPROFILE_NAMESPACE}/${CLUSTERPROFILE_NAME} is ready with ${OCM_ACCESS_PROVIDER} access"
        return 0
      fi
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "ClusterProfile with ${OCM_CLUSTER_NAME_LABEL}=${WLC_NAMESPACE} did not become usable"
      hub_kubectl get clusterprofile -A -l "${OCM_CLUSTER_NAME_LABEL}=${WLC_NAMESPACE}" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_clusterprofile_credential() {
  local secret_name start token

  secret_name="${CLUSTERPROFILE_NAME}-${MANAGED_SERVICE_ACCOUNT}"
  start="$(date +%s)"
  while true; do
    token="$(hub_kubectl -n "$CLUSTERPROFILE_NAMESPACE" get secret "$secret_name" -o jsonpath='{.data.token}' 2>/dev/null || true)"
    if [[ -n "$token" ]]; then
      log "ClusterProfile credential secret ${CLUSTERPROFILE_NAMESPACE}/${secret_name} is ready"
      return 0
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "ClusterProfile credential secret ${CLUSTERPROFILE_NAMESPACE}/${secret_name} was not created"
      hub_kubectl -n "$CLUSTERPROFILE_NAMESPACE" get secret "$secret_name" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_clusterprofile_property() {
  local property_name="$1"
  local property_value="$2"
  local json start

  start="$(date +%s)"
  while true; do
    if json="$(resolve_clusterprofile_json)"; then
      if jq -e --arg name "$property_name" --arg value "$property_value" '
        any(.status.properties[]?; .name == $name and .value == $value)
      ' <<<"$json" >/dev/null; then
        log "ClusterProfile ${CLUSTERPROFILE_NAMESPACE}/${CLUSTERPROFILE_NAME} property ${property_name}=${property_value} is ready"
        return 0
      fi
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "ClusterProfile property ${property_name}=${property_value} was not synced"
      hub_kubectl get managedcluster "$WLC_NAMESPACE" -o yaml || true
      hub_kubectl get clusterprofile -A -l "${OCM_CLUSTER_NAME_LABEL}=${WLC_NAMESPACE}" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_role_arn() {
  local start
  start="$(date +%s)"

  while true; do
    ROLE_ARN="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o jsonpath='{.status.roleARN}' 2>/dev/null || true)"
    if [[ -n "$ROLE_ARN" ]]; then
      log "AWSServiceAccountRole role ARN resolved"
      return 0
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_token_file_role_arn() {
  local start
  start="$(date +%s)"

  while true; do
    TOKEN_FILE_ROLE_ARN="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o jsonpath='{.status.roleARN}' 2>/dev/null || true)"
    if [[ -n "$TOKEN_FILE_ROLE_ARN" ]]; then
      log "token-file AWSServiceAccountRole role ARN resolved"
      return 0
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o yaml || true
      return 1
    fi

    sleep 5
  done
}

wait_for_service_account_role_annotation() {
  local timeout="$1"
  local start role

  start="$(date +%s)"
  while true; do
    role="$(managed_kubectl -n "$APP_NAMESPACE" get serviceaccount "$APP_SERVICE_ACCOUNT" -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}' 2>/dev/null || true)"
    if [[ "$role" == "$ROLE_ARN" ]]; then
      log "ServiceAccount ${APP_NAMESPACE}/${APP_SERVICE_ACCOUNT} role annotation is synced"
      return 0
    fi

    if (( $(date +%s) - start > timeout )); then
      log "ServiceAccount ${APP_NAMESPACE}/${APP_SERVICE_ACCOUNT} role annotation did not sync within ${timeout}s"
      managed_kubectl -n "$APP_NAMESPACE" get serviceaccount "$APP_SERVICE_ACCOUNT" -o yaml || true
      return 1
    fi

    sleep 3
  done
}

wait_for_managed_service_account_role_annotation() {
  local timeout="$1"
  local start role

  [[ -n "$REMOTE_ACCESS_NAMESPACE" ]] || die "remote access namespace was not resolved"
  start="$(date +%s)"
  while true; do
    role="$(managed_kubectl -n "$REMOTE_ACCESS_NAMESPACE" get serviceaccount "$MANAGED_SERVICE_ACCOUNT" -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}' 2>/dev/null || true)"
    if [[ "$role" == "$TOKEN_FILE_ROLE_ARN" ]]; then
      log "ManagedServiceAccount ${REMOTE_ACCESS_NAMESPACE}/${MANAGED_SERVICE_ACCOUNT} role annotation is synced"
      return 0
    fi

    if (( $(date +%s) - start > timeout )); then
      log "ManagedServiceAccount ${REMOTE_ACCESS_NAMESPACE}/${MANAGED_SERVICE_ACCOUNT} role annotation did not sync within ${timeout}s"
      managed_kubectl -n "$REMOTE_ACCESS_NAMESPACE" get serviceaccount "$MANAGED_SERVICE_ACCOUNT" -o yaml || true
      return 1
    fi

    sleep 3
  done
}

diagnostics() {
  if hub_kubeconfig_ready || managed_kubeconfig_ready; then
    log "collecting diagnostics"
  fi

  if hub_kubeconfig_ready; then
    hub_kubectl get pods -A -o wide || true
    hub_kubectl -n "$AWIO_NAMESPACE" logs "deploy/${AWIO_FULLNAME}" --tail=200 || true
    hub_kubectl -n "$ACK_NAMESPACE" logs deploy/ack-iam-controller --tail=120 || true
    hub_kubectl -n "$ACK_NAMESPACE" logs deploy/ack-s3-controller --tail=120 || true
    hub_kubectl -n "$AWIO_NAMESPACE" get job,pod -l "job-name=${AWS_IRSA_SIDECAR_JOB_NAME}" -o wide || true
    hub_kubectl -n "$AWIO_NAMESPACE" describe job "$AWS_IRSA_SIDECAR_JOB_NAME" || true
    hub_kubectl -n "$AWIO_NAMESPACE" describe pod -l "job-name=${AWS_IRSA_SIDECAR_JOB_NAME}" || true
    hub_kubectl -n "$AWIO_NAMESPACE" logs -l "job-name=${AWS_IRSA_SIDECAR_JOB_NAME}" --all-containers --tail=300 || true
    hub_kubectl -n "$WLC_NAMESPACE" get clusterprofile,awsworkloadidentityconfig,awsserviceaccountrole,managedserviceaccount,managedclusteraddon,manifestwork -o wide || true
    hub_kubectl -n "$WLC_NAMESPACE" get roles.iam.services.k8s.aws,policies.iam.services.k8s.aws,openidconnectproviders.iam.services.k8s.aws,buckets.s3.services.k8s.aws -o wide || true
    hub_kubectl -n "$WLC_NAMESPACE" get cluster "$WLC_NAMESPACE" -o yaml || true
  fi
  if managed_kubeconfig_ready; then
    managed_kubectl get pods -A -o wide || true
    managed_kubectl get events -A --sort-by=.lastTimestamp || true
    managed_kubectl -n "$APP_NAMESPACE" get serviceaccount,pod,job -o yaml || true
    managed_kubectl -n "$APP_NAMESPACE" describe serviceaccount "$APP_SERVICE_ACCOUNT" || true
    managed_kubectl -n "$APP_NAMESPACE" describe job awio-sts-canary || true
    managed_kubectl -n "$APP_NAMESPACE" describe pod -l job-name=awio-sts-canary || true
    managed_kubectl -n "$APP_NAMESPACE" logs -l job-name=awio-sts-canary --all-containers --tail=200 || true
    managed_kubectl -n "$APP_NAMESPACE" get events --sort-by=.lastTimestamp || true
    managed_kubectl -n aws-pod-identity-webhook get all || true
    managed_kubectl -n aws-pod-identity-webhook get service,endpoints,endpointslice,pod,deployment,replicaset,secret -o wide || true
    managed_kubectl -n aws-pod-identity-webhook describe deploy/pod-identity-webhook || true
    managed_kubectl -n aws-pod-identity-webhook logs deploy/pod-identity-webhook --all-containers --tail=400 || true
    managed_kubectl -n aws-pod-identity-webhook get events --sort-by=.lastTimestamp || true
    managed_kubectl get mutatingwebhookconfiguration pod-identity-webhook -o yaml || true
  fi
}

verify_pod_identity_webhook_mutation() {
  local dry_run_err dry_run_file dry_run_json

  dry_run_err="${WORK_DIR}/pod-identity-webhook-dry-run.err"
  dry_run_file="${WORK_DIR}/pod-identity-webhook-dry-run.json"

  log "verifying pod identity webhook mutation with server-side dry-run"
  if ! dry_run_json="$(managed_kubectl create --dry-run=server -f - -o json 2>"$dry_run_err" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: awio-sts-canary-dry-run
  namespace: ${APP_NAMESPACE}
spec:
  securityContext:
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  restartPolicy: Never
  serviceAccountName: ${APP_SERVICE_ACCOUNT}
  containers:
    - name: aws
      image: ${AWS_CLI_IMAGE}
      env:
        - name: HOME
          value: /tmp
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop:
            - ALL
        runAsGroup: 65532
        runAsNonRoot: true
        runAsUser: 65532
      command:
        - aws
        - sts
        - get-caller-identity
        - --output
        - json
EOF
)"; then
    log "server-side dry-run Pod create did not pass admission"
    sed 's/^/[e2e] dry-run: /' "$dry_run_err" >&2 || true
    return 1
  fi

  printf '%s\n' "$dry_run_json" >"$dry_run_file"
  if ! jq -e --arg role "$ROLE_ARN" --arg service_account "$APP_SERVICE_ACCOUNT" '
    (.spec.containers[]? | select(.name == "aws")) as $container
    | def env_value($name): [($container.env // [])[]? | select(.name == $name) | .value][0] // "";
      def projected_token_volume($mount_names):
        any(.spec.volumes[]?;
          (.name as $volume_name | any($mount_names[]?; . == $volume_name))
          and any(.projected.sources[]?.serviceAccountToken?;
            .audience == "sts.amazonaws.com"
            and .path == "token"
            and (.expirationSeconds // 0) == 86400
          )
        );
      .spec.serviceAccountName == $service_account
      and env_value("AWS_ROLE_ARN") == $role
      and env_value("AWS_STS_REGIONAL_ENDPOINTS") == "regional"
      and env_value("AWS_WEB_IDENTITY_TOKEN_FILE") != ""
      and (
        env_value("AWS_WEB_IDENTITY_TOKEN_FILE") as $token_file
        | [$container.volumeMounts[]? | . as $mount | select($token_file | startswith(($mount.mountPath | sub("/+$"; "")) + "/")) | .name] as $mount_names
        | ($mount_names | length) > 0
        and projected_token_volume($mount_names)
      )
  ' <<<"$dry_run_json" >/dev/null; then
    log "server-side dry-run Pod was not mutated for IRSA as expected"
    jq '{containers: [.spec.containers[]? | {name, env, volumeMounts}], volumes: .spec.volumes}' "$dry_run_file" >&2 || true
    return 1
  fi

  log "server-side dry-run Pod mutation verified"
}

wait_for_hub_delete() {
  local namespace="$1"
  local resource="$2"
  local name="$3"
  local timeout="$4"
  local start

  start="$(date +%s)"
  while hub_kubectl -n "$namespace" get "$resource" "$name" >/dev/null 2>&1; do
    if (( $(date +%s) - start > timeout )); then
      log "${resource}/${name} did not delete before timeout"
      return 1
    fi

    sleep 3
  done

  return 0
}

delete_hub_object_if_crd_exists() {
  local crd="$1"
  local namespace="$2"
  local resource="$3"
  local name="$4"
  local timeout="$5"

  if ! hub_kubectl get crd "$crd" >/dev/null 2>&1; then
    return 0
  fi

  hub_kubectl -n "$namespace" delete "$resource" "$name" --ignore-not-found=true --timeout=90s >/dev/null 2>&1 &&
    wait_for_hub_delete "$namespace" "$resource" "$name" "$timeout"
}

capture_aws_cleanup_identifiers() {
  local value

  if ! hub_kubeconfig_ready; then
    return 0
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o jsonpath='{.status.roleARN}' 2>/dev/null || true)"
  if [[ -z "$ROLE_ARN" && -n "$value" ]]; then
    ROLE_ARN="$value"
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}' 2>/dev/null || true)"
  if [[ -z "$GENERATED_POLICY_ARN" && -n "$value" ]]; then
    GENERATED_POLICY_ARN="$value"
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o jsonpath='{.status.roleARN}' 2>/dev/null || true)"
  if [[ -z "$TOKEN_FILE_ROLE_ARN" && -n "$value" ]]; then
    TOKEN_FILE_ROLE_ARN="$value"
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}' 2>/dev/null || true)"
  if [[ -z "$TOKEN_FILE_GENERATED_POLICY_ARN" && -n "$value" ]]; then
    TOKEN_FILE_GENERATED_POLICY_ARN="$value"
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.oidcProviderARN}' 2>/dev/null || true)"
  if [[ -z "$OIDC_PROVIDER_ARN" && -n "$value" ]]; then
    OIDC_PROVIDER_ARN="$value"
  fi

  value="$(hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.selfHostedIssuer.bucketName}' 2>/dev/null || true)"
  if [[ -z "$BUCKET_NAME" && -n "$value" ]]; then
    BUCKET_NAME="$value"
  fi
  if [[ -z "$BUCKET_NAME" && -n "$AWS_REGION" ]]; then
    BUCKET_NAME="$(bucket_name "$WLC_NAMESPACE" "$AWS_REGION")"
  fi
}

iam_role_name_from_arn() {
  local arn="$1"
  local resource

  if [[ "$arn" != *":role/"* ]]; then
    return 1
  fi

  resource="${arn##*:role/}"
  [[ -n "$resource" ]] || return 1
  printf '%s' "${resource##*/}"
}

aws_iam_role_absent() {
  local role_name="$1"
  local err_file="${WORK_DIR}/aws-iam-role-absent.err"

  AWS_ABSENCE_LAST_ERROR=""
  if aws iam get-role --role-name "$role_name" >/dev/null 2>"$err_file"; then
    AWS_ABSENCE_LAST_ERROR="IAM Role still exists"
    return 1
  fi
  if grep -Eiq 'NoSuchEntity|not found|cannot be found' "$err_file"; then
    return 0
  fi

  AWS_ABSENCE_LAST_ERROR="$(file_summary "$err_file")"
  return 1
}

aws_iam_policy_absent() {
  local policy_arn="$1"
  local err_file="${WORK_DIR}/aws-iam-policy-absent.err"

  AWS_ABSENCE_LAST_ERROR=""
  if aws iam get-policy --policy-arn "$policy_arn" >/dev/null 2>"$err_file"; then
    AWS_ABSENCE_LAST_ERROR="IAM Policy still exists"
    return 1
  fi
  if grep -Eiq 'NoSuchEntity|not found|cannot be found' "$err_file"; then
    return 0
  fi

  AWS_ABSENCE_LAST_ERROR="$(file_summary "$err_file")"
  return 1
}

aws_iam_oidc_provider_absent() {
  local provider_arn="$1"
  local err_file="${WORK_DIR}/aws-iam-oidc-provider-absent.err"

  AWS_ABSENCE_LAST_ERROR=""
  if aws iam get-open-id-connect-provider --open-id-connect-provider-arn "$provider_arn" >/dev/null 2>"$err_file"; then
    AWS_ABSENCE_LAST_ERROR="IAM OIDC Provider still exists"
    return 1
  fi
  if grep -Eiq 'NoSuchEntity|not found|cannot be found' "$err_file"; then
    return 0
  fi

  AWS_ABSENCE_LAST_ERROR="$(file_summary "$err_file")"
  return 1
}

aws_s3_bucket_absent() {
  local bucket="$1"
  local err_file="${WORK_DIR}/aws-s3-bucket-absent.err"

  AWS_ABSENCE_LAST_ERROR=""
  if aws s3api head-bucket --bucket "$bucket" >/dev/null 2>"$err_file"; then
    AWS_ABSENCE_LAST_ERROR="S3 bucket still exists"
    return 1
  fi
  if grep -Eiq 'NoSuchBucket|Not Found|\(404\)|(^|[^0-9])404([^0-9]|$)' "$err_file"; then
    return 0
  fi

  AWS_ABSENCE_LAST_ERROR="$(file_summary "$err_file")"
  return 1
}

wait_for_aws_absent_until() {
  local description="$1"
  local deadline="$2"
  shift 2

  while true; do
    if "$@"; then
      log "${description} is absent in AWS"
      return 0
    fi

    if (( $(date +%s) > deadline )); then
      log "${description} was not absent before timeout: ${AWS_ABSENCE_LAST_ERROR:-unknown AWS CLI response}"
      return 1
    fi

    sleep 10
  done
}

verify_managed_aws_resources_absent() {
  local timeout="$1"
  local deadline role_name checked="0" failed="0"

  capture_aws_cleanup_identifiers
  deadline=$(( $(date +%s) + timeout ))

  if [[ -n "$ROLE_ARN" ]]; then
    if ! role_name="$(iam_role_name_from_arn "$ROLE_ARN")"; then
      log "could not parse IAM Role name from ARN ${ROLE_ARN}"
      failed="1"
    else
      checked="1"
      wait_for_aws_absent_until "AWS IAM Role ${role_name}" "$deadline" aws_iam_role_absent "$role_name" || failed="1"
    fi
  fi

  if [[ -n "$GENERATED_POLICY_ARN" ]]; then
    checked="1"
    wait_for_aws_absent_until "AWS generated IAM Policy ${GENERATED_POLICY_ARN}" "$deadline" aws_iam_policy_absent "$GENERATED_POLICY_ARN" || failed="1"
  fi

  if [[ -n "$TOKEN_FILE_ROLE_ARN" ]]; then
    if ! role_name="$(iam_role_name_from_arn "$TOKEN_FILE_ROLE_ARN")"; then
      log "could not parse token-file IAM Role name from ARN ${TOKEN_FILE_ROLE_ARN}"
      failed="1"
    else
      checked="1"
      wait_for_aws_absent_until "AWS token-file IAM Role ${role_name}" "$deadline" aws_iam_role_absent "$role_name" || failed="1"
    fi
  fi

  if [[ -n "$TOKEN_FILE_GENERATED_POLICY_ARN" ]]; then
    checked="1"
    wait_for_aws_absent_until "AWS token-file generated IAM Policy ${TOKEN_FILE_GENERATED_POLICY_ARN}" "$deadline" aws_iam_policy_absent "$TOKEN_FILE_GENERATED_POLICY_ARN" || failed="1"
  fi

  if [[ -n "$OIDC_PROVIDER_ARN" ]]; then
    checked="1"
    wait_for_aws_absent_until "AWS IAM OIDC Provider ${OIDC_PROVIDER_ARN}" "$deadline" aws_iam_oidc_provider_absent "$OIDC_PROVIDER_ARN" || failed="1"
  fi

  if [[ -n "$BUCKET_NAME" ]]; then
    checked="1"
    wait_for_aws_absent_until "AWS S3 bucket ${BUCKET_NAME}" "$deadline" aws_s3_bucket_absent "$BUCKET_NAME" || failed="1"
  fi

  if [[ "$checked" == "0" ]]; then
    log "no AWS resource identifiers were captured; skipping AWS absence verification"
    return 0
  fi

  if [[ "$failed" == "1" ]]; then
    return 1
  fi

  log "AWS managed resource cleanup verified"
}

cleanup() {
  local exit_code=$?
  local cleanup_failed="0"
  local preserve_clusters_for_cleanup_failure="0"
  local wait_for_finalizers="0"
  local awio_resources_deleted="0"
  set +e

  if (( exit_code != 0 )); then
    diagnostics
  fi

  log "cleaning e2e resources"
  if hub_kubeconfig_ready; then
    capture_aws_cleanup_identifiers
    hub_kubectl -n "$AWIO_NAMESPACE" delete job "$AWS_IRSA_SIDECAR_JOB_NAME" --ignore-not-found=true --timeout=60s >/dev/null 2>&1 || true
    hub_kubectl -n "$WLC_NAMESPACE" delete manifestwork "$AWS_IRSA_SIDECAR_PERMISSIONS_NAME" --ignore-not-found=true --timeout=60s >/dev/null 2>&1 || true
    if delete_hub_object_if_crd_exists awsserviceaccountroles.aws.identity.appthrust.io "$WLC_NAMESPACE" awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" 180 &&
      delete_hub_object_if_crd_exists awsserviceaccountroles.aws.identity.appthrust.io "$WLC_NAMESPACE" awsserviceaccountrole "$ROLE_RESOURCE_NAME" 180 &&
      delete_hub_object_if_crd_exists awsworkloadidentityconfigs.aws.identity.appthrust.io "$WLC_NAMESPACE" awsworkloadidentityconfig default 180; then
      awio_resources_deleted="1"
    else
      log "AWIO custom resources did not fully delete; keeping Helm-owned OCM access resources for finalizers"
      cleanup_failed="1"
      preserve_clusters_for_cleanup_failure="1"
    fi
    wait_for_finalizers="1"
    if [[ "$awio_resources_deleted" == "1" ]] && ! verify_managed_aws_resources_absent 900; then
      log "AWS managed resources did not fully disappear; keeping hub kind cluster ${HUB_KIND_CLUSTER_NAME} for ACK diagnostics"
      cleanup_failed="1"
      preserve_clusters_for_cleanup_failure="1"
    fi
    if [[ "$awio_resources_deleted" == "1" && "$preserve_clusters_for_cleanup_failure" != "1" ]]; then
      hub_helm uninstall "$AWIO_RELEASE" \
        --namespace "$AWIO_NAMESPACE" \
        --ignore-not-found \
        --wait \
        --timeout "$HELM_TIMEOUT" >/dev/null 2>&1 || true
    fi
  fi
  if managed_kubeconfig_ready; then
    if [[ "$preserve_clusters_for_cleanup_failure" == "1" ]]; then
      log "skipping managed cluster cleanup because cleanup is being preserved for inspection"
    else
      managed_kubectl -n "$APP_NAMESPACE" delete job awio-sts-canary --ignore-not-found=true --timeout=60s >/dev/null 2>&1 || true
      if [[ -n "${MANAGED_CONTEXT:-}" ]]; then
        clusteradm_cmd unjoin --cluster-name "$WLC_NAMESPACE" --context "$MANAGED_CONTEXT" >/dev/null 2>&1 || true
      fi
      wait_for_finalizers="1"
    fi
  fi

  if [[ "$wait_for_finalizers" == "1" ]]; then
    sleep 10
  fi

  if [[ "$preserve_clusters_for_cleanup_failure" == "1" ]]; then
    log "cleanup did not fully complete; keeping hub kind cluster ${HUB_KIND_CLUSTER_NAME} and workload cluster ${WLC_NAMESPACE} for inspection"
    log "hub context: ${HUB_CONTEXT:-<unresolved>}; managed context: ${MANAGED_CONTEXT:-<unresolved>}; kubeconfig: ${KUBECONFIG_MERGE_TARGET}"
  elif hub_kubeconfig_ready; then
    hub_kubectl -n "$WLC_NAMESPACE" delete cluster "$WLC_NAMESPACE" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    hub_kubectl delete namespace "$WLC_NAMESPACE" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
  fi

  if [[ "$preserve_clusters_for_cleanup_failure" != "1" && "$CAPD_PROVIDER" == docker* ]]; then
    kind delete cluster --name "$WLC_NAMESPACE" >/dev/null 2>&1 || true
  fi

  if [[ "$preserve_clusters_for_cleanup_failure" != "1" ]]; then
    kind delete cluster --name "$HUB_KIND_CLUSTER_NAME" >/dev/null 2>&1 || true
  fi

  rm -rf "$WORK_DIR"
  if (( exit_code == 0 )) && [[ "$cleanup_failed" == "1" ]]; then
    log "cleanup failed after successful e2e run"
    exit 1
  fi
  exit "$exit_code"
}

trap cleanup EXIT

preflight() {
  local aws_err

  need aws
  need docker
  need go
  need helm
  need jq
  need kind
  need kubectl
  need openssl
  need sha256sum
  need clusterctl
  require_python_yq
  mkdir -p "${WORK_DIR}/bin"

  [[ -d "${ROOT_DIR}/cmd/aws-remote-irsa-credential-process" ]] ||
    die "cmd/aws-remote-irsa-credential-process is required before running this e2e"
  [[ -d "${ROOT_DIR}/cmd/aws-irsa-sidecar" ]] ||
    die "cmd/aws-irsa-sidecar is required before running this e2e"

  if [[ -z "$AWS_REGION" ]]; then
    AWS_REGION="$(aws configure get region 2>/dev/null || true)"
  fi
  [[ -n "$AWS_REGION" ]] || die "AWS_REGION or AWS_DEFAULT_REGION is required"

  aws_err="${WORK_DIR}/aws-sts-get-caller-identity.err"
  if ! aws sts get-caller-identity >/dev/null 2>"$aws_err"; then
    die_aws_error "AWS credentials are not available" "$aws_err"
  fi

  docker info >/dev/null

  DOCKER_SOCK="$(resolve_docker_sock)"
  [[ -S "$DOCKER_SOCK" ]] || die "Docker socket ${DOCKER_SOCK} does not exist"

  if ! command -v clusteradm >/dev/null 2>&1; then
    mkdir -p "${WORK_DIR}/bin"
    log "installing clusteradm latest into ${WORK_DIR}/bin"
    GOBIN="${WORK_DIR}/bin" go install "open-cluster-management.io/clusteradm/cmd/clusteradm@latest"
  fi

  prepare_kubeconfig_merge_target
}

parse_operator_image() {
  local image_without_tag first_component

  AWIO_IMAGE_TAG="${AWIO_IMAGE##*:}"
  image_without_tag="${AWIO_IMAGE%:*}"
  if [[ "$image_without_tag" == "$AWIO_IMAGE" ]]; then
    die "operator image must include a tag"
  fi

  first_component="${image_without_tag%%/*}"
  if [[ "$image_without_tag" == */* && ("$first_component" == *.* || "$first_component" == *:* || "$first_component" == "localhost") ]]; then
    AWIO_IMAGE_REGISTRY="$first_component"
    AWIO_IMAGE_REPOSITORY="${image_without_tag#*/}"
  else
    AWIO_IMAGE_REGISTRY=""
    AWIO_IMAGE_REPOSITORY="$image_without_tag"
  fi
}

load_aws_credentials() {
  local env_file="${WORK_DIR}/aws-credentials.env"
  local export_err="${WORK_DIR}/aws-export-credentials.err"
  local shared_file="${WORK_DIR}/aws-credentials"

  if ! aws configure export-credentials --format env-no-export >"$env_file" 2>"$export_err"; then
    die_aws_error "AWS credentials could not be exported" "$export_err"
  fi
  set -a
  # shellcheck disable=SC1090
  source "$env_file"
  set +a

  [[ -n "${AWS_ACCESS_KEY_ID:-}" ]] || die "AWS_ACCESS_KEY_ID could not be resolved"
  [[ -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || die "AWS_SECRET_ACCESS_KEY could not be resolved"

  {
    printf '[default]\n'
    printf 'aws_access_key_id=%s\n' "$AWS_ACCESS_KEY_ID"
    printf 'aws_secret_access_key=%s\n' "$AWS_SECRET_ACCESS_KEY"
    if [[ -n "${AWS_SESSION_TOKEN:-}" ]]; then
      printf 'aws_session_token=%s\n' "$AWS_SESSION_TOKEN"
    fi
  } >"$shared_file"
}

build_remote_irsa_helper() {
  log "building remote IRSA helper and IRSA sidecar binaries"
  (cd "$ROOT_DIR" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$HELPER_BINARY" ./cmd/aws-remote-irsa-credential-process)
  (cd "$ROOT_DIR" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$IRSA_SIDECAR_BINARY" ./cmd/aws-irsa-sidecar)
}

install_cp_creds_plugin() {
  local container_id
  local cp_creds="${WORK_DIR}/bin/cp-creds"

  log "extracting cp-creds access-provider plugin from ${CP_CREDS_IMAGE}"
  docker pull "$CP_CREDS_IMAGE" >/dev/null
  container_id="$(docker create "$CP_CREDS_IMAGE")"
  trap 'docker rm -f "$container_id" >/dev/null 2>&1 || true' RETURN
  if ! docker cp "${container_id}:/plugins/cp-creds" "$cp_creds" 2>/dev/null; then
    docker cp "${container_id}:/cp-creds" "$cp_creds"
  fi
  docker rm -f "$container_id" >/dev/null 2>&1 || true
  trap - RETURN
  chmod +x "$cp_creds"
}

write_helper_provider_file() {
  jq -n \
    --arg command /cp-creds \
    --arg msa "$MANAGED_SERVICE_ACCOUNT" \
    '{
      providers: [{
        name: "open-cluster-management",
        execConfig: {
          apiVersion: "client.authentication.k8s.io/v1",
          command: $command,
          args: ["--managed-serviceaccount=" + $msa],
          provideClusterInfo: true,
          interactiveMode: "Never"
        }
      }]
    }' >"$PROVIDER_FILE"
}

build_and_load_remote_irsa_helper_image() {
  local dockerfile="${WORK_DIR}/remote-irsa-helper.Dockerfile"

  cat >"$dockerfile" <<'EOF'
FROM gcr.io/distroless/static:nonroot
COPY bin/aws-remote-irsa-credential-process /aws-remote-irsa-credential-process
COPY bin/aws-irsa-sidecar /aws-irsa-sidecar
COPY bin/cp-creds /cp-creds
COPY clusterprofile-provider-file.json /clusterprofile-provider-file.json
USER 65532:65532
ENTRYPOINT ["/aws-remote-irsa-credential-process"]
EOF

  docker buildx build -f "$dockerfile" -t "$REMOTE_HELPER_IMAGE" --load "$WORK_DIR"
  kind load docker-image "$REMOTE_HELPER_IMAGE" --name "$HUB_KIND_CLUSTER_NAME"
}

generate_service_account_keys() {
  openssl genrsa -out "${WORK_DIR}/sa.key" 2048 >/dev/null 2>&1
  openssl rsa -in "${WORK_DIR}/sa.key" -pubout -out "${WORK_DIR}/sa.pub" >/dev/null 2>&1
}

prepare_hub_cluster() {
  local kind_config="${WORK_DIR}/hub-kind.yaml"

  HUB_KUBECONFIG="${WORK_DIR}/hub-kubeconfig"

  cat >"$kind_config" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: ${DOCKER_SOCK}
        containerPath: /var/run/docker.sock
EOF

  log "creating hub kind cluster ${HUB_KIND_CLUSTER_NAME}"
  kind create cluster \
    --name "$HUB_KIND_CLUSTER_NAME" \
    --config "$kind_config" \
    --kubeconfig "$HUB_KUBECONFIG"

  hub_kubectl version >/dev/null
  CAPD_KUBERNETES_VERSION="$(hub_kubectl version -o json | jq -r '.serverVersion.gitVersion')"
  [[ "$CAPD_KUBERNETES_VERSION" == v* ]] || die "could not resolve hub Kubernetes server version for CAPD workload cluster"
  HUB_CONTEXT="$(hub_kubectl config current-context)"
  HUB_READY="1"
  log "hub context: ${HUB_CONTEXT}"
  log "CAPD Kubernetes version: ${CAPD_KUBERNETES_VERSION}"
  merge_kubeconfig_for_k9s "$HUB_KUBECONFIG" "hub"
}

install_hub_cert_manager() {
  hub_kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  hub_kubectl -n cert-manager rollout status deploy/cert-manager --timeout="$HELM_TIMEOUT"
  hub_kubectl -n cert-manager rollout status deploy/cert-manager-cainjector --timeout="$HELM_TIMEOUT"
  hub_kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout="$HELM_TIMEOUT"
}

install_cluster_inventory_crds() {
  local cluster_inventory_dir
  cluster_inventory_dir="$(GOWORK=off go list -m -f '{{.Dir}}' sigs.k8s.io/cluster-inventory-api)"
  hub_kubectl apply -f "${cluster_inventory_dir}/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml"
}

configure_ack_credentials() {
  hub_kubectl create namespace "$ACK_NAMESPACE" --dry-run=client -o yaml | hub_kubectl apply -f -
  hub_kubectl -n "$ACK_NAMESPACE" create secret generic aws-credentials \
    --from-file=credentials="${WORK_DIR}/aws-credentials" \
    --dry-run=client -o yaml | hub_kubectl apply -f -
}

install_ack_controllers() {
  local iam_dir s3_dir
  iam_dir="$(GOWORK=off go list -m -f '{{.Dir}}' github.com/aws-controllers-k8s/iam-controller)"
  s3_dir="$(GOWORK=off go list -m -f '{{.Dir}}' github.com/aws-controllers-k8s/s3-controller)"

  configure_ack_credentials

  hub_helm upgrade --install ack-iam "${iam_dir}/helm" \
    --namespace "$ACK_NAMESPACE" \
    --set-string fullnameOverride=ack-iam-controller \
    --set-string aws.region="$AWS_REGION" \
    --set-string aws.credentials.secretName=aws-credentials \
    --set-string aws.credentials.profile=default \
    --wait \
    --timeout "$HELM_TIMEOUT"

  hub_helm upgrade --install ack-s3 "${s3_dir}/helm" \
    --namespace "$ACK_NAMESPACE" \
    --set-string fullnameOverride=ack-s3-controller \
    --set-string aws.region="$AWS_REGION" \
    --set-string aws.credentials.secretName=aws-credentials \
    --set-string aws.credentials.profile=default \
    --wait \
    --timeout "$HELM_TIMEOUT"
}

install_ocm() {
  local cluster_proxy_post_renderer

  cluster_proxy_post_renderer="$(write_cluster_proxy_post_renderer)"

  clusteradm_cmd init --wait --feature-gates=ClusterProfile=true --context "$HUB_CONTEXT"

  hub_helm repo add ocm https://open-cluster-management.io/helm-charts >/dev/null 2>&1 || true
  hub_helm repo update >/dev/null
  hub_helm upgrade --install cluster-proxy ocm/cluster-proxy \
    --version "$OCM_CLUSTER_PROXY_CHART_VERSION" \
    --namespace open-cluster-management-cluster-proxy \
    --create-namespace \
    --post-renderer "$cluster_proxy_post_renderer" \
    --set featureGates.clusterProfileAccessProvider=true \
    --set enableServiceProxy=true \
    --set userServer.enabled=true \
    --wait \
    --timeout "$HELM_TIMEOUT"
  hub_helm upgrade --install managed-serviceaccount ocm/managed-serviceaccount \
    --version "$OCM_MANAGED_SERVICEACCOUNT_CHART_VERSION" \
    --namespace open-cluster-management-managed-serviceaccount \
    --create-namespace \
    --set featureGates.clusterProfileCredSyncer=true \
    --wait \
    --timeout "$HELM_TIMEOUT"
}

bind_operator_clusterprofile_namespace() {
  hub_kubectl create namespace "$AWIO_NAMESPACE" --dry-run=client -o yaml | hub_kubectl apply -f -
  hub_kubectl config set-context "$HUB_CONTEXT" --namespace "$AWIO_NAMESPACE" >/dev/null

  cat <<EOF | hub_kubectl apply -f -
apiVersion: cluster.open-cluster-management.io/v1beta2
kind: ManagedClusterSetBinding
metadata:
  name: global
  namespace: ${AWIO_NAMESPACE}
spec:
  clusterSet: global
EOF
}

install_capd() {
  install_cluster_api_provider --core "cluster-api:${CLUSTER_API_PROVIDER_VERSION}" capi-core
  install_cluster_api_provider --bootstrap "kubeadm:${CLUSTER_API_PROVIDER_VERSION}" capi-kubeadm-bootstrap
  install_cluster_api_provider --control-plane "kubeadm:${CLUSTER_API_PROVIDER_VERSION}" capi-kubeadm-control-plane
  install_cluster_api_provider --infrastructure "$CAPD_PROVIDER" capd

  hub_kubectl -n capd-system rollout status deploy/capd-controller-manager --timeout="$HELM_TIMEOUT"
  hub_kubectl -n capi-system rollout status deploy/capi-controller-manager --timeout="$HELM_TIMEOUT"
  hub_kubectl -n capi-kubeadm-control-plane-system rollout status deploy/capi-kubeadm-control-plane-controller-manager --timeout="$HELM_TIMEOUT"
  hub_kubectl -n capi-kubeadm-bootstrap-system rollout status deploy/capi-kubeadm-bootstrap-controller-manager --timeout="$HELM_TIMEOUT"
}

patch_capd_manifest_for_irsa() {
  local raw_manifest="$1"
  local patched_manifest="$2"
  local issuer_url="$3"

  yq -y --explicit-start \
    --arg issuer "$issuer_url" \
    --arg work "$WORK_DIR" \
    '
      def awio_args: [
        {"name":"service-account-issuer","value":$issuer},
        {"name":"service-account-signing-key-file","value":"/etc/kubernetes/pki/awio-sa.key"},
        {"name":"service-account-key-file","value":"/etc/kubernetes/pki/awio-sa.pub"},
        {"name":"api-audiences","value":($issuer + ",https://kubernetes.default.svc,sts.amazonaws.com")}
      ];
      def awio_volumes: [
        {"name":"awio-sa-key","hostPath":"/etc/kubernetes/pki/awio-sa.key","mountPath":"/etc/kubernetes/pki/awio-sa.key","pathType":"File","readOnly":true},
        {"name":"awio-sa-pub","hostPath":"/etc/kubernetes/pki/awio-sa.pub","mountPath":"/etc/kubernetes/pki/awio-sa.pub","pathType":"File","readOnly":true}
      ];
      if .kind == "ClusterClass" then
        (.spec.patches[]?.definitions[]?.jsonPatches[]? |
          select(.path == "/spec/template/spec/kubeadmConfigSpec/clusterConfiguration/apiServer/extraArgs") |
          .value) |= ((. // []) + awio_args)
        | (.spec.patches[]?.definitions[]?.jsonPatches[]? |
          select(.path == "/spec/template/spec/kubeadmConfigSpec/clusterConfiguration/apiServer/extraVolumes") |
          .value) |= ((. // []) + awio_volumes)
      elif .kind == "DockerMachineTemplate" and .metadata.name == "quick-start-control-plane" then
        .spec.template.spec.extraMounts =
          ((.spec.template.spec.extraMounts // []) + [
            {"hostPath":($work + "/sa.key"),"containerPath":"/etc/kubernetes/pki/awio-sa.key","readOnly":true},
            {"hostPath":($work + "/sa.pub"),"containerPath":"/etc/kubernetes/pki/awio-sa.pub","readOnly":true}
          ])
      else
        .
      end
    ' "$raw_manifest" >"$patched_manifest"
}

hub_kubectl_apply_with_retry() {
  local file="$1"
  local timeout="${2:-180}"
  local start

  start="$(date +%s)"
  while true; do
    if hub_kubectl apply -f "$file"; then
      return 0
    fi

    if (( $(date +%s) - start > timeout )); then
      log "kubectl apply ${file} did not succeed before timeout"
      return 1
    fi

    log "retrying kubectl apply ${file}"
    sleep 5
  done
}

create_capd_workload_cluster() {
  local issuer_url="$1"
  local raw_manifest="${WORK_DIR}/capd-cluster.raw.yaml"
  local patched_manifest="${WORK_DIR}/capd-cluster.yaml"

  hub_kubectl create namespace "$WLC_NAMESPACE" --dry-run=client -o yaml | hub_kubectl apply -f -

  CLUSTER_TOPOLOGY=true clusterctl generate cluster "$WLC_NAMESPACE" \
    --kubeconfig "$HUB_KUBECONFIG" \
    --target-namespace "$WLC_NAMESPACE" \
    --infrastructure "$CAPD_PROVIDER" \
    --flavor "$CAPD_FLAVOR" \
    --kubernetes-version "$CAPD_KUBERNETES_VERSION" \
    --control-plane-machine-count 1 \
    --worker-machine-count 1 \
    >"$raw_manifest"

  patch_capd_manifest_for_irsa "$raw_manifest" "$patched_manifest" "$issuer_url"
  hub_kubectl_apply_with_retry "$patched_manifest" 180

  wait_for_managed_kubeconfig
  install_workload_cni
  wait_for_json_condition_hub "$WLC_NAMESPACE" cluster "$WLC_NAMESPACE" ControlPlaneAvailable "$TIMEOUT_SECONDS"
  wait_for_json_condition_hub "$WLC_NAMESPACE" cluster "$WLC_NAMESPACE" WorkersAvailable "$TIMEOUT_SECONDS"
}

wait_for_managed_kubeconfig() {
  local start
  MANAGED_KUBECONFIG="${WORK_DIR}/managed-kubeconfig"
  start="$(date +%s)"

  while true; do
    if clusterctl get kubeconfig "$WLC_NAMESPACE" \
      --namespace "$WLC_NAMESPACE" \
      --kubeconfig "$HUB_KUBECONFIG" \
      >"$MANAGED_KUBECONFIG" 2>/dev/null; then
      MANAGED_CONTEXT="$(managed_kubectl config current-context)"
      MANAGED_READY="1"
      log "managed context: ${MANAGED_CONTEXT}"
      merge_kubeconfig_for_k9s "$MANAGED_KUBECONFIG" "managed"
      log "managed kubeconfig resolved"
      return 0
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "managed kubeconfig was not created"
      hub_kubectl -n "$WLC_NAMESPACE" get cluster,machine,kubeadmcontrolplane -o wide || true
      return 1
    fi

    sleep 5
  done
}

wait_for_managed_api() {
  local start
  start="$(date +%s)"

  while true; do
    if managed_kubectl --request-timeout=5s get --raw=/readyz >/dev/null 2>&1; then
      log "managed cluster API is ready"
      return 0
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "managed cluster API did not become ready"
      return 1
    fi

    sleep 5
  done
}

install_workload_cni() {
  local start node_count

  wait_for_managed_api
  managed_kubectl apply --validate=false -f "$CNI_MANIFEST_URL"
  start="$(date +%s)"
  while true; do
    node_count="$(managed_kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "$node_count" != "0" ]]; then
      break
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "managed cluster nodes were not visible"
      return 1
    fi

    sleep 5
  done

  managed_kubectl wait --for=condition=Ready nodes --all --timeout="$HELM_TIMEOUT"
}

register_managed_cluster_with_ocm() {
  local hub_apiserver klusterlet_values_file token token_json

  klusterlet_values_file="$(write_klusterlet_values_file)"

  token_json="$(clusteradm_cmd get token --context "$HUB_CONTEXT" -o json)"
  token="$(jq -r '."hub-token"' <<<"$token_json")"
  hub_apiserver="$(jq -r '."hub-apiserver"' <<<"$token_json")"
  [[ "$token" != "null" && -n "$token" ]] || die "failed to resolve OCM hub token"
  [[ "$hub_apiserver" != "null" && -n "$hub_apiserver" ]] || die "failed to resolve OCM hub apiserver"

  clusteradm_cmd join \
    --hub-token "$token" \
    --hub-apiserver "$hub_apiserver" \
    --cluster-name "$WLC_NAMESPACE" \
    --feature-gates=ClusterProperty=true \
    --force-internal-endpoint-lookup \
    --klusterlet-values-file "$klusterlet_values_file" \
    --context "$MANAGED_CONTEXT"

  clusteradm_cmd accept --clusters "$WLC_NAMESPACE" --wait --context "$HUB_CONTEXT"
}

enable_ocm_addons_for_cluster() {
  cat <<EOF | hub_kubectl apply --server-side --force-conflicts --field-manager=awio-e2e -f -
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: ManagedClusterAddOn
metadata:
  name: cluster-proxy
  namespace: ${WLC_NAMESPACE}
spec:
  installNamespace: open-cluster-management-agent-addon
---
apiVersion: addon.open-cluster-management.io/v1alpha1
kind: ManagedClusterAddOn
metadata:
  name: managed-serviceaccount
  namespace: ${WLC_NAMESPACE}
spec:
  installNamespace: open-cluster-management-managed-serviceaccount
EOF

  wait_for_json_condition_hub "$WLC_NAMESPACE" managedclusteraddon cluster-proxy Available "$TIMEOUT_SECONDS"
  wait_for_json_condition_hub "$WLC_NAMESPACE" managedclusteraddon managed-serviceaccount Available "$TIMEOUT_SECONDS"
}

wait_for_managed_service_account_access() {
  wait_for_json_condition_hub "$WLC_NAMESPACE" managedserviceaccount "$MANAGED_SERVICE_ACCOUNT" TokenReported "$TIMEOUT_SECONDS"
  wait_for_json_condition_hub "$WLC_NAMESPACE" manifestwork "$OCM_REMOTE_PERMISSIONS_NAME" Applied "$TIMEOUT_SECONDS"
  wait_for_clusterprofile
  wait_for_clusterprofile_credential
}

prepare_workload_application() {
  managed_kubectl create namespace "$APP_NAMESPACE" --dry-run=client -o yaml | managed_kubectl apply -f -
  managed_kubectl -n "$APP_NAMESPACE" create serviceaccount "$APP_SERVICE_ACCOUNT" --dry-run=client -o yaml | managed_kubectl apply -f -
  managed_kubectl -n "$APP_NAMESPACE" create serviceaccount "$DENIED_SERVICE_ACCOUNT" --dry-run=client -o yaml | managed_kubectl apply -f -
  managed_kubectl -n "$APP_NAMESPACE" create serviceaccount "$MISMATCH_SERVICE_ACCOUNT" --dry-run=client -o yaml | managed_kubectl apply -f -
}

grant_consumer_tokenrequest_permissions() {
  local addon_namespace

  addon_namespace="$(hub_kubectl -n "$WLC_NAMESPACE" get managedclusteraddon managed-serviceaccount -o jsonpath='{.status.namespace}' 2>/dev/null || true)"
  if [[ -z "$addon_namespace" ]]; then
    addon_namespace="open-cluster-management-managed-serviceaccount"
  fi
  REMOTE_ACCESS_NAMESPACE="$addon_namespace"
  REMOTE_ACCESS_SUBJECT="system:serviceaccount:${addon_namespace}:${MANAGED_SERVICE_ACCOUNT}"

  cat <<EOF | hub_kubectl apply -f -
apiVersion: work.open-cluster-management.io/v1
kind: ManifestWork
metadata:
  name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}
  namespace: ${WLC_NAMESPACE}
spec:
  workload:
    manifests:
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: Role
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}
          namespace: ${APP_NAMESPACE}
        rules:
          - apiGroups:
              - ""
            resources:
              - serviceaccounts/token
            resourceNames:
              - ${APP_SERVICE_ACCOUNT}
              - ${MISMATCH_SERVICE_ACCOUNT}
            verbs:
              - create
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: RoleBinding
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}
          namespace: ${APP_NAMESPACE}
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: Role
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}
        subjects:
          - kind: ServiceAccount
            name: ${MANAGED_SERVICE_ACCOUNT}
            namespace: ${addon_namespace}
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: Role
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-self
          namespace: ${addon_namespace}
        rules:
          - apiGroups:
              - ""
            resources:
              - serviceaccounts
            resourceNames:
              - ${MANAGED_SERVICE_ACCOUNT}
            verbs:
              - get
          - apiGroups:
              - ""
            resources:
              - serviceaccounts/token
            resourceNames:
              - ${MANAGED_SERVICE_ACCOUNT}
            verbs:
              - create
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: RoleBinding
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-self
          namespace: ${addon_namespace}
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: Role
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-self
        subjects:
          - kind: ServiceAccount
            name: ${MANAGED_SERVICE_ACCOUNT}
            namespace: ${addon_namespace}
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-selfsubjectreview
        rules:
          - apiGroups:
              - authentication.k8s.io
            resources:
              - selfsubjectreviews
            verbs:
              - create
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        metadata:
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-selfsubjectreview
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: ${AWS_IRSA_SIDECAR_PERMISSIONS_NAME}-selfsubjectreview
        subjects:
          - kind: ServiceAccount
            name: ${MANAGED_SERVICE_ACCOUNT}
            namespace: ${addon_namespace}
EOF

  wait_for_json_condition_hub "$WLC_NAMESPACE" manifestwork "$AWS_IRSA_SIDECAR_PERMISSIONS_NAME" Applied "$TIMEOUT_SECONDS"
}

publish_remote_cluster_properties() {
  cat <<EOF | hub_kubectl apply -f -
apiVersion: work.open-cluster-management.io/v1
kind: ManifestWork
metadata:
  name: ${REMOTE_IRSA_CLUSTER_PROPERTIES_NAME}
  namespace: ${WLC_NAMESPACE}
spec:
  workload:
    manifests:
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: ${REMOTE_IRSA_CLUSTER_PROPERTIES_NAME}
        rules:
          - apiGroups:
              - about.k8s.io
            resources:
              - clusterproperties
            verbs:
              - get
              - list
              - watch
              - create
              - update
              - patch
              - delete
      - apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        metadata:
          name: ${REMOTE_IRSA_CLUSTER_PROPERTIES_NAME}
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: ${REMOTE_IRSA_CLUSTER_PROPERTIES_NAME}
        subjects:
          - kind: ServiceAccount
            name: klusterlet-work-sa
            namespace: open-cluster-management-agent
      - apiVersion: about.k8s.io/v1alpha1
        kind: ClusterProperty
        metadata:
          name: ${AWS_REGION_CLUSTER_PROPERTY}
        spec:
          value: ${AWS_REGION}
EOF

  wait_for_json_condition_hub "$WLC_NAMESPACE" manifestwork "$REMOTE_IRSA_CLUSTER_PROPERTIES_NAME" Applied "$TIMEOUT_SECONDS"
  wait_for_clusterprofile
  wait_for_clusterprofile_property "$AWS_REGION_CLUSTER_PROPERTY" "$AWS_REGION"
}

build_and_install_operator() {
  local values_file="${WORK_DIR}/operator-values.yaml"

  docker buildx build -t "$AWIO_IMAGE" --load "$ROOT_DIR"
  kind load docker-image "$AWIO_IMAGE" --name "$HUB_KIND_CLUSTER_NAME"

  hub_kubectl create namespace "$AWIO_NAMESPACE" --dry-run=client -o yaml | hub_kubectl apply -f -
  hub_kubectl -n "$AWIO_NAMESPACE" create secret generic awio-e2e-aws-env \
    --from-literal=AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
    --from-literal=AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
    --from-literal=AWS_SESSION_TOKEN="${AWS_SESSION_TOKEN:-}" \
    --from-literal=AWS_REGION="$AWS_REGION" \
    --from-literal=AWS_DEFAULT_REGION="$AWS_REGION" \
    --dry-run=client -o yaml | hub_kubectl apply -f -

  cat >"$values_file" <<EOF
global:
  imageRegistry: "${AWIO_IMAGE_REGISTRY}"
image:
  registry: ""
  repository: "${AWIO_IMAGE_REPOSITORY}"
  tag: "${AWIO_IMAGE_TAG}"
  pullPolicy: IfNotPresent
logging:
  exporter: console
  level: debug
operator:
  leaderElection:
    enabled: false
  podIdentityWebhookImage: "${POD_IDENTITY_WEBHOOK_IMAGE}"
ocm:
  managedServiceAccount:
    name: "${MANAGED_SERVICE_ACCOUNT}"
    create: true
    namespaces:
      - "${WLC_NAMESPACE}"
    remotePermissions:
      name: "${OCM_REMOTE_PERMISSIONS_NAME}"
extraEnvVarsSecret: awio-e2e-aws-env
operatorConfig:
  create: false
EOF

  hub_helm upgrade --install "$AWIO_RELEASE" "$ROOT_DIR/charts/aws-workload-identity-operator" \
    --namespace "$AWIO_NAMESPACE" \
    --values "$values_file" \
    --timeout "$HELM_TIMEOUT"
}

wait_for_operator_rollout() {
  hub_kubectl -n "$AWIO_NAMESPACE" rollout status "deploy/${AWIO_FULLNAME}" --timeout="$HELM_TIMEOUT"
  hub_kubectl -n "$AWIO_NAMESPACE" wait --for=condition=Ready certificate --all --timeout="$HELM_TIMEOUT"
  wait_for_nonempty_hub_jsonpath "validating webhook CA bundle" "$TIMEOUT_SECONDS" \
    hub_kubectl get validatingwebhookconfiguration "${AWIO_FULLNAME}-validating" -o jsonpath='{.webhooks[0].clientConfig.caBundle}'
}

apply_workload_identity_resources() {
  hub_kubectl -n "$WLC_NAMESPACE" create secret generic awi-signing-key-default \
    --from-file=sa.key="${WORK_DIR}/sa.key" \
    --from-file=sa.pub="${WORK_DIR}/sa.pub" \
    --dry-run=client -o yaml | hub_kubectl apply -f -

  cat <<EOF | hub_kubectl apply -f -
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityOperatorConfig
metadata:
  name: default
spec:
  selfHostedIRSA:
    webhookNamespace: aws-pod-identity-webhook
---
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSWorkloadIdentityConfig
metadata:
  name: default
  namespace: ${WLC_NAMESPACE}
spec:
  type: SelfHostedIRSA
  region: ${AWS_REGION}
---
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: ${ROLE_RESOURCE_NAME}
  namespace: ${WLC_NAMESPACE}
spec:
  serviceAccount:
    namespace: ${APP_NAMESPACE}
    name: ${APP_SERVICE_ACCOUNT}
  policyDocument:
    Version: "2012-10-17"
    Statement:
      - Effect: Allow
        Action:
          - sts:GetCallerIdentity
        Resource: "*"
---
apiVersion: aws.identity.appthrust.io/v1alpha1
kind: AWSServiceAccountRole
metadata:
  name: ${TOKEN_FILE_ROLE_RESOURCE_NAME}
  namespace: ${WLC_NAMESPACE}
spec:
  serviceAccount:
    namespace: ${REMOTE_ACCESS_NAMESPACE}
    name: ${MANAGED_SERVICE_ACCOUNT}
  policyDocument:
    Version: "2012-10-17"
    Statement:
      - Effect: Allow
        Action:
          - sts:GetCallerIdentity
        Resource: "*"
EOF
}

validate_clusterprofile_access_only() {
  local profile_json

  profile_json="$(resolve_clusterprofile_json)"
  if jq -e '
    any(.status.accessProviders[]?; (.name | test("aws|sts|irsa"; "i")))
  ' <<<"$profile_json" >/dev/null; then
    log "ClusterProfile contains an AWS-looking access provider; remote IRSA credentials must not be published through ClusterProfile accessProviders"
    jq '.status.accessProviders' <<<"$profile_json" >&2 || true
    return 1
  fi
  log "ClusterProfile accessProviders remain Kubernetes-access only"
}

verify_tokenrequest_authorization() {
  local denied_err

  [[ -n "$REMOTE_ACCESS_SUBJECT" ]] || die "remote access subject was not resolved"

  managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$APP_NAMESPACE" create token "$APP_SERVICE_ACCOUNT" \
    --audience=sts.amazonaws.com \
    --duration=10m >/dev/null

  managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$APP_NAMESPACE" create token "$MISMATCH_SERVICE_ACCOUNT" \
    --audience=sts.amazonaws.com \
    --duration=10m >/dev/null

  managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$REMOTE_ACCESS_NAMESPACE" get serviceaccount "$MANAGED_SERVICE_ACCOUNT" >/dev/null

  managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$REMOTE_ACCESS_NAMESPACE" create token "$MANAGED_SERVICE_ACCOUNT" \
    --audience=sts.amazonaws.com \
    --duration=10m >/dev/null

  denied_err="${WORK_DIR}/remote-tokenrequest-denied.err"
  if managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$APP_NAMESPACE" create token "$DENIED_SERVICE_ACCOUNT" \
    --audience=sts.amazonaws.com \
    --duration=10m >/dev/null 2>"$denied_err"; then
    log "remote TokenRequest unexpectedly allowed for ${APP_NAMESPACE}/${DENIED_SERVICE_ACCOUNT}"
    return 1
  fi
  if ! grep -Eiq 'forbidden|cannot create|serviceaccounts/token' "$denied_err"; then
    log "remote TokenRequest denied check failed with an unexpected error"
    sed 's/^/[e2e] tokenrequest: /' "$denied_err" >&2 || true
    return 1
  fi

  log "remote TokenRequest RBAC verified for ${REMOTE_ACCESS_SUBJECT}"
}

run_remote_irsa_helper() {
  local service_account="$1"
  local output_file="$2"
  local job_name phase start

  job_name="remote-irsa-${service_account}"
  hub_kubectl -n "$AWIO_NAMESPACE" delete job "$job_name" --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1 || true

  cat <<EOF | hub_kubectl -n "$AWIO_NAMESPACE" apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: ${AWIO_FULLNAME}
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: helper
          image: ${REMOTE_HELPER_IMAGE}
          imagePullPolicy: IfNotPresent
          args:
            - --namespace
            - ${WLC_NAMESPACE}
            - --service-account
            - ${APP_NAMESPACE}/${service_account}
            - --aws-service-account-role
            - ${ROLE_RESOURCE_NAME}
            - --session-name
            - awio-e2e-${RUN_ID}-${service_account}
            - --clusterprofile-provider-file
            - /clusterprofile-provider-file.json
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
            runAsGroup: 65532
            runAsNonRoot: true
            runAsUser: 65532
EOF

  start="$(date +%s)"
  while true; do
    phase="$(hub_kubectl -n "$AWIO_NAMESPACE" get pod -l "job-name=${job_name}" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    if [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]]; then
      break
    fi

    if (( $(date +%s) - start > 180 )); then
      log "remote IRSA helper job ${job_name} did not finish"
      hub_kubectl -n "$AWIO_NAMESPACE" describe job "$job_name" >&2 || true
      hub_kubectl -n "$AWIO_NAMESPACE" describe pod -l "job-name=${job_name}" >&2 || true
      return 1
    fi

    sleep 2
  done

  hub_kubectl -n "$AWIO_NAMESPACE" logs "job/${job_name}" >"$output_file" 2>/dev/null || true
  if [[ "$phase" == "Succeeded" ]]; then
    return 0
  fi

  cat "$output_file" >&2 || true
  return 1
}

validate_credential_process_json() {
  local credentials_file="$1"

  jq -e '
    .Version == 1
    and (.AccessKeyId | type == "string" and length > 0)
    and (.SecretAccessKey | type == "string" and length > 0)
    and (.SessionToken | type == "string" and length > 0)
    and (.Expiration | type == "string" and length > 0)
  ' "$credentials_file" >/dev/null

  log "credential_process JSON shape verified"
}

create_aws_irsa_sidecar_managed_kubeconfig_secret() {
  local ca_data kubeconfig_file server token token_secret

  token_secret="$(hub_kubectl -n "$WLC_NAMESPACE" get managedserviceaccount "$MANAGED_SERVICE_ACCOUNT" -o jsonpath='{.status.tokenSecretRef.name}' 2>/dev/null || true)"
  [[ -n "$token_secret" ]] || die "ManagedServiceAccount ${WLC_NAMESPACE}/${MANAGED_SERVICE_ACCOUNT} did not report tokenSecretRef.name"

  token="$(hub_kubectl -n "$WLC_NAMESPACE" get secret "$token_secret" -o jsonpath='{.data.token}' | base64 -d)"
  server="$(kubectl --kubeconfig "$MANAGED_KUBECONFIG" config view --raw -o jsonpath='{.clusters[0].cluster.server}')"
  ca_data="$(kubectl --kubeconfig "$MANAGED_KUBECONFIG" config view --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
  [[ -n "$token" ]] || die "ManagedServiceAccount token secret ${WLC_NAMESPACE}/${token_secret} did not include token data"
  [[ -n "$server" ]] || die "managed kubeconfig did not include a cluster server"
  [[ -n "$ca_data" ]] || die "managed kubeconfig did not include certificate-authority-data"

  kubeconfig_file="${WORK_DIR}/aws-irsa-sidecar-kubeconfig"
  cat >"$kubeconfig_file" <<EOF
apiVersion: v1
kind: Config
clusters:
  - name: managed
    cluster:
      server: ${server}
      certificate-authority-data: ${ca_data}
users:
  - name: ${MANAGED_SERVICE_ACCOUNT}
    user:
      token: ${token}
contexts:
  - name: managed
    context:
      cluster: managed
      user: ${MANAGED_SERVICE_ACCOUNT}
current-context: managed
EOF

  hub_kubectl -n "$AWIO_NAMESPACE" create secret generic "$IRSA_SIDECAR_MANAGED_KUBECONFIG_SECRET_NAME" \
    --from-file=kubeconfig="$kubeconfig_file" \
    --dry-run=client -o yaml | hub_kubectl apply -f -
  log "ManagedKubeConfigSecret ${AWIO_NAMESPACE}/${IRSA_SIDECAR_MANAGED_KUBECONFIG_SECRET_NAME} created for IRSA sidecar"
}

verify_aws_irsa_sidecar() {
  local caller_json_file job_name phase role_name rotation_after start

  job_name="$AWS_IRSA_SIDECAR_JOB_NAME"
  caller_json_file="${WORK_DIR}/aws-irsa-sidecar-caller.json"
  role_name="${TOKEN_FILE_ROLE_ARN##*/}"

  log "verifying IRSA sidecar"
  hub_kubectl -n "$AWIO_NAMESPACE" delete job "$job_name" --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1 || true

  cat <<EOF | hub_kubectl -n "$AWIO_NAMESPACE" apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  labels:
    app.kubernetes.io/component: aws-irsa-sidecar-e2e
spec:
  activeDeadlineSeconds: 480
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/component: aws-irsa-sidecar-e2e
    spec:
      restartPolicy: Never
      serviceAccountName: ${AWIO_FULLNAME}
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
            secretName: ${IRSA_SIDECAR_MANAGED_KUBECONFIG_SECRET_NAME}
      initContainers:
        - name: aws-irsa-sidecar
          image: ${REMOTE_HELPER_IMAGE}
          imagePullPolicy: IfNotPresent
          restartPolicy: Always
          command:
            - /aws-irsa-sidecar
          args:
            - --kubeconfig
            - /managed/config/kubeconfig
            - --token-file
            - /var/run/aws-irsa/token
            - --aws-config-file
            - /var/run/aws-irsa/config
          startupProbe:
            exec:
              command:
                - /aws-irsa-sidecar
                - check
                - --token-file
                - /var/run/aws-irsa/token
                - --aws-config-file
                - /var/run/aws-irsa/config
            failureThreshold: 90
            periodSeconds: 2
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
        - name: aws
          image: ${AWS_CLI_IMAGE}
          imagePullPolicy: IfNotPresent
          env:
            - name: AWS_CONFIG_FILE
              value: /var/run/aws-irsa/config
            - name: AWS_SDK_LOAD_CONFIG
              value: "1"
            - name: AWS_REGION
              value: ${AWS_REGION}
            - name: AWS_DEFAULT_REGION
              value: ${AWS_REGION}
            - name: HOME
              value: /tmp
          command:
            - /bin/sh
            - -c
          args:
            - |
              set -eu
              token_file="/var/run/aws-irsa/token"
              first_token="\$(cat "\$token_file")"
              start_ts="\$(date +%s)"
              test -n "\$first_token"
              if ! aws sts get-caller-identity --output json >/tmp/first-caller.json 2>/tmp/first-sts.err; then
                cat /tmp/first-sts.err >&2
                exit 1
              fi
              deadline=\$(( \$(date +%s) + 300 ))
              current_token="\$first_token"
              while [ "\$(date +%s)" -lt "\$deadline" ]; do
                sleep 5
                current_token="\$(cat "\$token_file")"
                if [ "\$current_token" != "\$first_token" ]; then
                  rotation_after=\$(( \$(date +%s) - start_ts ))
                  if ! aws sts get-caller-identity --output json >/tmp/second-caller.json 2>/tmp/second-sts.err; then
                    cat /tmp/second-sts.err >&2
                    exit 1
                  fi
                  stable_token="\$current_token"
                  observation_seconds=20
                  observation_deadline=\$(( \$(date +%s) + observation_seconds ))
                  while [ "\$(date +%s)" -lt "\$observation_deadline" ]; do
                    sleep 5
                    current_token="\$(cat "\$token_file")"
                    if [ "\$current_token" != "\$stable_token" ]; then
                      echo "IRSA sidecar token file rotated more than once inside the post-rotation observation window" >&2
                      exit 1
                    fi
                  done
                  printf '{"caller":'
                  cat /tmp/second-caller.json
                  printf ',"rotationCount":1,"rotationAfterSeconds":%s,"postRotationObservationSeconds":%s}\n' "\$rotation_after" "\$observation_seconds"
                  exit 0
                fi
              done
              echo "IRSA sidecar token file did not rotate before timeout" >&2
              exit 1
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          volumeMounts:
            - name: aws-irsa-state
              mountPath: /var/run/aws-irsa
              readOnly: true
EOF

  start="$(date +%s)"
  while true; do
    phase="$(hub_kubectl -n "$AWIO_NAMESPACE" get pod -l "job-name=${job_name}" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
    if [[ "$phase" == "Succeeded" || "$phase" == "Failed" ]]; then
      break
    fi

    if (( $(date +%s) - start > 540 )); then
      log "IRSA sidecar job ${job_name} did not finish"
      hub_kubectl -n "$AWIO_NAMESPACE" describe job "$job_name" >&2 || true
      hub_kubectl -n "$AWIO_NAMESPACE" describe pod -l "job-name=${job_name}" >&2 || true
      hub_kubectl -n "$AWIO_NAMESPACE" logs -l "job-name=${job_name}" --all-containers --tail=300 >&2 || true
      return 1
    fi

    sleep 2
  done

  hub_kubectl -n "$AWIO_NAMESPACE" logs "job/${job_name}" -c aws >"$caller_json_file" 2>/dev/null || true
  if [[ "$phase" != "Succeeded" ]]; then
    log "IRSA sidecar job ${job_name} failed"
    hub_kubectl -n "$AWIO_NAMESPACE" describe job "$job_name" >&2 || true
    hub_kubectl -n "$AWIO_NAMESPACE" describe pod -l "job-name=${job_name}" >&2 || true
    hub_kubectl -n "$AWIO_NAMESPACE" logs -l "job-name=${job_name}" --all-containers --tail=300 >&2 || true
    return 1
  fi

  if ! jq -e --arg role_name "$role_name" '
    (.caller.Arn | contains(":assumed-role/" + $role_name + "/"))
    and (.caller.Account | length > 0)
    and (.caller.UserId | length > 0)
    and .rotationCount == 1
    and (.rotationAfterSeconds | type == "number" and . >= 30 and . <= 300)
    and .postRotationObservationSeconds == 20
  ' "$caller_json_file" >/dev/null; then
    log "IRSA sidecar caller identity or rotation evidence did not match expectations"
    cat "$caller_json_file" >&2 || true
    return 1
  fi

  rotation_after="$(jq -r '.rotationAfterSeconds' "$caller_json_file")"
  log "IRSA sidecar verified for generated role ${role_name}; token rotated after ${rotation_after}s without an extra immediate rotation"
}

verify_remote_irsa_consumer() {
  local caller_json credentials_file mismatch_token mismatched_err role_name sts_err start

  wait_for_json_condition_hub "$WLC_NAMESPACE" awsworkloadidentityconfig default Ready "$TIMEOUT_SECONDS"
  wait_for_json_condition_hub "$WLC_NAMESPACE" awsserviceaccountrole "$ROLE_RESOURCE_NAME" Ready "$TIMEOUT_SECONDS"
  wait_for_json_condition_hub "$WLC_NAMESPACE" awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" Ready "$TIMEOUT_SECONDS"
  wait_for_role_arn
  wait_for_token_file_role_arn
  wait_for_nonempty_hub_jsonpath "AWSServiceAccountRole generated policy ARN" "$TIMEOUT_SECONDS" \
    hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}'
  wait_for_nonempty_hub_jsonpath "token-file AWSServiceAccountRole generated policy ARN" "$TIMEOUT_SECONDS" \
    hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}'
  wait_for_nonempty_hub_jsonpath "AWSWorkloadIdentityConfig OIDC provider ARN" "$TIMEOUT_SECONDS" \
    hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.oidcProviderARN}'
  wait_for_nonempty_hub_jsonpath "AWSWorkloadIdentityConfig bucket name" "$TIMEOUT_SECONDS" \
    hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.selfHostedIssuer.bucketName}'

  BUCKET_NAME="$(hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.selfHostedIssuer.bucketName}')"
  GENERATED_POLICY_ARN="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}')"
  TOKEN_FILE_GENERATED_POLICY_ARN="$(hub_kubectl -n "$WLC_NAMESPACE" get awsserviceaccountrole "$TOKEN_FILE_ROLE_RESOURCE_NAME" -o jsonpath='{.status.generatedPolicyARN}')"
  OIDC_PROVIDER_ARN="$(hub_kubectl -n "$WLC_NAMESPACE" get awsworkloadidentityconfig default -o jsonpath='{.status.oidcProviderARN}')"
  [[ -n "$BUCKET_NAME" ]] || die "AWSWorkloadIdentityConfig.status.selfHostedIssuer.bucketName is required for cleanup verification"
  [[ -n "$GENERATED_POLICY_ARN" ]] || die "AWSServiceAccountRole.status.generatedPolicyARN is required for cleanup verification"
  [[ -n "$TOKEN_FILE_GENERATED_POLICY_ARN" ]] || die "token-file AWSServiceAccountRole.status.generatedPolicyARN is required for cleanup verification"
  [[ -n "$OIDC_PROVIDER_ARN" ]] || die "AWSWorkloadIdentityConfig.status.oidcProviderARN is required for cleanup verification"

  validate_clusterprofile_access_only
  verify_tokenrequest_authorization

  credentials_file="${WORK_DIR}/remote-irsa-creds.json"
  sts_err="${WORK_DIR}/remote-irsa-helper.err"
  start="$(date +%s)"
  while true; do
    if run_remote_irsa_helper "$APP_SERVICE_ACCOUNT" "$credentials_file" 2>"$sts_err"; then
      break
    fi

    if (( $(date +%s) - start > TIMEOUT_SECONDS )); then
      log "remote IRSA credential helper did not succeed"
      sed 's/^/[e2e] helper: /' "$sts_err" >&2 || true
      return 1
    fi

    log "waiting for STS to accept the new OIDC provider and trust policy"
    sleep 10
  done

  validate_credential_process_json "$credentials_file"

  role_name="${ROLE_ARN##*/}"
  caller_json="$(AWS_ACCESS_KEY_ID="$(jq -r .AccessKeyId "$credentials_file")" \
    AWS_SECRET_ACCESS_KEY="$(jq -r .SecretAccessKey "$credentials_file")" \
    AWS_SESSION_TOKEN="$(jq -r .SessionToken "$credentials_file")" \
    AWS_REGION="$AWS_REGION" \
    AWS_DEFAULT_REGION="$AWS_REGION" \
    aws sts get-caller-identity --output json)"

  jq -e --arg role_name "$role_name" '
    (.Arn | contains(":assumed-role/" + $role_name + "/"))
    and (.Account | length > 0)
    and (.UserId | length > 0)
  ' <<<"$caller_json" >/dev/null
  log "AWS STS caller identity verified for generated role ${role_name}"

  wait_for_managed_service_account_role_annotation "$TIMEOUT_SECONDS"
  create_aws_irsa_sidecar_managed_kubeconfig_secret
  verify_aws_irsa_sidecar

  mismatch_token="${WORK_DIR}/mismatch-web-identity-token"
  managed_kubectl --as "$REMOTE_ACCESS_SUBJECT" -n "$APP_NAMESPACE" create token "$MISMATCH_SERVICE_ACCOUNT" \
    --audience=sts.amazonaws.com \
    --duration=15m >"$mismatch_token"
  mismatched_err="${WORK_DIR}/remote-irsa-mismatch.err"
  if aws sts assume-role-with-web-identity \
    --role-arn "$ROLE_ARN" \
    --role-session-name "awio-e2e-${RUN_ID}-${MISMATCH_SERVICE_ACCOUNT}" \
    --web-identity-token "file://${mismatch_token}" \
    --duration-seconds 900 >/dev/null 2>"$mismatched_err"; then
    log "STS unexpectedly allowed ServiceAccount outside the IAM trust policy"
    return 1
  fi
  if ! grep -Eiq 'AccessDenied|access denied|Not authorized|not authorized|InvalidIdentityToken|assume' "$mismatched_err"; then
    log "mismatched ServiceAccount negative check did not fail with an expected STS denial"
    sed 's/^/[e2e] mismatch: /' "$mismatched_err" >&2 || true
    return 1
  fi
  log "negative STS trust-policy check verified"
}

main() {
  preflight
  parse_operator_image
  load_aws_credentials
  build_remote_irsa_helper
  install_cp_creds_plugin
  write_helper_provider_file

  BUCKET_NAME="$(bucket_name "$WLC_NAMESPACE" "$AWS_REGION")"
  issuer_url="https://${BUCKET_NAME}.s3.${AWS_REGION}.amazonaws.com"

  log "run id: ${RUN_ID}"
  log "AWS region: ${AWS_REGION}"
  log "workload cluster namespace/name: ${WLC_NAMESPACE}"
  log "issuer URL: ${issuer_url}"

  prepare_hub_cluster
  build_and_load_remote_irsa_helper_image
  generate_service_account_keys
  install_hub_cert_manager
  install_cluster_inventory_crds
  install_ack_controllers
  install_ocm
  bind_operator_clusterprofile_namespace
  install_capd
  create_capd_workload_cluster "$issuer_url"
  register_managed_cluster_with_ocm
  enable_ocm_addons_for_cluster
  prepare_workload_application
  build_and_install_operator
  grant_consumer_tokenrequest_permissions
  publish_remote_cluster_properties
  wait_for_managed_service_account_access
  wait_for_operator_rollout
  apply_workload_identity_resources
  verify_remote_irsa_consumer

  log "remote IRSA consumer e2e completed"
}

main "$@"
