{{/* Chart name. */}}
{{- define "open-pages.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified resource name. */}}
{{- define "open-pages.fullname" -}}
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

{{- define "open-pages.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "open-pages.selectorLabels" -}}
app.kubernetes.io/name: {{ include "open-pages.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "open-pages.labels" -}}
helm.sh/chart: {{ include "open-pages.chart" . }}
{{ include "open-pages.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "open-pages.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "open-pages.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Image reference: a digest wins over a tag; global.imageRegistry wins over image.registry. */}}
{{- define "open-pages.image" -}}
{{- $registry := default .Values.image.registry (.Values.global).imageRegistry -}}
{{- $repo := .Values.image.repository -}}
{{- if $registry }}{{ $repo = printf "%s/%s" (trimSuffix "/" $registry) $repo }}{{ end -}}
{{- if .Values.image.digest -}}
{{ $repo }}@{{ .Values.image.digest }}
{{- else -}}
{{ $repo }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}
{{- end }}

{{/* Required hosts, with a clear message. */}}
{{- define "open-pages.appHost" -}}
{{- required "config.appHost is required: set the host name of the API and UI, for example --set config.appHost=pages.corp" .Values.config.appHost -}}
{{- end }}

{{- define "open-pages.contentDomain" -}}
{{- required "config.contentDomain is required: set the domain that serves sites, for example --set config.contentDomain=pages-content.corp" .Values.config.contentDomain -}}
{{- end }}

{{/* Refuse unsupported settings. Included once from the Deployment. */}}
{{- define "open-pages.validate" -}}
{{- if ne (int .Values.replicaCount) 1 }}
{{- fail "replicaCount must be 1: open-pages stores its state in SQLite on a ReadWriteOnce volume and does not support more than one replica" }}
{{- end }}
{{- $app := include "open-pages.appHost" . }}
{{- $content := include "open-pages.contentDomain" . }}
{{- $min := add (int .Values.config.drainDelay) (int .Values.config.shutdownTimeout) }}
{{- if le (int .Values.terminationGracePeriodSeconds) $min }}
{{- fail (printf "terminationGracePeriodSeconds (%d) must be greater than config.drainDelay + config.shutdownTimeout (%d)" (int .Values.terminationGracePeriodSeconds) $min) }}
{{- end }}
{{- if and .Values.persistence.enabled (not .Values.persistence.existingClaim) (not (has "ReadWriteOnce" .Values.persistence.accessModes)) (not (has "ReadWriteOncePod" .Values.persistence.accessModes)) }}
{{- fail "persistence.accessModes must contain ReadWriteOnce (or ReadWriteOncePod): SQLite must not be on shared storage" }}
{{- end }}
{{- if and .Values.config.oidc.enabled (not (and .Values.config.oidc.issuer .Values.config.oidc.clientId .Values.config.oidc.redirectUrl)) }}
{{- fail "config.oidc.enabled needs config.oidc.issuer, clientId and redirectUrl" }}
{{- end }}
{{- if and .Values.config.oidc.enabled (not (include "open-pages.hasSecret" (dict "ctx" . "key" "oidcClientSecret"))) }}
{{- fail "config.oidc.enabled needs secrets.oidcClientSecret or secrets.existingSecret" }}
{{- end }}
{{- if and .Values.config.saml.enabled (not (and .Values.config.saml.idpMetadataUrl .Values.config.saml.entityId .Values.config.saml.acsUrl)) }}
{{- fail "config.saml.enabled needs config.saml.idpMetadataUrl, entityId and acsUrl" }}
{{- end }}
{{- if and .Values.config.saml.enabled .Values.config.saml.signRequests (not (and (include "open-pages.hasSecret" (dict "ctx" . "key" "samlKey")) (include "open-pages.hasSecret" (dict "ctx" . "key" "samlCert")))) }}
{{- fail "config.saml.signRequests needs secrets.samlKey and secrets.samlCert, or secrets.existingSecret" }}
{{- end }}
{{- if and .Values.config.metrics.tokenFromSecret (not (include "open-pages.hasSecret" (dict "ctx" . "key" "metricsToken"))) }}
{{- fail "config.metrics.tokenFromSecret needs secrets.metricsToken or secrets.existingSecret" }}
{{- end }}
{{- if and .Values.serviceMonitor.enabled (not .Values.config.metrics.listen) }}
{{- fail "serviceMonitor.enabled needs config.metrics.listen" }}
{{- end }}
{{- if and .Values.httpRoute.enabled (not .Values.httpRoute.parentRefs) }}
{{- fail "httpRoute.enabled needs httpRoute.parentRefs" }}
{{- end }}
{{- end }}

{{/* "true" when the named secret value is available (existing Secret or a value). */}}
{{- define "open-pages.hasSecret" -}}
{{- $v := index .ctx.Values.secrets .key -}}
{{- if or .ctx.Values.secrets.existingSecret $v }}true{{ end -}}
{{- end }}

{{/* True when the chart creates the Secret. */}}
{{- define "open-pages.createSecret" -}}
{{- if and (not .Values.secrets.existingSecret) (or .Values.secrets.oidcClientSecret .Values.secrets.metricsToken .Values.secrets.samlKey .Values.secrets.samlCert) }}true{{ end -}}
{{- end }}

{{- define "open-pages.secretName" -}}
{{- default (printf "%s-secrets" (include "open-pages.fullname" .)) .Values.secrets.existingSecret }}
{{- end }}

{{- define "open-pages.claimName" -}}
{{- default (include "open-pages.fullname" .) .Values.persistence.existingClaim }}
{{- end }}

{{/* Port number of config.metrics.listen (the part after the last colon). */}}
{{- define "open-pages.metricsPort" -}}
{{- last (splitList ":" .Values.config.metrics.listen) -}}
{{- end }}

{{/* Directory of the staging volume and of the secret files. */}}
{{- define "open-pages.stagingPath" -}}/staging{{- end }}
{{- define "open-pages.secretsPath" -}}/etc/open-pages/secrets{{- end }}

{{/* Whether any secret file is delivered to the container. */}}
{{- define "open-pages.usesSecret" -}}
{{- $c := .Values.config -}}
{{- if or $c.oidc.enabled $c.metrics.tokenFromSecret (and $c.saml.enabled $c.saml.signRequests) }}true{{ end -}}
{{- end }}
