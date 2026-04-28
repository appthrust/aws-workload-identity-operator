{{/*
Expand the name of the chart.
*/}}
{{- define "awio.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "awio.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version.
*/}}
{{- define "awio.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Release namespace, honoring namespaceOverride.
*/}}
{{- define "awio.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "awio.labels" -}}
helm.sh/chart: {{ include "awio.chart" . }}
{{ include "awio.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "awio.selectorLabels" -}}
app.kubernetes.io/name: {{ include "awio.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Service account name.
*/}}
{{- define "awio.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "awio.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
ClusterProfile provider file ConfigMap name.
*/}}
{{- define "awio.clusterInventoryProviderConfigMapName" -}}
{{- printf "%s-clusterprofile-provider-file" (include "awio.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Webhook service name.
*/}}
{{- define "awio.webhookServiceName" -}}
{{- printf "%s-webhook" (include "awio.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Webhook TLS secret name.
*/}}
{{- define "awio.webhookTLSSecretName" -}}
{{- printf "%s-webhook-tls" (include "awio.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Render an image reference from chart image values.
*/}}
{{- define "awio.image" -}}
{{- $registry := default .image.registry .global.imageRegistry -}}
{{- $repository := .image.repository -}}
{{- $tag := default .chart.AppVersion .image.tag | toString -}}
{{- if .image.digest -}}
{{- if $registry -}}
{{- printf "%s/%s@%s" $registry $repository .image.digest -}}
{{- else -}}
{{- printf "%s@%s" $repository .image.digest -}}
{{- end -}}
{{- else -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repository $tag -}}
{{- else -}}
{{- printf "%s:%s" $repository $tag -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "awio.managerImage" -}}
{{- include "awio.image" (dict "image" .Values.image "global" .Values.global "chart" .Chart) -}}
{{- end -}}

{{/*
Image pull secrets for the manager image.
*/}}
{{- define "awio.imagePullSecrets" -}}
{{- $global := default (list) .Values.global.imagePullSecrets -}}
{{- $local := default (list) .Values.image.pullSecrets -}}
{{- $secrets := concat $global $local -}}
{{- if $secrets }}
imagePullSecrets:
{{- range $secrets }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Render a map as comma-separated key=value pairs.
*/}}
{{- define "awio.keyValueCSV" -}}
{{- $items := list -}}
{{- range $key, $value := . -}}
{{- if and $key (ne (toString $value) "") -}}
{{- $items = append $items (printf "%s=%s" $key (toString $value)) -}}
{{- end -}}
{{- end -}}
{{- join "," $items -}}
{{- end -}}

{{/*
OpenTelemetry resource attributes for logging.
*/}}
{{- define "awio.loggingResourceAttributes" -}}
{{- $resource := .Values.logging.resource | default dict -}}
{{- $attrs := dict -}}
{{- with $resource.serviceName -}}
{{- $_ := set $attrs "service.name" . -}}
{{- end -}}
{{- with $resource.serviceNamespace -}}
{{- $_ := set $attrs "service.namespace" . -}}
{{- end -}}
{{- $serviceVersion := default .Chart.AppVersion $resource.serviceVersion -}}
{{- with $serviceVersion -}}
{{- $_ := set $attrs "service.version" . -}}
{{- end -}}
{{- with $resource.deploymentEnvironmentName -}}
{{- $_ := set $attrs "deployment.environment.name" . -}}
{{- end -}}
{{- range $key, $value := ($resource.attributes | default dict) -}}
{{- $_ := set $attrs $key $value -}}
{{- end -}}
{{- include "awio.keyValueCSV" $attrs -}}
{{- end -}}

{{/*
OpenTelemetry OTLP headers for logging exporters.
*/}}
{{- define "awio.loggingOTLPHeaders" -}}
{{- include "awio.keyValueCSV" (.Values.logging.otlp.headers | default dict) -}}
{{- end -}}

{{- define "awio.loggingOTLPLogsHeaders" -}}
{{- include "awio.keyValueCSV" (.Values.logging.otlp.logsHeaders | default dict) -}}
{{- end -}}

{{/*
Name of the OCM ManagedServiceAccount used by chart-created OCM resources.
*/}}
{{- define "awio.ocmManagedServiceAccountName" -}}
{{- .Values.ocm.managedServiceAccount.name -}}
{{- end -}}

{{/*
Name used for chart-created remote permission objects.
*/}}
{{- define "awio.ocmRemotePermissionsName" -}}
{{- $remotePermissions := .Values.ocm.managedServiceAccount.remotePermissions | default dict -}}
{{- $name := get $remotePermissions "name" | default "" -}}
{{- if $name -}}
{{- $name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-ocm-remote-permissions" (include "awio.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Remote webhook namespace used by chart-created remote permissions.
*/}}
{{- define "awio.ocmRemotePermissionsWebhookNamespace" -}}
{{- $remotePermissions := .Values.ocm.managedServiceAccount.remotePermissions | default dict -}}
{{- if $remotePermissions.webhookNamespace -}}
{{- $remotePermissions.webhookNamespace -}}
{{- else if .Values.operatorConfig.create -}}
{{- $operatorSpec := .Values.operatorConfig.spec | default dict -}}
{{- $selfHostedIRSA := get $operatorSpec "selfHostedIRSA" | default dict -}}
{{- default "aws-pod-identity-webhook" (get $selfHostedIRSA "webhookNamespace") -}}
{{- else -}}
{{- "aws-pod-identity-webhook" -}}
{{- end -}}
{{- end -}}

{{/*
Labels recognized by the remote self-hosted webhook runtime controller.
*/}}
{{- define "awio.webhookRuntimeLabels" -}}
app.kubernetes.io/name: pod-identity-webhook
app.kubernetes.io/managed-by: aws-workload-identity-operator
aws.identity.appthrust.io/runtime: self-hosted-webhook
{{- end -}}

{{/*
Canonical JSON for the final Cluster Inventory access-provider config.
*/}}
{{- define "awio.clusterInventoryProviderConfigJSON" -}}
{{- $inventory := .Values.clusterInventory | default dict -}}
{{- $providers := list -}}
{{- $args := list (printf "--managed-serviceaccount=%s" .Values.ocm.managedServiceAccount.name) -}}
{{- $execConfig := dict "apiVersion" "client.authentication.k8s.io/v1" "command" "/plugins/cp-creds" "args" $args "provideClusterInfo" true "interactiveMode" "Never" -}}
{{- $providers = append $providers (dict "name" "open-cluster-management" "execConfig" $execConfig) -}}
{{- $providers = concat $providers ($inventory.accessProviders | default (list)) -}}
{{- $config := dict "providers" $providers -}}
{{- if eq (len $providers) 0 -}}
{{-   fail "final Cluster Inventory access-provider config must contain at least one provider" -}}
{{- end -}}
{{- $mountPaths := list -}}
{{- range ($inventory.plugins | default (list)) -}}
{{-   $mountPaths = append $mountPaths .mountPath -}}
{{- end -}}
{{- $seenProviderNames := dict -}}
{{- range $index, $provider := $providers -}}
{{-   $name := get $provider "name" | default "" -}}
{{-   if eq $name "" -}}
{{-     fail (printf "provider at index %d: name is required" $index) -}}
{{-   end -}}
{{-   if hasKey $seenProviderNames $name -}}
{{-     fail (printf "duplicate Cluster Inventory access-provider name %q" $name) -}}
{{-   end -}}
{{-   $_ := set $seenProviderNames $name true -}}
{{-   $execConfig := get $provider "execConfig" | default dict -}}
{{-   $cmd := get $execConfig "command" | default "" -}}
{{-   if eq $cmd "" -}}
{{-     fail (printf "provider %q: execConfig.command is required" $name) -}}
{{-   end -}}
{{-   $matchesMountPath := false -}}
{{-   range $mountPath := $mountPaths -}}
{{-     $normalizedMountPath := trimSuffix "/" $mountPath -}}
{{-     if or (eq $cmd $normalizedMountPath) (hasPrefix (printf "%s/" $normalizedMountPath) $cmd) -}}
{{-       $matchesMountPath = true -}}
{{-     end -}}
{{-   end -}}
{{-   if not $matchesMountPath -}}
{{-     fail (printf "provider %q: command %q does not live under any clusterInventory.plugins[].mountPath %v" $name $cmd $mountPaths) -}}
{{-   end -}}
{{- end -}}
{{- $config | mustToJson -}}
{{- end -}}

{{/*
Webhook DNS names used by generated certificates.
*/}}
{{- define "awio.webhookDNSNames" -}}
{{- $name := include "awio.webhookServiceName" . -}}
{{- $namespace := include "awio.namespace" . -}}
{{- list $name (printf "%s.%s" $name $namespace) (printf "%s.%s.svc" $name $namespace) (printf "%s.%s.svc.%s" $name $namespace .Values.clusterDomain) | toJson -}}
{{- end -}}
