{{/*
Expand the name of the chart.
*/}}
{{- define "tatara-claude-code-wrapper.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "tatara-claude-code-wrapper.fullname" -}}
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
{{- define "tatara-claude-code-wrapper.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "tatara-claude-code-wrapper.labels" -}}
helm.sh/chart: {{ include "tatara-claude-code-wrapper.chart" . }}
{{ include "tatara-claude-code-wrapper.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "tatara-claude-code-wrapper.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tatara-claude-code-wrapper.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "tatara-claude-code-wrapper.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "tatara-claude-code-wrapper.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Map camelCase values.* scalars to UPPER_SNAKE ConfigMap keys.
Strict: values.yaml carries only scalars; this macro is the single mapping point.
File-shaped data (lists, multiline, maps) goes to configmap-files.yaml, never here.
*/}}
{{- define "tatara-claude-code-wrapper.envConfig" -}}
HTTP_ADDR: {{ .Values.httpAddr | quote }}
INTERNAL_ADDR: {{ .Values.internalAddr | quote }}
OIDC_ISSUER: {{ .Values.oidcIssuer | quote }}
OIDC_AUDIENCE: {{ .Values.oidcAudience | quote }}
LOG_LEVEL: {{ .Values.logLevel | quote }}
MODEL: {{ .Values.model | quote }}
PERMISSION_MODE: {{ .Values.permissionMode | quote }}
REPO_URL: {{ .Values.repoUrl | quote }}
REPO_BRANCH: {{ .Values.repoBranch | quote }}
DEFAULT_CALLBACK_URL: {{ .Values.defaultCallbackUrl | quote }}
TURN_TIMEOUT_SECONDS: {{ .Values.turnTimeoutSeconds | quote }}
AGENT_POD_TTL_SECONDS: {{ .Values.agentPodTTLSeconds | quote }}
BOOT_TIMEOUT_SECONDS: {{ .Values.bootTimeoutSeconds | quote }}
WEBHOOK_RETRIES: {{ .Values.webhookRetries | quote }}
GLOBAL_CLAUDE_MD_PATH: "/etc/wrapper/global-claude.md"
PROJECT_CLAUDE_MD_PATH: "/etc/wrapper/project-claude.md"
MCP_BASE_PATH: "/etc/wrapper/mcp-base.json"
MCP_OVERLAY_DIR: "/etc/wrapper/mcp.d"
{{- /* skillsCloneDir takes filepath.Dir of the FIRST entry as the boot-clone
target, so this must keep the parent-of-skills shape, must match
cmd/wrapper/config.go's defaultSkillsSrcDirs, and the derived parent
(/etc/wrapper/skills) must be a WRITABLE mount - it sits inside the read-only
`files` ConfigMap projection, so deployment.yaml nests an emptyDir there.
Both halves are asserted by cmd/wrapper/guard_chart_skills_dir_test.go; this
value has already been wrong twice in opposite directions without failing CI. */}}
SKILLS_SRC_DIRS: "/etc/wrapper/skills/skills"
ALLOWED_TOOLS_PATH: "/etc/wrapper/allowed-tools.txt"
{{- end -}}
