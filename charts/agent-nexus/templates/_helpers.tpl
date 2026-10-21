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
The runners' names and labels. Their app.kubernetes.io/name differs from
the controller's, so they never join the controller's Services (whose
selector, like the StatefulSet's, cannot change).
*/}}
{{- define "agent-nexus.runnerName" -}}
{{- printf "%s-runner" (include "agent-nexus.fullname" . | trunc 56 | trimSuffix "-") }}
{{- end }}

{{- define "agent-nexus.runnerSelectorLabels" -}}
app.kubernetes.io/name: {{ printf "%s-runner" (include "agent-nexus.name" . | trunc 56 | trimSuffix "-") }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "agent-nexus.runnerLabels" -}}
helm.sh/chart: {{ include "agent-nexus.chart" . }}
{{ include "agent-nexus.runnerSelectorLabels" . }}
app.kubernetes.io/component: runner
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
The environment of a process running jobs, controller or runner: the
models, forges, Zulip bot, MCP servers and job settings. The controller
adds its own listeners, store and secrets.
*/}}
{{- define "agent-nexus.agentEnv" -}}
- name: METRICS_ADDR
  value: ":9090"
- name: LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
- name: REPO_CACHE_DIR
  value: /data/repos
- name: CHART_CACHE_DIR
  value: /data/charts
- name: ALLOWED_OWNERS
  value: {{ join "," .Values.allowedOwners | quote }}
{{- with .Values.review.modes }}
- name: REVIEW_MODES
  value: {{ include "agent-nexus.reviewModes" $ | quote }}
{{- end }}
- name: AGENT_MAX_TURNS
  value: {{ .Values.agentMaxTurns | quote }}
{{- with .Values.fetch.allowedHosts }}
- name: FETCH_ALLOWED_HOSTS
  value: {{ join "," . | quote }}
{{- end }}
{{- if .Values.models }}
- name: MODELS_CONFIG
  value: /etc/agent-nexus/models/config.yaml
{{- range .Values.models }}
{{- if .apiKeyKey }}
{{- include "agent-nexus.secretEnv" (dict "name" (include "agent-nexus.modelKeyEnv" .name) "secret" $.Values.existingSecret "key" .apiKeyKey) | nindent 0 }}
{{- end }}
{{- end }}
{{- else }}
- name: LLM_BASE_URL
  value: {{ .Values.llm.baseURL | quote }}
- name: LLM_MODEL
  value: {{ .Values.llm.model | quote }}
{{- if .Values.llm.contextTokens }}
- name: LLM_CONTEXT_TOKENS
  value: {{ .Values.llm.contextTokens | quote }}
{{- end }}
{{- if .Values.llm.thinkingBudget }}
- name: LLM_THINKING_BUDGET
  value: {{ .Values.llm.thinkingBudget | quote }}
{{- end }}
- name: LLM_CONCURRENCY
  value: {{ .Values.llm.concurrency | quote }}
- name: LLM_REQUEST_TIMEOUT
  value: {{ .Values.llm.requestTimeout | quote }}
{{- include "agent-nexus.secretEnv" (dict "name" "LLM_API_KEY" "secret" .Values.existingSecret "key" "llm-api-key" "optional" true) | nindent 0 }}
{{- end }}
{{- if .Values.gitea.enabled }}
- name: GITEA_URL
  value: {{ .Values.gitea.url | quote }}
- name: GITEA_USER
  value: {{ .Values.gitea.user | quote }}
{{- include "agent-nexus.secretEnv" (dict "name" "GITEA_TOKEN" "secret" .Values.existingSecret "key" "gitea-token") | nindent 0 }}
{{- end }}
{{- if .Values.github.enabled }}
- name: GITHUB_APP_ID
  value: {{ .Values.github.appID | quote }}
- name: GITHUB_API_URL
  value: {{ .Values.github.apiURL | quote }}
- name: GITHUB_SERVER_URL
  value: {{ .Values.github.serverURL | quote }}
{{- include "agent-nexus.secretEnv" (dict "name" "GITHUB_APP_PRIVATE_KEY" "secret" .Values.existingSecret "key" "github-app-private-key") | nindent 0 }}
{{- end }}
{{- if .Values.zulip.enabled }}
- name: ZULIP_URL
  value: {{ .Values.zulip.url | quote }}
- name: ZULIP_BOT_EMAIL
  value: {{ .Values.zulip.botEmail | quote }}
{{- include "agent-nexus.secretEnv" (dict "name" "ZULIP_BOT_API_KEY" "secret" .Values.existingSecret "key" "zulip-bot-api-key") | nindent 0 }}
{{- end }}
{{- if .Values.mcp.servers }}
- name: MCP_CONFIG
  value: /etc/agent-nexus/mcp/config.yaml
{{- range .Values.mcp.servers }}
{{- if .tokenKey }}
{{- include "agent-nexus.secretEnv" (dict "name" (include "agent-nexus.mcpTokenEnv" .name) "secret" $.Values.existingSecret "key" .tokenKey) | nindent 0 }}
{{- end }}
{{- end }}
{{- end }}
{{- with .Values.extraEnv }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}

{{/*
Fail early on settings agent-nexus would refuse at startup.
*/}}
{{- define "agent-nexus.validate" -}}
{{- if not .Values.existingSecret }}
{{- fail "existingSecret is required: a Secret with at least the api-keys key" }}
{{- end }}
{{- if .Values.models }}
{{- if or .Values.llm.baseURL .Values.llm.model }}
{{- fail "set either models or llm.baseURL and llm.model, not both" }}
{{- end }}
{{- else if not (and .Values.llm.baseURL .Values.llm.model) }}
{{- fail "llm.baseURL and llm.model are required, unless models lists the models" }}
{{- end }}
{{- if and .Values.runners.replicas (not .Values.runners.enabled) }}
{{- fail "runners.replicas needs runners.enabled: the controller only hands jobs to runners then" }}
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
{{- if and .Values.zulip.enabled (not (and .Values.zulip.url .Values.zulip.botEmail)) }}
{{- fail "zulip.url and zulip.botEmail are required when zulip.enabled" }}
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

{{/*
mcp.servers as the MCP_CONFIG file. A server with a tokenKey reads its
bearer token from MCP_TOKEN_<NAME>, set from that key of existingSecret.
*/}}
{{- define "agent-nexus.mcpConfig" -}}
{{- $servers := list }}
{{- range .Values.mcp.servers }}
{{- if not (and .name .url .tools) }}
{{- fail (printf "mcp.servers entries need a name, a url and tools, got %v" .) }}
{{- end }}
{{- if or (not (regexMatch "^[a-z][a-z0-9]{0,15}$" .name)) (eq .name "mcp") }}
{{- fail (printf "mcp.servers name %q: want a lowercase letter, then up to 15 lowercase letters or digits, other than mcp" .name) }}
{{- end }}
{{- if and .load (not (has .load (list "lazy" "eager"))) }}
{{- fail (printf "mcp.servers load %q: want lazy or eager" .load) }}
{{- end }}
{{- $server := dict "name" .name "url" .url "tools" .tools }}
{{- with .description }}{{ $_ := set $server "description" . }}{{ end }}
{{- with .jobs }}{{ $_ := set $server "jobs" . }}{{ end }}
{{- with .load }}{{ $_ := set $server "load" . }}{{ end }}
{{- with .timeout }}{{ $_ := set $server "timeout" . }}{{ end }}
{{- if .tokenKey }}{{ $_ := set $server "tokenEnv" (include "agent-nexus.mcpTokenEnv" .name) }}{{ end }}
{{- $servers = append $servers $server }}
{{- end }}
{{- toYaml (dict "servers" $servers) }}
{{- end }}

{{- define "agent-nexus.mcpTokenEnv" -}}
MCP_TOKEN_{{ upper . }}
{{- end }}

{{/*
models as the MODELS_CONFIG file. A model with an apiKeyKey reads its API
key from LLM_API_KEY_<NAME>, set from that key of existingSecret.
*/}}
{{- define "agent-nexus.modelsConfig" -}}
{{- $models := list }}
{{- range .Values.models }}
{{- if not (and .name (regexMatch "^[a-z][a-z0-9]{0,15}$" .name)) }}
{{- fail (printf "models name %v: want a lowercase letter, then up to 15 lowercase letters or digits" .name) }}
{{- end }}
{{- $model := dict "name" .name }}
{{- with .baseURL }}{{ $_ := set $model "baseURL" . }}{{ end }}
{{- with .model }}{{ $_ := set $model "model" . }}{{ end }}
{{- with .concurrency }}{{ $_ := set $model "concurrency" . }}{{ end }}
{{- with .jobs }}{{ $_ := set $model "jobs" . }}{{ end }}
{{- if hasKey . "local" }}{{ $_ := set $model "local" .local }}{{ end }}
{{- with .contextTokens }}{{ $_ := set $model "contextTokens" . }}{{ end }}
{{- with .thinkingBudget }}{{ $_ := set $model "thinkingBudget" . }}{{ end }}
{{- with .requestTimeout }}{{ $_ := set $model "requestTimeout" . }}{{ end }}
{{- if .apiKeyKey }}{{ $_ := set $model "apiKeyEnv" (include "agent-nexus.modelKeyEnv" .name) }}{{ end }}
{{- $models = append $models $model }}
{{- end }}
{{- toYaml (dict "models" $models) }}
{{- end }}

{{- define "agent-nexus.modelKeyEnv" -}}
LLM_API_KEY_{{ upper . }}
{{- end }}
