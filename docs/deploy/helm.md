# Install with Helm

The chart in [deploy/helm/lajstrom/](../../deploy/helm/lajstrom/) renders the deployment of
[kubernetes.md](kubernetes.md) from values: two replicas behind one Service, the probes, resources
and restricted security context of the manifests, the configuration as a ConfigMap, and every
secret from a Secret you make. It adds what platform teams install with: a migration Job before
each upgrade, an optional HTTPRoute or Ingress, a ServiceMonitor, a HorizontalPodAutoscaler,
values for scheduling, and a Job that recovers a lost password. The chart is not published yet (#82): install it from the clone.

What it assumes is what the manifests assume ([kubernetes.md](kubernetes.md#what-the-manifests-assume)):
a Postgres that every replica shares, a registry with a user for Lajstrom, and an edge that
terminates TLS and routes the whole host to the port `http` without buffering. The chart ships
no Postgres: use a managed one or an operator's ([databases.md](databases.md)). It runs Postgres
only, since SQLite serves one replica on a volume; `helm template` refuses
`config.database.driver: sqlite`.

## 1. The Secrets

No secret goes into values. The defaults read the Secrets of kubernetes.md, so its commands
work unchanged:

```sh
kubectl create namespace lajstrom
kubectl -n lajstrom create secret generic lajstrom \
  --from-literal=LAJSTROM_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=LAJSTROM_DATABASE_DSN="postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=require" \
  --from-literal=LAJSTROM_REGISTRY_PASSWORD="$reg"
kubectl -n lajstrom create secret generic lajstrom-bootstrap \
  --from-literal=token="$(openssl rand -hex 32)"
```

The names are the release's full name (`lajstrom` for a release named `lajstrom`, otherwise
`<release>-lajstrom`) and `<full name>-bootstrap`. Each value can name another Secret and key,
such as the one your secret store writes, or CloudNativePG's for the DSN:

```yaml
masterKey: {existingSecret: lajstrom-master-key, key: LAJSTROM_MASTER_KEY}
database: {existingSecret: pg-app, key: uri}     # CloudNativePG's Secret <cluster>-app
registryPasswords:
  - {env: LAJSTROM_REGISTRY_PASSWORD, existingSecret: harbor-robot, key: secret}
```

The master key reaches the container as the variable `LAJSTROM_MASTER_KEY`, the DSN as
`LAJSTROM_DATABASE_DSN`, and each registry password as the variable its
`registries[].auth.passwordEnv` names; `helm template` fails on a `passwordEnv` that no entry of
`registryPasswords` sets. **Keep a copy of the master key outside the cluster**
([operations.md](../operations.md#the-master-key)).

## 2. The values

`config` is `lajstrom.yaml` ([configuration.md](../configuration.md)) as a map. The chart sets
`server.listen`, `server.metricsListen`, `database.driver`, `keys.masterKeyEnv` and
`auth.bootstrap.tokenFile` over it, whatever the map says. Helm merges maps and replaces lists, so
`config.registries` of your values replaces the default's whole.

```yaml
image:
  tag: v0.1.0
  digest: sha256:…          # pin the release by digest
config:
  server:
    externalURL: https://art.example.com
    trustedProxies: ["10.244.0.0/16"]   # the range the edge connects from
  registries:
    - name: main
      url: https://registry.example.com
      prefix: lajstrom/
      auth: {username: lajstrom, passwordEnv: LAJSTROM_REGISTRY_PASSWORD}
  repos:
    - {name: assets, format: raw, registry: main}
httpRoute:
  enabled: true
  parentRefs: [{name: lajstrom, sectionName: https}]
```

`server.trustedProxies` is found as in [kubernetes.md](kubernetes.md#2-the-configuration): while
it is empty the throttles of failed logins and of `/v2/token` are off, and NOTES.txt says so.

## 3. Install

```sh
helm install lajstrom ./deploy/helm/lajstrom -n lajstrom -f values.yaml --wait
```

The replicas start once the Secrets exist; without one, they wait in
`CreateContainerConfigError`. Each replica applies the migrations at start under the migration
lock, so on an empty database one applies them and the other finds them done. A change of the
configuration rolls the pods: the pod template carries a checksum of the file. A change of a
Secret does not, since the chart never reads one: `kubectl -n lajstrom rollout restart
deploy/lajstrom` after a new registry password or DSN.

## 4. The first admin

NOTES.txt prints the command. It runs `lajstrom admin bootstrap` on your machine against
`server.externalURL`, with the token read from the Secret; no `kubectl exec`:

```sh
lajstrom admin bootstrap --server https://art.example.com --name admin \
  --token-file <(kubectl -n lajstrom get secret lajstrom-bootstrap -o jsonpath='{.data.token}' | base64 -d)
kubectl -n lajstrom delete secret lajstrom-bootstrap
```

The volume of the Secret is optional, as in the manifests: once it is deleted, the file leaves the
pods. With `bootstrap.generate: true` the chart makes the Secret itself, with a random token that
it keeps across upgrades (`lookup`); drop it with `--set bootstrap.enabled=false` once the admin
exists. Tools that render with `helm template`, such as Argo CD, cannot look the Secret up and
would make a new token on every render: leave `generate` off there.

## A lost password

The database is the authority ([operations.md](../operations.md#a-lost-password)), and the chart
runs the recovery for you, without `kubectl exec`. `recovery.user` names the user. The chart
makes the Secret `<full name>-recovery` with a random password, and the Job
`<full name>-recovery` runs `lajstrom admin user set-password <user> --password-file` with it,
against the database, after the release's objects:

```sh
helm upgrade lajstrom ./deploy/helm/lajstrom -n lajstrom --reuse-values --set recovery.user=admin --wait
kubectl -n lajstrom get secret lajstrom-recovery -o jsonpath='{.data.password}' | base64 -d
```

Log in with that password, then turn the Job off. That upgrade removes the Secret:

```sh
helm upgrade lajstrom ./deploy/helm/lajstrom -n lajstrom --reuse-values --set recovery.user=
```

- **The old password fails** from the next request on, on every replica. The replicas do not
  restart. The audit log gets a `user.password` row by `anonymous`, with the source `cli` and
  the Job's pod as the `host`.
- **While `recovery.user` is set,** every `helm upgrade` runs the Job again with the same password,
  which the chart keeps across upgrades (`lookup`). A password the admin chose in the meantime
  would be replaced. So turn the Job off before you set a password of your own with
  `POST /api/v1/users/<user>/password` ([api.md](../api.md#password-post-apiv1usersuserpassword)).
- **Helm deletes the Job once it succeeds.** A Job that fails, for example for a user that does
  not exist, fails the upgrade (the release is `failed`, and the replicas serve on). It stays for
  its log, and turning the Job off does not remove it:

  ```text
  Error: UPGRADE FAILED: post-upgrade hooks failed: resource Job/lajstrom/lajstrom-recovery not ready. status: Failed, message: Job Failed. failed: 3/1
  $ kubectl -n lajstrom logs job/lajstrom-recovery
  lajstrom: user nobody does not exist
  $ kubectl -n lajstrom delete job lajstrom-recovery
  ```

- **A disabled user** gets the new password and stays disabled. `kubectl -n lajstrom exec
  deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin user enable <user>`
  enables it. When no admin is left at all, `admin user create --admin` the same way makes a new
  one ([operations.md](../operations.md#a-lost-password)).
- **A password of your own:** `recovery.existingSecret` and `recovery.key` name a Secret you
  made, and the chart then makes none. Delete that Secret yourself once you are in. Use it with
  tools that render with `helm template`, such as Argo CD: they cannot look the chart's Secret
  up, so they would make a new password on every render. Argo CD runs the Job as a `PostSync`
  hook.

The Job is a `post-install` hook as well. A `helm install` with `recovery.user` set, on a database
that already has the user (a reinstall), sets the password too. It runs as the chart's
ServiceAccount, with the DSN of `database` and without the master key, which `set-password` does
not read.

## Upgrades

`helm upgrade` runs `lajstrom migrate` of the new image first, as the Job `<full name>-migrate`
(a `pre-upgrade` hook), then rolls the replicas: the procedure of
[operations.md](../operations.md#several-replicas), which the Job of
[kubernetes.md](kubernetes.md#upgrade) runs by hand. Back up the database first, and read the
notes of every release you skip.

- The Job reads the new configuration from a hook ConfigMap of its own, `<full name>-migrate`.
- A Job that fails fails the upgrade: the release is `failed`, the Deployment keeps its pods and
  the old replicas serve. `kubectl -n lajstrom logs job/lajstrom-migrate` says why: a
  configuration error, or a migration that would break the replicas serving on the schema
  (upgrade through the release it requires).
- Helm waits for the Job up to `--timeout`, 5 minutes by default: give a long migration more.
- The Job and its ConfigMap stay until the next upgrade replaces them, for the log.
- `helm rollback` runs no Job: the release before runs on the newer schema, as
  `kubectl rollout undo` does ([operations.md](../operations.md#several-replicas) has how far
  back that works).
- `migrate.onInstall: true` runs the Job before the first install too; Postgres must answer by
  then. `migrate.database` gives the Job the schema owner's DSN when `serve` runs as a user with
  data rights only ([operations.md](../operations.md#a-database-user-for-serve)).
- The Job runs as the namespace's default ServiceAccount (`migrate.serviceAccountName`): on a
  first sync of Argo CD, which runs it as a `PreSync` hook, the chart's ServiceAccount does not
  exist yet.

## High availability

[`ci/ha-values.yaml`](../../deploy/helm/lajstrom/ci/ha-values.yaml) is the shape of a production
install, and `make helm-lint` renders it:

- three to six replicas by CPU (`autoscaling`), one per node: `DoNotSchedule` on
  `kubernetes.io/hostname` with `nodeTaintsPolicy: Honor`, so that a drained node does not hold
  the evicted replica back, and `matchLabelKeys: [pod-template-hash]`, so that each rollout's
  pods spread among themselves; the zones as well as they allow;
- the PodDisruptionBudget, at most one replica down; the rolling update, one new replica before
  an old one stops (`maxUnavailable: 0`);
- the route on the Gateway of `deploy/kubernetes/gateway/` (#834), the ServiceMonitor, and a
  NetworkPolicy, through `extraObjects`, that lets only the Gateway's proxies reach `http`, so that
  nothing else in the range of `trustedProxies` can name a client.

A constraint of `topologySpreadConstraints` without a `labelSelector` gets the pods' own.

## The edge

`httpRoute` is an HTTPRoute of the Gateway API: the whole host to the port `http`, no prefix or
rewrite, `timeouts.request: 0s`, and `hostnames` from `server.externalURL` unless set. The
Gateway is the cluster's or the one of `deploy/kubernetes/gateway/`; what Gateway API has no field
for, its implementation sets ([reverse-proxy.md](../reverse-proxy.md#gateway-api)): Envoy
Gateway's two policies can go into `extraObjects`, with `targetRefs` naming the route by the
release's full name.

`ingress` is an Ingress for the controller you name in `className`, with its annotations from
values. They must let bodies of any size through unbuffered and leave a long transfer alone
([reverse-proxy.md](../reverse-proxy.md)); ingress-nginx is retired, and its annotations are in
the table of [Gateway API](../reverse-proxy.md#gateway-api) for anyone moving off it.

## Metrics

`metrics.enabled` (on) opens `server.metricsListen` on `0.0.0.0:9464` and the port `metrics` of the
pods and the Service; no edge routes it. `metrics.serviceMonitor.enabled` adds the ServiceMonitor
of [operations.md](../operations.md#metrics), with the labels your Prometheus selects by in
`metrics.serviceMonitor.labels`.

## Try it on a local cluster

**Not for production.** With the fixtures of `deploy/kubernetes/try/` (#825), an image built from
the clone ([release.md](../release.md); kind wants `kind load docker-image lajstrom:local`), and a
port-forward instead of an edge. The fixture's registry is anonymous, so no password:

```yaml
# try-values.yaml
image: {repository: lajstrom, tag: local, pullPolicy: Never}
config:
  server: {externalURL: "http://localhost:18790", insecureHTTP: true}
  registries:
    - {name: main, url: "http://registry:5000", prefix: "lajstrom/"}
  repos:
    - {name: assets, format: raw, registry: main}
registryPasswords: []
```

```sh
pg=$(openssl rand -hex 16)
kubectl create namespace lajstrom-try
kubectl -n lajstrom-try create secret generic postgres --from-literal=password="$pg"
kubectl -n lajstrom-try create secret generic lajstrom \
  --from-literal=LAJSTROM_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=LAJSTROM_DATABASE_DSN="postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=disable"
kubectl -n lajstrom-try create secret generic lajstrom-bootstrap --from-literal=token="$(openssl rand -hex 32)"
kubectl -n lajstrom-try apply -f deploy/kubernetes/try/postgres.yaml -f deploy/kubernetes/try/registry.yaml
helm install lajstrom ./deploy/helm/lajstrom -n lajstrom-try -f try-values.yaml --wait --timeout 3m
kubectl -n lajstrom-try port-forward svc/lajstrom 18790:http &
```

Then the first admin with `--server http://localhost:18790`, and the check of
[kubernetes.md](kubernetes.md#5-check).

## The run

On 2026-11-12, on kind v0.33.0 (Kubernetes 1.37.0, one node), helm v4.3.1, with an image built
from main at `a7aa4d3e`. Not with the fixtures above: the machine's disk was too tight to load a
Postgres image into the cluster, so Postgres 17.11 was the dev stack's, as a client with a
database of its own, through `host.docker.internal`; the registry was distribution 3.1.2 in the
cluster with htpasswd, its password from the Secret `lajstrom`. The values were those of the
section above with that registry's `auth`, the prefix `ljh/`, and
`externalURL: http://localhost:18080`.

`helm install --wait` returned with both replicas ready; one applied the migrations, the other
none, and both signed with the same token key:

```text
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[1,2,…,20],"schema":20}
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[],"schema":20}
{"level":"INFO","msg":"bearer tokens are signed with the key in the database","fingerprint":"d84190a2…","algorithm":"ES256","keys.masterKeyEnv":"LAJSTROM_MASTER_KEY"}
{"level":"INFO","msg":"registry capabilities","registry":"main","vendor":"distribution",…}
```

Through `kubectl port-forward svc/lajstrom`: the first admin with the command of NOTES.txt, a raw
upload with `curl -T` and its download, and the metrics:

```text
readyz: ok
{"uid":"01a1d1a7-845d-78d8-99bc-507dec5db5b1","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-12T21:26:18.973827Z"}
PUT anonymous: 401
PUT: 201
GET: hello helm 2026-11-12T21:26:18Z
lajstrom_ingest_total{format="raw",result="published"} 1
```

`helm upgrade --set config.server.maxConcurrentUploads=16` ran the Job (`schema is current at
version 0020`), changed the checksum, and rolled both replicas to a new ReplicaSet within 17
seconds. An upgrade with `externalURL: http://localhost:18080/sub` failed in the Job and left the
replicas as they were:

```text
lajstrom: config: /etc/lajstrom/lajstrom.yaml: invalid:
server.externalURL: must be https://<host>[:port]: no path, query or fragment is allowed, Lajstrom owns the whole host
Error: UPGRADE FAILED: pre-upgrade hooks failed: resource Job/lajstrom/lajstrom-migrate not ready. status: Failed, message: Job Failed. failed: 1/1
```

`bootstrap.generate` made a 64-byte token, kept it across an upgrade, and
`bootstrap.enabled=false` removed the Secret. `kubectl label --dry-run=server ns lajstrom
pod-security.kubernetes.io/enforce=restricted` named no pod.

### The recovery

On 2026-11-13, on kind v0.33.0 (Kubernetes 1.37.0, one node) and helm v4.3.1. The image was built
from the branch of #440. Postgres 17.11 was the dev stack's, again with a database of its own. The
registry was the dev stack's Zot, anonymous, both through `host.docker.internal`. The values were
those of [Try it on a local cluster](#try-it-on-a-local-cluster) with that registry, and
`externalURL: http://localhost:16041`. The first admin was made with the command of NOTES.txt,
and then its password was taken as lost:

```text
$ helm upgrade lajstrom ./deploy/helm/lajstrom -n lajstrom-a440 --reuse-values --set recovery.user=admin --wait
STATUS: deployed                                        (8 s; the replicas kept their pods)
password length 32, alnum only: yes
whoami with the lost password: 401
whoami with the password of the Secret: 200
{"action":"user.password","principal":"anonymous","subject":"user:01a1d588-…","details":{"host":"lajstrom-recovery-xxbfq","name":"admin","source":"cli"}}
```

- **A second upgrade, still on:** the Job ran again, and the Secret kept its password, which
  still logged in.
- **`recovery.user=nobody`:** the upgrade failed after three attempts in 34 seconds, with the log
  and the release `failed` shown in [A lost password](#a-lost-password).
- **`recovery.user=`:** the release was `deployed` again and `lajstrom-recovery` was gone. The
  failed Job stayed until `kubectl delete job`.
- **`recovery.existingSecret` with a Secret of my own:** the chart made no Secret, the password
  before got 401, and the Secret's got 200.
- **A reinstall on the same database:** after `helm uninstall`, a `helm install` with
  `recovery.user=admin` ran the Job as a `post-install` hook, and its password logged in. With it,
  `POST /api/v1/users/admin/password` set a password of the admin's own (`204`), and the
  recovered one then got 401.
- **The namespace labelled `pod-security.kubernetes.io/enforce=restricted`:** the Job was admitted
  and succeeded.

## Values

| Value | Default | What |
| --- | --- | --- |
| `image.repository`, `image.tag`, `image.digest` | `ghcr.io/lajstrom/lajstrom`, the chart's `appVersion`, empty | the image; a digest is pulled beside the tag |
| `image.pullPolicy`, `imagePullSecrets` | `IfNotPresent`, `[]` | |
| `nameOverride`, `fullnameOverride` | empty | the full name is the release's when it contains `lajstrom`, else `<release>-lajstrom`, at most 50 characters |
| `config` | `externalURL` `https://art.example.com`, JSON logs, the registry `main` | `lajstrom.yaml`; the chart sets the keys of [2. The values](#2-the-values) |
| `masterKey.existingSecret`, `.key` | the full name, `LAJSTROM_MASTER_KEY` | the master key, as `LAJSTROM_MASTER_KEY` |
| `database.existingSecret`, `.key` | the full name, `LAJSTROM_DATABASE_DSN` | the Postgres DSN, as `LAJSTROM_DATABASE_DSN` |
| `registryPasswords[]` | `{env: LAJSTROM_REGISTRY_PASSWORD, existingSecret: "", key: LAJSTROM_REGISTRY_PASSWORD}` | one per `registries[].auth.passwordEnv`; an empty `existingSecret` is the full name |
| `bootstrap.enabled` | `true` | mount the bootstrap Secret and set `auth.bootstrap.tokenFile` |
| `bootstrap.existingSecret`, `.key` | `<full name>-bootstrap`, `token` | the Secret of the token |
| `bootstrap.generate` | `false` | the chart makes the Secret with a random token ([4](#4-the-first-admin)) |
| `migrate.enabled`, `migrate.onInstall` | `true`, `false` | the `pre-upgrade` Job, and before the first install too ([Upgrades](#upgrades)) |
| `migrate.database.existingSecret`, `.key` | `database`'s | the Job's own DSN |
| `migrate.backoffLimit`, `migrate.activeDeadlineSeconds` | `0`, none | |
| `migrate.serviceAccountName`, `migrate.podAnnotations`, `migrate.resources` | `default`, `{}`, 100m/64Mi, limit 256Mi | |
| `recovery.user` | empty, off | the Job that gives this user the password of the Secret ([A lost password](#a-lost-password)) |
| `recovery.existingSecret`, `.key` | `<full name>-recovery`, made by the chart; `password` | the Secret of the password |
| `recovery.backoffLimit`, `recovery.podAnnotations`, `recovery.resources` | `2`, `{}`, 100m/64Mi, limit 256Mi | |
| `replicaCount` | `2` | unless `autoscaling.enabled` |
| `strategy` | `RollingUpdate`, `maxUnavailable: 0`, `maxSurge: 1` | |
| `terminationGracePeriodSeconds` | `40` | must exceed `server.shutdownDelay` and `server.shutdownTimeout` together |
| `revisionHistoryLimit` | `10` | |
| `podDisruptionBudget.enabled`, `.maxUnavailable`, `.minAvailable` | `true`, `1`, none | `minAvailable` replaces `maxUnavailable` when set |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, the full name, `{}` | for a registry of type `ecr`: IRSA's `eks.amazonaws.com/role-arn` |
| `automountServiceAccountToken` | `false` | Lajstrom never talks to the Kubernetes API |
| `podAnnotations`, `podLabels` | `{}` | |
| `podSecurityContext`, `securityContext` | those of `deployment.yaml` | user 65532, `RuntimeDefault`, no privilege escalation, no capability, read-only root |
| `resources` | 100m/128Mi, limit 512Mi | |
| `startupProbe`, `livenessProbe`, `readinessProbe` | those of `deployment.yaml` | [kubernetes.md](kubernetes.md#probes) |
| `tmp.sizeLimit`, `tmp.medium` | `4Gi`, empty | `/tmp`; `Memory` makes it a tmpfs |
| `service.type`, `.port`, `.annotations` | `ClusterIP`, `8080`, `{}` | the port `http` |
| `httpRoute.enabled`, `.parentRefs`, `.hostnames`, `.timeouts` | `false`, `[]` (required), the host of `externalURL`, `request: 0s` | plus `.labels`, `.annotations` |
| `ingress.enabled`, `.className`, `.annotations`, `.host`, `.tls` | `false`, empty, `{}`, the host of `externalURL`, `[]` | |
| `metrics.enabled`, `metrics.port` | `true`, `9464` | |
| `metrics.serviceMonitor.enabled`, `.labels`, `.interval`, `.scrapeTimeout` | `false`, `{}`, `30s`, empty | |
| `autoscaling.enabled`, `.minReplicas`, `.maxReplicas` | `false`, `2`, `6` | |
| `autoscaling.targetCPUUtilizationPercentage`, `.targetMemoryUtilizationPercentage`, `.behavior` | `70`, none, `{}` | |
| `topologySpreadConstraints`, `affinity`, `nodeSelector`, `tolerations`, `priorityClassName` | empty | |
| `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts` | `[]` | an upstream's credential file, `TMPDIR` on a volume of its own |
| `extraObjects` | `[]` | more objects, maps or strings, each rendered with `tpl` |

## What was not verified

- The HTTPRoute, the Ingress, the ServiceMonitor and the HPA on a cluster: `make helm-lint`
  validates them against their schemas only. The route's settings ran through Envoy Gateway in
  #834.
- A pending migration through the hook (the image had none to apply), Argo CD and Flux, Helm 3.
- The fixtures of `deploy/kubernetes/try/` with the chart, a multi-node cluster, a released image.
