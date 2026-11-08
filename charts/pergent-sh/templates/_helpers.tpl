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

{{/*
The pergent.sh image: the Deployment's, and the one a skill fetch Job sends
its result back with.
*/}}
{{- define "pergent-sh.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "pergent-sh.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pergent-sh.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The config file. Jobs run in this cluster, so by default they post their results
to this chart's Service and need no public route. config.callback_url wins.
*/}}
{{- define "pergent-sh.config" -}}
{{- $config := deepCopy .Values.config -}}
{{- if not $config.callback_url -}}
{{- $_ := set $config "callback_url" (printf "http://%s.%s.svc:%d" (include "pergent-sh.fullname" .) .Release.Namespace (int .Values.service.port)) -}}
{{- end -}}
{{- if .Values.metrics.enabled -}}
{{- $_ := set $config "metrics_listen" (printf ":%d" (int .Values.metrics.port)) -}}
{{- end -}}
{{- toYaml $config -}}
{{- end -}}

{{/*
Settings the chart fixes. Failing here beats a service that starts and cannot
reach its Jobs, or whose Jobs land where the Role does not reach.
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
{{- if hasKey .Values "skills" -}}
{{- fail "skills was removed: skills live in the dashboard (see the README, \"Skills and instructions\")" -}}
{{- end -}}
{{- if not (hasSuffix ":8080" (toString .Values.config.listen)) -}}
{{- fail "config.listen must end in :8080: the container port, Service and probes use 8080" -}}
{{- end -}}
{{- if .Values.config.metrics_listen -}}
{{- fail "metrics_listen is set by the chart: use metrics.enabled and metrics.port" -}}
{{- end -}}
{{- if and .Values.metrics.enabled (eq (int .Values.metrics.port) 8080) -}}
{{- fail "metrics.port must not be 8080: that is the port of the main server" -}}
{{- end -}}
{{- $dashboard := default dict .Values.config.dashboard -}}
{{- if and .Values.adminPassword.existingSecret (not $dashboard.admin_password_env) -}}
{{- fail "adminPassword.existingSecret needs config.dashboard.admin_password_env: the name of the env var the password is passed as" -}}
{{- end -}}
{{- if or (hasKey .Values "ingress") (hasKey .Values "dashboardIngress") -}}
{{- fail "ingress and dashboardIngress were removed: the chart creates only a Service; route to it yourself (see the README, \"Exposing pergent.sh\")" -}}
{{- end -}}
{{- end -}}
