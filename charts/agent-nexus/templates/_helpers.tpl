{{/*
Expand the name of the chart.
*/}}
{{- define "agent-nexus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name, truncated to 63 characters.
*/}}
{{- define "agent-nexus.fullname" -}}
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
Name of the headless Service governing the StatefulSet. The base is cut to
54 characters so the suffix always fits the 63-character limit: cutting the
whole name instead could drop the suffix and collide with the main Service.
*/}}
{{- define "agent-nexus.headlessName" -}}
{{- printf "%s-headless" (include "agent-nexus.fullname" . | trunc 54 | trimSuffix "-") }}
{{- end }}

{{- define "agent-nexus.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "agent-nexus.labels" -}}
helm.sh/chart: {{ include "agent-nexus.chart" . }}
{{ include "agent-nexus.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "agent-nexus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-nexus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Fail early on settings agent-nexus would refuse at startup.
*/}}
{{- define "agent-nexus.validate" -}}
{{- if not .Values.existingSecret }}
{{- fail "existingSecret is required: a Secret with at least the api-keys key" }}
{{- end }}
{{- if not (and .Values.llm.baseURL .Values.llm.model) }}
{{- fail "llm.baseURL and llm.model are required" }}
{{- end }}
{{- if not .Values.allowedOwners }}
{{- fail "allowedOwners must list at least one repository owner" }}
{{- end }}
{{- if not (or .Values.gitea.enabled .Values.github.enabled) }}
{{- fail "enable at least one forge: gitea.enabled or github.enabled" }}
{{- end }}
{{- if and .Values.gitea.enabled (not (and .Values.gitea.url .Values.gitea.user)) }}
{{- fail "gitea.url and gitea.user are required when gitea.enabled" }}
{{- end }}
{{- if and .Values.github.enabled (not .Values.github.appID) }}
{{- fail "github.appID is required when github.enabled" }}
{{- end }}
{{- end }}

{{/*
review.modes as REVIEW_MODES: "repo=mode,repo=mode", in order.
*/}}
{{- define "agent-nexus.reviewModes" -}}
{{- $rules := list }}
{{- range .Values.review.modes }}
{{- if not (and .repo (has .mode (list "comment" "verdict"))) }}
{{- fail (printf "review.modes entries need a repo and a mode of comment or verdict, got %v" .) }}
{{- end }}
{{- $rules = append $rules (printf "%s=%s" .repo .mode) }}
{{- end }}
{{- join "," $rules }}
{{- end }}

{{- define "agent-nexus.secretEnv" -}}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ .secret }}
      key: {{ .key }}
      {{- if .optional }}
      optional: true
      {{- end }}
{{- end }}
