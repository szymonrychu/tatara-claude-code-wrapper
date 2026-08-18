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
# Deliberately NOT cmd/wrapper/config.go's default (/etc/wrapper/skills/skills):
# skillsCloneDir is filepath.Dir of the first entry, and this chart mounts the
# `files` ConfigMap AT /etc/wrapper, which kubelet mounts read-only - so both the
# clone dir and its .staging sibling would be unwritable. Agent pods keep the Go
# default and are unaffected: the operator sets no SKILLS_SRC_DIRS and mounts
# nothing over /etc/wrapper, which the image creates and chowns to uid 10001.
#
# HOME is the writable root here. Getting this wrong is a crashloop now that
# Render fails on zero installed skills, and two successive values were wrong
# with nothing to catch them ("/templates/skills", whose target died with the
# baked skills on 2026-06-28, then the Go default above), hence the render
# assertion in .github/ci/check-chart-skills-clone-dir.sh.
SKILLS_SRC_DIRS: "/home/agent/skills/skills"
ALLOWED_TOOLS_PATH: "/etc/wrapper/allowed-tools.txt"
{{- end -}}
