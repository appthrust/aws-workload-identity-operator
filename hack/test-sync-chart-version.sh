#!/usr/bin/env bash
set -euo pipefail

# Exercise hack/sync-chart-version.sh end-to-end with a simulated version bump
# to a dotted-semver pre-release identifier. The production --check target only
# proves idempotency at the *current* chart version, so prose patterns that
# happen to match the current version by accident would never be exercised.
# Here the mirrored tree is bumped to a synthetic version, the script is run
# against the mirror, and each documented prose anchor is asserted post-bump.

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
new_version="9.9.9-rc.99"

tmp_root="$(mktemp -d)"
cleanup() { rm -rf "${tmp_root}"; }
trap cleanup EXIT

mirror_files=(
  charts/aws-workload-identity-operator/Chart.yaml
  charts/aws-workload-identity-operator/values.yaml
  charts/aws-workload-identity-operator/README.md
  docs/guides/install-helm.md
  docs/reference/compatibility.md
  docs/guides/remote-irsa-consumers.md
  docs/guides/ocm-addon-framework-token-file-sidecar.md
)

for f in "${mirror_files[@]}"; do
  mkdir -p "${tmp_root}/$(dirname "${f}")"
  cp "${repo_root}/${f}" "${tmp_root}/${f}"
done

current_version="$(sed -nE 's/^version:[[:space:]]*"?([^"]+)"?$/\1/p' \
  "${tmp_root}/charts/aws-workload-identity-operator/Chart.yaml" | head -n1)"
if [[ -z "${current_version}" ]]; then
  echo "test setup: failed to read current chart version" >&2
  exit 1
fi
if [[ "${current_version}" == "${new_version}" ]]; then
  echo "test setup: synthetic version collides with current chart version" >&2
  exit 1
fi

perl -i -0pe "s/^version:.*\$/version: ${new_version}/m" \
  "${tmp_root}/charts/aws-workload-identity-operator/Chart.yaml"

( cd "${tmp_root}" && bash "${repo_root}/hack/sync-chart-version.sh" )

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# file_contains_literal slurps the file as a single string and tests for a
# literal multi-line needle. `grep -F` is unsuitable here: a multi-line search
# string is treated as an OR of independent line patterns, which silently
# weakens both positive and negative assertions on prose anchors that span
# a markdown reflow boundary.
file_contains_literal() {
  local file="$1" needle="$2"
  perl -0777 -e '
    my $needle = shift;
    my $path = shift;
    open my $fh, "<", $path or die "open $path: $!";
    my $content = do { local $/; <$fh> };
    exit($content =~ /\Q$needle\E/ ? 0 : 1);
  ' -- "${needle}" "${file}"
}

assert_contains() {
  local file="$1" needle="$2"
  if ! file_contains_literal "${tmp_root}/${file}" "${needle}"; then
    fail "${file} missing post-bump string: ${needle}"
  fi
}

assert_not_contains() {
  local file="$1" needle="$2"
  if file_contains_literal "${tmp_root}/${file}" "${needle}"; then
    fail "${file} still contains pre-bump string: ${needle}"
  fi
}

# Chart.yaml: appVersion must follow version.
assert_contains "charts/aws-workload-identity-operator/Chart.yaml" \
  "version: ${new_version}"
assert_contains "charts/aws-workload-identity-operator/Chart.yaml" \
  "appVersion: \"${new_version}\""

# values.yaml: image.tag must follow version.
assert_contains "charts/aws-workload-identity-operator/values.yaml" \
  "tag: \"${new_version}\""

# Chart README: every prose pattern the renderer claims to cover.
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "--version ${new_version} \\"
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "  tag: \"${new_version}\""
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "Chart version \`${new_version}\`"
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "\`v${new_version}\`"
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "default
\`${new_version}\` tag"
assert_contains "charts/aws-workload-identity-operator/README.md" \
  "\`ghcr.io/appthrust/aws-workload-identity-operator:${new_version}\`"

# Install guide: --version flag, chart-version-is sentence, bare v-tag.
assert_contains "docs/guides/install-helm.md" \
  "--version ${new_version} \\"
assert_contains "docs/guides/install-helm.md" \
  "Helm chart version is \`${new_version}\`"
assert_contains "docs/guides/install-helm.md" \
  "\`v${new_version}\`"

# Compatibility doc: bare v-tag, --version table cell, three ghcr image refs.
assert_contains "docs/reference/compatibility.md" \
  "\`v${new_version}\`"
assert_contains "docs/reference/compatibility.md" \
  "--version ${new_version}"
assert_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator:${new_version}"
assert_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:${new_version}"
assert_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${new_version}"
# The "chart version is" sentence is split across lines by markdown reflow.
assert_contains "docs/reference/compatibility.md" \
  "chart version
is \`${new_version}\`"

# Remote-IRSA consumer guide: ghcr tags + backticked tool tag + bare v-tag.
assert_contains "docs/guides/remote-irsa-consumers.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${new_version}"
assert_contains "docs/guides/remote-irsa-consumers.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:${new_version}"
assert_contains "docs/guides/remote-irsa-consumers.md" \
  "\`aws-irsa-sidecar:${new_version}\`"
assert_contains "docs/guides/remote-irsa-consumers.md" \
  "\`remote-irsa-tools:${new_version}\`"
assert_contains "docs/guides/remote-irsa-consumers.md" \
  "\`v${new_version}\`"

# OCM sidecar guide: ghcr tag + backticked tool tag + bare v-tag.
assert_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${new_version}"
assert_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "\`aws-irsa-sidecar:${new_version}\`"
assert_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "\`v${new_version}\`"

# Negative coverage: each managed prose anchor must no longer carry the
# pre-bump version. The checks mirror the positive assertions above and so are
# scoped to the renderer's domain: backticked versions, `--version` flags,
# `tag:` image-stanza lines, and AWIO image URLs. Bare X.Y.Z mentions in
# unrelated contexts - third-party image tags such as
# `public.ecr.aws/karpenter/controller:vX.Y.Z`, or platform requirements such
# as `Kubernetes >=X.Y` - are intentionally not searched, so a future chart
# version that happens to collide with such a reference does not produce a
# spurious failure.

# Chart.yaml: appVersion + version.
assert_not_contains "charts/aws-workload-identity-operator/Chart.yaml" \
  "version: ${current_version}"
assert_not_contains "charts/aws-workload-identity-operator/Chart.yaml" \
  "appVersion: \"${current_version}\""

# values.yaml: image.tag.
assert_not_contains "charts/aws-workload-identity-operator/values.yaml" \
  "tag: \"${current_version}\""

# Chart README: every prose anchor the renderer claims to cover.
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "--version ${current_version} \\"
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "  tag: \"${current_version}\""
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "Chart version \`${current_version}\`"
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "\`v${current_version}\`"
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "default
\`${current_version}\` tag"
assert_not_contains "charts/aws-workload-identity-operator/README.md" \
  "\`ghcr.io/appthrust/aws-workload-identity-operator:${current_version}\`"

# Install guide: --version flag, chart-version-is sentence, bare v-tag.
assert_not_contains "docs/guides/install-helm.md" \
  "--version ${current_version} \\"
assert_not_contains "docs/guides/install-helm.md" \
  "Helm chart version is \`${current_version}\`"
assert_not_contains "docs/guides/install-helm.md" \
  "\`v${current_version}\`"

# Compatibility doc: bare v-tag, --version, three ghcr image refs, reflow.
assert_not_contains "docs/reference/compatibility.md" \
  "\`v${current_version}\`"
assert_not_contains "docs/reference/compatibility.md" \
  "--version ${current_version}"
assert_not_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator:${current_version}"
assert_not_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:${current_version}"
assert_not_contains "docs/reference/compatibility.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${current_version}"
assert_not_contains "docs/reference/compatibility.md" \
  "chart version
is \`${current_version}\`"

# Remote-IRSA consumer guide: ghcr tags + backticked tool tag + bare v-tag.
assert_not_contains "docs/guides/remote-irsa-consumers.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${current_version}"
assert_not_contains "docs/guides/remote-irsa-consumers.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:${current_version}"
assert_not_contains "docs/guides/remote-irsa-consumers.md" \
  "\`aws-irsa-sidecar:${current_version}\`"
assert_not_contains "docs/guides/remote-irsa-consumers.md" \
  "\`remote-irsa-tools:${current_version}\`"
assert_not_contains "docs/guides/remote-irsa-consumers.md" \
  "\`v${current_version}\`"

# OCM sidecar guide: ghcr tag + backticked tool tag + bare v-tag.
assert_not_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:${current_version}"
assert_not_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "\`aws-irsa-sidecar:${current_version}\`"
assert_not_contains "docs/guides/ocm-addon-framework-token-file-sidecar.md" \
  "\`v${current_version}\`"

# Idempotency: re-running --check on the bumped tree must report success.
if ! ( cd "${tmp_root}" && bash "${repo_root}/hack/sync-chart-version.sh" --check ); then
  fail "--check is not idempotent on the bumped tree"
fi

if (( failures > 0 )); then
  echo "test-sync-chart-version: ${failures} assertion(s) failed" >&2
  exit 1
fi

echo "test-sync-chart-version: ${#mirror_files[@]} mirrored files OK at ${new_version}"
