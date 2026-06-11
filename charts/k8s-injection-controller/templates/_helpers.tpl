{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-injection-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "k8s-injection-controller.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "k8s-injection-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Namespace the controller and its namespaced resources are installed into.
Honors the explicit `namespace.name` value, falling back to the Helm release
namespace when it is left empty.
*/}}
{{- define "k8s-injection-controller.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name }}
{{- end }}

{{/*
Namespace whose ConfigMaps the controller watches. Defaults to the install
namespace when `watchNamespace` is empty.
*/}}
{{- define "k8s-injection-controller.watchNamespace" -}}
{{- default (include "k8s-injection-controller.namespace" .) .Values.watchNamespace }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "k8s-injection-controller.labels" -}}
helm.sh/chart: {{ include "k8s-injection-controller.chart" . }}
{{ include "k8s-injection-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels. These also carry `control-plane: controller-manager`, which
the webhook/metrics Services and NetworkPolicies select on (kept from the
upstream kustomize manifests).
*/}}
{{- define "k8s-injection-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-injection-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "k8s-injection-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "k8s-injection-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Webhook Service name (and its in-cluster DNS address used for WEBHOOK_SERVICE_ADDR).
*/}}
{{- define "k8s-injection-controller.webhookServiceName" -}}
{{- printf "%s-webhook" (include "k8s-injection-controller.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name of the cert-manager Certificate / serving cert Secret for the webhook.
*/}}
{{- define "k8s-injection-controller.servingCertName" -}}
{{- printf "%s-serving-cert" (include "k8s-injection-controller.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Resolve the effective webhook cert mode to either "cert-manager" or
"self-signed". `auto` (the default) picks cert-manager when the cluster
exposes the cert-manager.io/v1 API, otherwise self-signed. An explicit
`cert-manager` always resolves to cert-manager: when the API is absent the
chart's pre-install hook installs cert-manager first (see installCertManager)
rather than failing. Any other value is rejected.
*/}}
{{- define "k8s-injection-controller.certMode" -}}
{{- $mode := .Values.webhook.certManager.mode | default "auto" -}}
{{- $hasCM := .Capabilities.APIVersions.Has "cert-manager.io/v1" -}}
{{- if eq $mode "auto" -}}
{{- /* auto never installs cert-manager: use it only if already present. */ -}}
{{- if $hasCM }}cert-manager{{ else }}self-signed{{ end -}}
{{- else if eq $mode "cert-manager" -}}
{{- /* Forced cert-manager. If the API is absent the pre-install hook installs
       it (see installCertManager), so we no longer fail here. */ -}}
cert-manager
{{- else if eq $mode "self-signed" -}}
self-signed
{{- else -}}
{{- fail (printf "invalid webhook.certManager.mode %q: must be auto, cert-manager, or self-signed" $mode) -}}
{{- end -}}
{{- end -}}

{{/*
Whether the chart should install cert-manager itself via the pre-install hook.
Renders "true" (non-empty) only when the user FORCED cert-manager mode and the
cert-manager.io/v1 API is absent — auto mode never installs (it falls back to
self-signed). An explicit certManager.installHook.enabled=true also forces it on.
When this is true the Issuer/Certificate are emitted as post-install hooks so
they are validated only after the installer hook has registered the CRDs.
*/}}
{{- define "k8s-injection-controller.installCertManager" -}}
{{- $mode := .Values.webhook.certManager.mode | default "auto" -}}
{{- $hasCM := .Capabilities.APIVersions.Has "cert-manager.io/v1" -}}
{{- if or .Values.certManager.installHook.enabled (and (eq $mode "cert-manager") (not $hasCM)) -}}
true
{{- end -}}
{{- end -}}
