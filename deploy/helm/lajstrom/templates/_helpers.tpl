{{/*
The chart's name, and the full name its objects take: the release's name when it already says
lajstrom, <release>-lajstrom otherwise. At most 50 characters, so that the suffixes of the
objects (-bootstrap, -migrate) keep within the 63 of a label value.
*/}}
{{- define "lajstrom.name" -}}
{{- .Values.nameOverride | default .Chart.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "lajstrom.fullname" -}}
{{- $name := include "lajstrom.name" . -}}
{{- $full := .Values.fullnameOverride -}}
{{- if not $full -}}
{{- $full = ternary .Release.Name (printf "%s-%s" .Release.Name $name) (contains $name .Release.Name) -}}
{{- end -}}
{{- $full | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{/* The labels of every object. */}}
{{- define "lajstrom.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "lajstrom.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "lajstrom.tag" . | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: lajstrom
{{- end -}}

{{/*
What the Deployment, the Service and the PodDisruptionBudget select: the replicas of serve, and
not the pod of the migration Job, whose component is migrate.
*/}}
{{- define "lajstrom.selectorLabels" -}}
app.kubernetes.io/name: {{ include "lajstrom.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "lajstrom.tag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end -}}

{{/* The image, with its digest when one is set. */}}
{{- define "lajstrom.image" -}}
{{- $ref := printf "%s:%s" .Values.image.repository (include "lajstrom.tag" .) -}}
{{- if .Values.image.digest -}}
{{- $ref = printf "%s@%s" $ref .Values.image.digest -}}
{{- end -}}
{{- $ref -}}
{{- end -}}

{{- define "lajstrom.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "lajstrom.fullname" .) -}}
{{- else -}}
{{- .Values.serviceAccount.name | default "default" -}}
{{- end -}}
{{- end -}}

{{/* The Secret of a value that names one: its existingSecret, or the release's full name. */}}
{{- define "lajstrom.secretName" -}}
{{- .ref.existingSecret | default (include "lajstrom.fullname" .root) -}}
{{- end -}}

{{- define "lajstrom.bootstrapSecretName" -}}
{{- .Values.bootstrap.existingSecret | default (printf "%s-bootstrap" (include "lajstrom.fullname" .)) -}}
{{- end -}}

{{/* Where the Secret of bootstrap is mounted, and the file auth.bootstrap.tokenFile names. */}}
{{- define "lajstrom.bootstrapDir" -}}/run/secrets/lajstrom-bootstrap{{- end -}}

{{/*
lajstrom.yaml: config, with the keys the chart wires set over it. Refuses what the chart cannot
run: another database than Postgres, the master key as a file, a passwordEnv no value sets.
*/}}
{{- define "lajstrom.config" -}}
{{- $config := deepCopy (.Values.config | default dict) -}}
{{- $database := $config.database | default dict -}}
{{- if and $database.driver (ne $database.driver "postgres") -}}
{{- fail (printf "config.database.driver is %q: the chart runs Postgres only, which every replica shares; SQLite serves one replica on a volume (docs/deploy/helm.md)" $database.driver) -}}
{{- end -}}
{{- if dig "keys" "masterKeyFile" "" $config -}}
{{- fail "config.keys.masterKeyFile: the chart passes the master key as the variable LAJSTROM_MASTER_KEY from masterKey.existingSecret; remove the file" -}}
{{- end -}}
{{- $set := dict -}}
{{- range .Values.registryPasswords -}}
{{- $_ := set $set (required "registryPasswords[].env is required" .env) true -}}
{{- end -}}
{{- range .Values.extraEnv -}}
{{- $_ := set $set .name true -}}
{{- end -}}
{{- range $i, $r := $config.registries | default list -}}
{{- $env := dig "auth" "passwordEnv" "" $r -}}
{{- if and $env (not (hasKey $set $env)) -}}
{{- fail (printf "config.registries[%d].auth.passwordEnv names %s, which no entry of registryPasswords (or extraEnv) sets: add {env: %s, existingSecret: <Secret>, key: <key>}" $i $env $env) -}}
{{- end -}}
{{- end -}}
{{- $wired := dict "server" (dict "listen" ":8080") "database" (dict "driver" "postgres") "keys" (dict "masterKeyEnv" "LAJSTROM_MASTER_KEY") -}}
{{- if .Values.metrics.enabled -}}
{{- $_ := set $wired.server "metricsListen" (printf "0.0.0.0:%d" (int .Values.metrics.port)) -}}
{{- end -}}
{{- if .Values.bootstrap.enabled -}}
{{- $_ := set $wired "auth" (dict "bootstrap" (dict "tokenFile" (printf "%s/token" (include "lajstrom.bootstrapDir" .)))) -}}
{{- end -}}
{{- toYaml (mustMergeOverwrite $config $wired) -}}
{{- end -}}

{{/*
The variables of the secrets, each from its Secret: the master key, the DSN, the registry
passwords. The Deployment and the migration Job both take them; dsn is the Job's own DSN.
*/}}
{{- define "lajstrom.secretEnv" -}}
{{- $root := .root -}}
- name: LAJSTROM_MASTER_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "lajstrom.secretName" (dict "root" $root "ref" $root.Values.masterKey) }}
      key: {{ $root.Values.masterKey.key }}
- name: LAJSTROM_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ include "lajstrom.secretName" (dict "root" $root "ref" .dsn) }}
      key: {{ .dsn.key }}
{{- range $root.Values.registryPasswords }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "lajstrom.secretName" (dict "root" $root "ref" .) }}
      key: {{ required "registryPasswords[].key is required" .key }}
{{- end }}
{{- end -}}
