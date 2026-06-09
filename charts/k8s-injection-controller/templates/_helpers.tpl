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
