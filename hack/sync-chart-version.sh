#!/usr/bin/env bash
set -euo pipefail

check=false
case "${1:-}" in
  "")
    ;;
  --check)
    check=true
    ;;
  -h|--help)
    cat <<'USAGE'
Usage: hack/sync-chart-version.sh [--check]

Synchronize versioned chart, docs, and helper-image snippets from
charts/aws-workload-identity-operator/Chart.yaml.

  --check   report stale files without rewriting them
USAGE
    exit 0
    ;;
  *)
    echo "unknown argument: $1" >&2
    exit 2
    ;;
esac

chart_file="charts/aws-workload-identity-operator/Chart.yaml"
values_file="charts/aws-workload-identity-operator/values.yaml"
chart_readme_file="charts/aws-workload-identity-operator/README.md"
install_guide_file="docs/guides/install-helm.md"
compatibility_file="docs/reference/compatibility.md"
remote_irsa_consumers_file="docs/guides/remote-irsa-consumers.md"
ocm_sidecar_file="docs/guides/ocm-addon-framework-token-file-sidecar.md"

version="$(sed -nE 's/^version:[[:space:]]*"?([^"]+)"?$/\1/p' "${chart_file}" | head -n1)"
if [[ -z "${version}" ]]; then
  echo "failed to read chart version from ${chart_file}" >&2
  exit 1
fi

render_file() {
  local file="$1"

  case "${file}" in
    "${chart_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's/^appVersion:.*/appVersion: "$ENV{AWIO_VERSION}"/m' \
        "${file}"
      ;;
    "${values_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's/(^image:\n(?:^[ \t]+.*\n)*?^[ \t]+tag: ).*/$1"$ENV{AWIO_VERSION}"/m' \
        "${file}"
      ;;
    "${chart_readme_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's/(--version )\S+([ \t]*\\)/$1$ENV{AWIO_VERSION}$2/g;
         s/(^image:\n(?:^[ \t]+.*\n)*?^[ \t]+tag: ).*/$1"$ENV{AWIO_VERSION}"/m' \
        "${file}"
      ;;
    "${install_guide_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's/(--version )\S+([ \t]*\\)/$1$ENV{AWIO_VERSION}$2/g;
         s/`v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?`/`v$ENV{AWIO_VERSION}`/g;
         s/chart version is `[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?`/chart version is `$ENV{AWIO_VERSION}`/g' \
        "${file}"
      ;;
    "${compatibility_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's/`v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?` is the first public release line/`v$ENV{AWIO_VERSION}` is the first public release line/g;
         s/chart version is `[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?`/chart version is `$ENV{AWIO_VERSION}`/g;
         s/release tag is `v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?`/release tag is `v$ENV{AWIO_VERSION}`/g;
         s/`v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?` reference/`v$ENV{AWIO_VERSION}` reference/g;
         s/(--version )[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?/$1$ENV{AWIO_VERSION}/g;
         s#(ghcr\.io/appthrust/aws-workload-identity-operator:)[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?#$1$ENV{AWIO_VERSION}#g;
         s#(ghcr\.io/appthrust/aws-workload-identity-operator/(?:remote-irsa-tools|aws-irsa-sidecar):)v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?#$1v$ENV{AWIO_VERSION}#g' \
        "${file}"
      ;;
    "${remote_irsa_consumers_file}"|"${ocm_sidecar_file}")
      AWIO_VERSION="${version}" perl -0pe \
        's#(ghcr\.io/appthrust/aws-workload-identity-operator/(?:remote-irsa-tools|aws-irsa-sidecar):)v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9._-]+)?#$1v$ENV{AWIO_VERSION}#g' \
        "${file}"
      ;;
    *)
      echo "internal error: no renderer for ${file}" >&2
      exit 1
      ;;
  esac
}

status=0
for file in \
  "${chart_file}" \
  "${values_file}" \
  "${chart_readme_file}" \
  "${install_guide_file}" \
  "${compatibility_file}" \
  "${remote_irsa_consumers_file}" \
  "${ocm_sidecar_file}"
do
  tmp="$(mktemp)"
  render_file "${file}" >"${tmp}"

  if "${check}"; then
    if ! cmp -s "${file}" "${tmp}"; then
      echo "${file} is not synchronized with chart version ${version}" >&2
      diff -u "${file}" "${tmp}" || true
      status=1
    fi
    rm -f "${tmp}"
  else
    cat "${tmp}" >"${file}"
    rm -f "${tmp}"
  fi
done

exit "${status}"
