#!/usr/bin/env bash
set -euo pipefail

chart_file="charts/aws-workload-identity-operator/Chart.yaml"
values_file="charts/aws-workload-identity-operator/values.yaml"
chart_readme_file="charts/aws-workload-identity-operator/README.md"
root_readme_file="README.md"

version="$(sed -nE 's/^version:[[:space:]]*"?([^"]+)"?$/\1/p' "${chart_file}" | head -n1)"
if [[ -z "${version}" ]]; then
  echo "failed to read chart version from ${chart_file}" >&2
  exit 1
fi

sed -Ei "s/^appVersion:.*/appVersion: \"${version}\"/" "${chart_file}"
sed -Ei "/^image:/,/^[^[:space:]]/ s/^([[:space:]]+tag: ).*$/\\1\"${version}\"/" "${values_file}"
sed -Ei '/^image:/,/^```$/ s/^([[:space:]]+tag: ).*$/\1"'"${version}"'"/' "${chart_readme_file}"
sed -Ei "s/^([[:space:]]+--version )[[:alnum:].+_-]+([[:space:]]*\\\\)$/\\1${version}\\2/" "${root_readme_file}" "${chart_readme_file}"
