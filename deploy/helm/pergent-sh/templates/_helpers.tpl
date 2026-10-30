{{- define "pergent-sh.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pergent-sh.fullname" -}}
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

{{- define "pergent-sh.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "pergent-sh.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "pergent-sh.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pergent-sh.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pergent-sh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pergent-sh.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Settings the chart fixes. Failing here beats a service that starts and cannot
find its skills, or whose Jobs land where the Role does not reach.
*/}}
{{- define "pergent-sh.validate" -}}
{{- if not .Values.existingSecret -}}
{{- fail "existingSecret is required: the name of the Secret holding DATABASE_URL, PERGENT_SH_SECRET_KEY and the other env vars" -}}
{{- end -}}
{{- if not .Values.jobsNamespace -}}
{{- fail "jobsNamespace is required" -}}
{{- end -}}
{{- $jobs := default dict .Values.config.jobs -}}
{{- if ne (toString $jobs.namespace) .Values.jobsNamespace -}}
{{- fail (printf "config.jobs.namespace (%q) must equal jobsNamespace (%q): the Role and NetworkPolicy are created there" (toString $jobs.namespace) .Values.jobsNamespace) -}}
{{- end -}}
{{- if ne (toString .Values.config.skills_dir) "/etc/pergent-sh/skills" -}}
{{- fail "config.skills_dir must be /etc/pergent-sh/skills: that is where the chart mounts the skills" -}}
{{- end -}}
{{- if not (hasSuffix ":8080" (toString .Values.config.listen)) -}}
{{- fail "config.listen must end in :8080: the container port, Service and probes use 8080" -}}
{{- end -}}
{{- end -}}
