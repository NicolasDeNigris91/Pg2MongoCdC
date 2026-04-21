{{/*
Common labels and selector helpers. Kept tiny on purpose — this chart has
three near-identical workloads, so a small _helpers.tpl avoids template
duplication without becoming a meta-language of its own.
*/}}

{{- define "pg2mongo-cdc.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pg2mongo-cdc.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pg2mongo-cdc.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels — applied to every resource. Component-specific labels are
added inline at the resource level.
*/}}
{{- define "pg2mongo-cdc.labels" -}}
helm.sh/chart: {{ include "pg2mongo-cdc.chart" . }}
app.kubernetes.io/name: {{ include "pg2mongo-cdc.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: pg2mongo-cdc
{{- end -}}

{{/*
Per-component selector labels. Pass the component name as the second arg.
Usage: include "pg2mongo-cdc.selectorLabels" (list . "transformer")
*/}}
{{- define "pg2mongo-cdc.selectorLabels" -}}
{{- $ctx := index . 0 -}}
{{- $component := index . 1 -}}
app.kubernetes.io/name: {{ include "pg2mongo-cdc.name" $ctx }}
app.kubernetes.io/instance: {{ $ctx.Release.Name }}
app.kubernetes.io/component: {{ $component }}
{{- end -}}

{{/*
ServiceAccount name — generated unless explicitly overridden.
*/}}
{{- define "pg2mongo-cdc.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pg2mongo-cdc.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Image tag resolution. If image.tag is set globally, use it; otherwise fall
back to .Chart.AppVersion.
*/}}
{{- define "pg2mongo-cdc.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag -}}
{{- end -}}
