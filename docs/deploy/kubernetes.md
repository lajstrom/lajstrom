# Deploy on Kubernetes

Plain manifests in [deploy/kubernetes/](../../deploy/kubernetes/), applied with `kubectl apply
-k`; the Helm chart renders the same ([helm.md](helm.md)). Two replicas of Lajstrom behind one
Service; every replica is the same, since the state is in Postgres and in the registry.

| File | What |
| --- | --- |
| `base/configmap.yaml` | `lajstrom-config`: the configuration file, mounted at `/etc/lajstrom/lajstrom.yaml` |
| `base/deployment.yaml` | `lajstrom`: two replicas, probes, the non-root security context, `/tmp` as an `emptyDir`, the optional Secret `lajstrom-bootstrap` as a file |
| `base/service.yaml` | the Service `lajstrom` (ports `http` 8080 and `metrics` 9464) and a PodDisruptionBudget |
| `base/kustomization.yaml` | the three files, the base of an overlay |
| `kustomization.yaml` | `base`, for `kubectl apply -k deploy/kubernetes` |
| `migrate-job.yaml` | the Job of [Upgrade](#upgrade), applied on its own |
| `gateway/` | a Gateway and an HTTPRoute in front of the Service, and Envoy Gateway's policies ([Gateway API](#gateway-api)) |
| `try/` | not for production: an overlay with a Postgres and a registry in the namespace ([Try it on a local cluster](#try-it-on-a-local-cluster)) |

The Secret `lajstrom` is not among them: it is made from the commands below or by your secret
store (External Secrets, Sealed Secrets, Vault), with three keys that the Deployment passes to
the container as environment variables (`envFrom`). The manifests are in `base/` so that an
overlay inside `deploy/kubernetes/` can build on them: kustomize refuses a base that contains the
overlay (`cycle detected`).

## Try it on a local cluster

**Not for production.** [deploy/kubernetes/try/](../../deploy/kubernetes/try/) replaces what the
manifests assume with fixtures in the namespace, for a cluster on one machine (OrbStack, Docker
Desktop, kind, minikube) and a port-forward instead of an ingress:

| File | What |
| --- | --- |
| `kustomization.yaml` | `../base`, the two fixtures, `lajstrom.yaml` in place of the file of `base/configmap.yaml` (`configMapGenerator`, `behavior: replace`), the image `lajstrom:local` |
| `lajstrom.yaml` | `externalURL` `http://localhost:18790` with `insecureHTTP: true`, the Service `postgres`, the Service `registry` without auth, the repos `raw/assets` and `oci/images` |
| `postgres.yaml` | Postgres 17.11 with a 1 GiB PersistentVolumeClaim; the password is the key `password` of the Secret `postgres` |
| `registry.yaml` | distribution 3.1.2 with a 5 GiB PersistentVolumeClaim |

It is not for production because each fixture is one replica on a volume of the cluster's default
StorageClass, without a backup; anything in the cluster that reaches the registry may push and
delete there; Postgres has no TLS; and Lajstrom answers plain HTTP for `localhost`. The fixtures
are pinned by digest and, like Lajstrom, meet the `restricted` Pod Security Standard. No release
is published yet, so the image is `lajstrom:local`, built from the clone
([release.md](../release.md)): OrbStack and Docker Desktop see the local Docker's images, kind
wants `kind load docker-image lajstrom:local` first; for a release, the comment in
`kustomization.yaml` names it by digest.

```sh
pg=$(openssl rand -hex 16)
kubectl create namespace lajstrom-try
kubectl -n lajstrom-try create secret generic postgres --from-literal=password="$pg"
kubectl -n lajstrom-try create secret generic lajstrom \
  --from-literal=LAJSTROM_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=LAJSTROM_DATABASE_DSN="postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=disable"
kubectl -n lajstrom-try create secret generic lajstrom-bootstrap --from-literal=token="$(openssl rand -hex 32)"
kubectl -n lajstrom-try apply -k deploy/kubernetes/try
kubectl -n lajstrom-try rollout status deploy/lajstrom
```

The replicas of Lajstrom stop until Postgres answers (`postgres: connect: … connection
refused`) and the kubelet starts them again. Then the first admin as in
[section 4](#4-the-first-admin-and-the-repos), without the repos, which `lajstrom.yaml` lists,
and the check of [section 5](#5-check), in the namespace `lajstrom-try`. To remove everything,
the volumes with it: `kubectl delete namespace lajstrom-try`.

The run on 2026-11-12 was on the Kubernetes of OrbStack (1.36.5, one node), with an image built
from main at `a7aa4d3e`: the commands above, then section 4 with `--password-stdin` and
section 5, and a 32 MiB file up and down. Postgres was ready 7 seconds after `apply`; each
replica stopped twice, and `rollout status` returned 21 seconds after `apply`. One replica
applied the migrations 1 to 20 and made the repos, the other found them done:

```text
{"uid":"01a1d051-326f-7352-880a-bf2a4fc9be29","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-12T15:12:24.687916Z"}
ok                                       # /-/readyz
201                                      # the upload of hello.txt; 401 without credentials
hello kubernetes
Login Succeeded
Digest: sha256:4ab97eff8f8f811b374ca60f55cdf27d2190dfc3d52bbecf35cfe905f5bcd4d4
{"repositories":["lajstrom/oci/images/hello","lajstrom/raw/assets/big.bin","lajstrom/raw/assets/hello.txt"]}
```

The last line is the registry's catalog (`kubectl -n lajstrom-try exec deploy/registry -- wget
-qO- http://127.0.0.1:5000/v2/_catalog`), the same after a `rollout restart` of both fixtures,
as was the admin in Postgres. `kubectl label --dry-run=server ns lajstrom-try
pod-security.kubernetes.io/enforce=restricted` named no pod.

## What the manifests assume

- **Postgres**, reachable from the pods, with a database that Lajstrom's user owns (`serve` and
  `migrate` create and alter its tables): a managed one (RDS, Cloud SQL, Azure Database), an
  operator's (CloudNativePG, Zalando), or one you run, straight or behind a pooler
  ([databases.md](databases.md)). The manifests do not ship one, but for the fixture of
  [`try/`](#try-it-on-a-local-cluster). Every replica shares it; SQLite does not work with more than
  one replica, and these manifests have no volume for it. The Postgres that Lajstrom is tested
  against is 17.11 ([dev-stack.md](../dev-stack.md)).
- **A registry** for the bytes, with a user that may push, pull and delete under a prefix
  ([registries/](../registries/README.md) has what each one needs).
- **An ingress or a Gateway** that terminates TLS for the public host and routes the whole host,
  without a path prefix, to the port `http` of the Service, with unlimited and unbuffered
  request bodies ([http-routing.md](../spec/http-routing.md) §1). What the edge must do in detail
  is in [reverse-proxy.md](../reverse-proxy.md). `gateway/` is a Gateway for it
  ([Gateway API](#gateway-api)); there is no Ingress, since ingress-nginx, whose annotations an
  Ingress would carry, is retired.

## 1. The Secret

```sh
kubectl create namespace lajstrom
kubectl -n lajstrom create secret generic lajstrom \
  --from-literal=LAJSTROM_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=LAJSTROM_DATABASE_DSN="postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=disable" \
  --from-literal=LAJSTROM_REGISTRY_PASSWORD="$reg"
kubectl -n lajstrom create secret generic lajstrom-bootstrap \
  --from-literal=token="$(openssl rand -hex 32)"
```

`$pg` and `$reg` are the passwords of the database user and of the registry user, and
`postgres:5432` is the database's host and port, here a Service `postgres` in the namespace.
The Secret `lajstrom-bootstrap` holds the one-time token that makes the first admin
(section 4): the Deployment mounts it as `/run/secrets/lajstrom-bootstrap/token`, the file of
`auth.bootstrap.tokenFile`, and you delete it once the admin exists. The master key stays a
variable here rather than `keys.masterKeyFile`: a Secret volume's files are root's, `0440` at
best for the user 65532, which Lajstrom warns about
([operations.md](../operations.md#the-master-key)). A managed
Postgres wants `sslmode=require` (or `verify-full` with its CA). **Keep a copy of the master
key outside the cluster and apart from the database backups**
([operations.md](../operations.md#the-master-key)): `kubectl -n lajstrom get secret lajstrom -o
jsonpath='{.data.LAJSTROM_MASTER_KEY}' | base64 -d`.

## 2. The configuration

Edit `base/configmap.yaml`, or replace its file from an overlay of your own as `try/` does:

- `server.externalURL`: the public URL of the ingress, `https://art.example.com` in the file.
  The realm of the bearer challenge of `/v2` is built from it.
- `server.trustedProxies`, commented out in the file: the addresses the ingress or the Gateway
  connects to the pods from. While it is empty, Lajstrom takes the edge for the client: the
  per-address throttles of failed logins and of `/v2/token` are off, the startup log says so,
  and the access and audit logs record the edge's address ([auth.md](../auth.md#failed-logins)).
  No address fits every cluster, so find yours: apply with the list empty, send one request
  through the edge, and read `remoteAddr` of its `request` line in `kubectl -n lajstrom logs`,
  an address and a port. Then list the range that address comes from, not the address itself: a controller's pod gets a
  new one from the pod CIDR when it is rescheduled, and a node address, an Istio sidecar or a
  cloud balancer are other cases. Every peer in the range can name any client, so a wide range
  wants a NetworkPolicy that admits only the edge
  ([reverse-proxy.md](../reverse-proxy.md#the-client-address) has the cases, and when to leave
  the list empty).
- `registries[0].url`, `prefix`, `auth.username`: your registry.
- The image: `ghcr.io/lajstrom/lajstrom:v0.1.0` in `base/deployment.yaml`; pin a digest
  (`ghcr.io/lajstrom/lajstrom:vX.Y.Z@sha256:…`), there or with `images:` in an overlay.

A ConfigMap's file is one string, so an overlay replaces it whole: `try/kustomization.yaml` does
it with a `configMapGenerator` of `behavior: replace` and a file next to it, `try/lajstrom.yaml`.
An `http` `externalURL` takes `insecureHTTP: true`, as that file has; without it every replica
stops at once (`server.externalURL: must be https`, `CrashLoopBackOff`).

## 3. Apply

```sh
kubectl -n lajstrom apply -k deploy/kubernetes
kubectl -n lajstrom rollout status deploy/lajstrom
```

The run below, on 2026-11-11, was on the Kubernetes of OrbStack 2.2.5 (1.36.5, one node), in
the namespace `walkthrough`, with an overlay that `try/` comes from (an image built from main),
and two test fixtures then outside the repository: Postgres 17.11 as the Service `postgres` and
distribution 3.1.2 with htpasswd as the Service `registry`. An earlier run, on 2026-11-09, was
on kind v0.33.0 (Kubernetes 1.37.0) with the snapshot image of the release build (#57).

```text
configmap/lajstrom-config created
service/lajstrom created
deployment.apps/lajstrom created
poddisruptionbudget.policy/lajstrom created
Waiting for deployment "lajstrom" rollout to finish: 0 of 2 updated replicas are available...
Waiting for deployment "lajstrom" rollout to finish: 1 of 2 updated replicas are available...
deployment "lajstrom" successfully rolled out
```

Both replicas apply the migrations at start, each one under the migration lock, so between them
they apply each once: here one applied 1 to 11 and the other 12 and 13. One bearer-token key in
the database serves both (`kubectl -n lajstrom logs <pod>`, a selection, without the times):

```text
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[1,2,3,4,5,6,7,8,9,10,11]}
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[12,13]}
{"level":"INFO","msg":"token signing key made","fingerprint":"e16202518b071f0a9e161d8acdf3cd560468bd2f141e37f28293182a333fc2f8","algorithm":"ES256"}
{"level":"INFO","msg":"registry capabilities","registry":"main","vendor":"distribution","createOnPush":true,"referrers":false,"catalog":true,"deleteManifest":true,"immutableTags":false,"blobRedirect":true,"maxBlobSize":0,"consistency":"strong","maxNameDepth":0}
{"level":"INFO","msg":"listening","addr":"[::]:8080","externalURL":"http://localhost:18790","tls":false}
{"level":"INFO","msg":"metrics listening","addr":"[::]:9464","pprof":false}
```

The `registry capabilities` line is from main at `c3deee75`, on 2026-11-12, against the
distribution 3.1.2 of the dev stack: the image of the run was older, and its line named the
vendor `unknown` and had neither `consistency` nor `maxNameDepth`.

## 4. The first admin and the repos

No admin exists yet, so the replicas made the token of the Secret `lajstrom-bootstrap` valid at
start ([operations.md](../operations.md#the-first-admin)), on both, and logged that, never the
token. The image has no shell; `kubectl exec` runs the binary itself, with the pod's environment,
configuration and files: `admin bootstrap` reads the token from the mounted Secret, asks the
server of the pod and prompts for the password. Then the token is dead, and the Secret goes:

```sh
kubectl -n lajstrom exec -it deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin bootstrap --name admin \
  --token-file /run/secrets/lajstrom-bootstrap/token
kubectl -n lajstrom delete secret lajstrom-bootstrap
kubectl -n lajstrom exec deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin repo create raw/assets --registry main
kubectl -n lajstrom exec deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin repo create oci/images --registry main
```

```text
{"uid":"01a1c15b-4142-7d54-b491-a03967ca3c1d","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-09T17:29:05.602748Z"}
{"format":"raw","name":"assets","registry":"main","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-09T17:29:05.682413Z","updatedAt":"2026-11-09T17:29:05.682413Z"}
{"format":"oci","name":"images","registry":"main","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-09T17:29:05.760261Z","updatedAt":"2026-11-09T17:29:05.760261Z"}
```

From a machine outside the cluster, `lajstrom admin bootstrap --server https://art.example.com
--name admin --token-file <(kubectl -n lajstrom get secret lajstrom-bootstrap -o
jsonpath='{.data.token}' | base64 -d)` does the same through the ingress. The volume of the
Secret is optional: without it, `serve` starts and warns that there is no bootstrap token, and
once it is deleted the file leaves the pods. `admin user create --name admin --admin
--password-stdin` through `kubectl exec -i` makes the admin straight in the database instead,
and is how a lost admin password is recovered ([operations.md](../operations.md#a-lost-password)).

Repos can also be listed in the ConfigMap (`repos:`, [configuration.md](../configuration.md#repos)),
which creates them at the start of a replica when the database does not have them.

## 5. Check

Through the ingress, or a port-forward as in the run; `pw` holds the admin's password:

```sh
read -rs pw
kubectl -n lajstrom port-forward svc/lajstrom 18790:http &
printf 'hello kubernetes\n' > hello.txt
curl -s localhost:18790/-/readyz                                                                  # ok
curl -s -u "admin:$pw" -o /dev/null -w '%{http_code}\n' -T hello.txt localhost:18790/raw/assets/hello.txt   # 201
curl -s -u "admin:$pw" localhost:18790/raw/assets/hello.txt                  # hello kubernetes
printf '%s\n' "$pw" | oras login --plain-http -u admin --password-stdin localhost:18790
oras push --plain-http --artifact-type application/vnd.example.notes localhost:18790/oci/images/hello:1 hello.txt
oras pull --plain-http -o out localhost:18790/oci/images/hello:1
```

```text
Login Succeeded
ArtifactType: application/vnd.example.notes
Digest: sha256:aaf0230765342b32740b96cbf849bbe37815b1762f7acc0ec614acfaf475a91d
Digest: sha256:aaf0230765342b32740b96cbf849bbe37815b1762f7acc0ec614acfaf475a91d
```

The metrics answer on the pod's port `metrics`, never on `http`
([operations.md](../operations.md#metrics) has the ServiceMonitor). Each replica counts its own:
a port-forward to the Service picks one pod, the same one for both forwards in the run, so the
upload above shows; through an ingress it may have gone to the other replica.

```sh
kubectl -n lajstrom port-forward svc/lajstrom 18791:metrics &
curl -s localhost:18791/metrics | grep -E '^lajstrom_build_info|^lajstrom_ingest_total'
```

```text
lajstrom_build_info{commit="533acf3",goversion="go1.27.3",version="v0.0.1-357-g533acf3"} 1
lajstrom_ingest_total{format="raw",result="published"} 1
```

## Gateway API

[deploy/kubernetes/gateway/](../../deploy/kubernetes/gateway/) puts the Service behind a
Gateway. ingress-nginx retired in March 2026 and its repository is archived
([reverse-proxy.md](../reverse-proxy.md#gateway-api)), so there is no Ingress.

| File | What |
| --- | --- |
| `gateway.yaml` | the Gateway `lajstrom`: one `HTTPS` listener for `art.example.com`, TLS from the Secret `lajstrom-tls`; leave it out to attach the route to a shared Gateway |
| `httproute.yaml` | the whole host to the port `http`, with `timeouts.request: 0s`, no bound on a long transfer |
| `envoy-gateway.yaml` | Envoy Gateway's ClientTrafficPolicy and BackendTrafficPolicy: what Gateway API has no field for |
| `kustomization.yaml` | the three |

Set the host of `server.externalURL` in both `gateway.yaml` and `httproute.yaml`, and your
`gatewayClassName`. With `server.fqdn.domain`, add a listener for `*.<domain>` to `gateway.yaml`,
with a certificate that has the name, and `*.<domain>` to the route's `hostnames`
([reverse-proxy.md](../reverse-proxy.md#fqdn-mode)). On an implementation other than Envoy Gateway, drop `envoy-gateway.yaml` from
`kustomization.yaml` and set the same things its way. [reverse-proxy.md](../reverse-proxy.md#gateway-api)
has each setting, the ingress-nginx annotation it replaces, and what Cilium and Traefik need.

```sh
kubectl -n lajstrom create secret tls lajstrom-tls --cert=tls.crt --key=tls.key   # or cert-manager
kubectl -n lajstrom apply -k deploy/kubernetes/gateway
kubectl -n lajstrom wait --for=condition=Programmed gateway/lajstrom
```

`server.trustedProxies` (section 2) is the range the Gateway's proxy pods take their addresses
from. For Envoy Gateway that is the pod CIDR: its proxies run in `envoy-gateway-system`. Add a
NetworkPolicy that lets only that namespace reach the port `http`, so that nothing else in the
range can name a client.

The run on 2026-11-12, on OrbStack with Envoy Gateway v1.10.0, is in
[reverse-proxy.md](../reverse-proxy.md#gateway-api). Through it: a 200 MiB raw file up and
down, and 40 MiB at 1 MB/s, each with the same sha256; an `oras` push and pull; the client's
address in the access log. Without `timeouts.request: 0s`, Envoy cut the slow download after
17 s.

## Probes

| Probe | Path | Why |
| --- | --- | --- |
| startup | `/-/healthz`, every 2 s, 30 tries | `serve` applies the migrations and asks the registries before it listens, and liveness must not count that. The start gets about a minute, up to 20 s of it for a registry that does not answer ([configuration.md](../configuration.md#what-serve-needs)), before the kubelet restarts the container: run a long migration as the Job of [Upgrade](#upgrade), or raise `failureThreshold` |
| liveness | `/-/healthz`, every 10 s | only "the process answers": an outage of the database or the registry must not restart the pods |
| readiness | `/-/readyz`, every 5 s | `503` while the database does not answer or a registry has not answered `GET /v2/` for a minute ([operations.md](../operations.md#readiness)), and from `SIGTERM` on |

A stopping pod needs no `preStop` hook. Kubernetes removes its endpoint while it sends `SIGTERM`,
whatever the readiness probe says, and the Service and the ingress take a few seconds to follow;
`serve` keeps its listener open for `server.shutdownDelay` (5 s) after the signal, answering
`/-/readyz` with `503` and closing each connection after its response, then drains for
`server.shutdownTimeout` (30 s). `terminationGracePeriodSeconds` must exceed the two together:
`base/deployment.yaml` sets 40. A `preStop` sleep, which needs Kubernetes 1.30 for the `sleep` action
(the image has no shell), adds to the delay and to what the grace period must cover.

Tried by scaling the registry to zero at 17:29:25 and polling the ready flag and the restart
count of both replicas every 5 seconds: they left the endpoints within 56 seconds and were not
restarted; once the registry was back, they were ready again at the next poll but one. The run
of 2026-11-11 on OrbStack: out of the endpoints after 65 seconds, ready again 20 seconds after
the registry, no restart.

```text
17:29:25
17:29:31 true/0 true/0
…
17:30:16 true/0 true/0
17:30:21 false/0 false/0
Unhealthy   Readiness probe failed: HTTP probe failed with statuscode: 503
{"level":"WARN","msg":"not ready","error":"a registry has not answered for a minute","requestId":"5f81d584479839e1bb92758b677af6a6"}
deployment "registry" successfully rolled out
17:30:28 false/0 false/0
17:30:33 true/0 true/0
```

## Security context

The pod runs as the image's user `nonroot` (65532:65532) with `runAsNonRoot`, the
`RuntimeDefault` seccomp profile, no privilege escalation, every capability dropped and a
read-only root filesystem; it does not mount a service account token, since it never talks to
the Kubernetes API. That is the `restricted` Pod Security Standard: labelling the namespace
`pod-security.kubernetes.io/enforce=restricted` (a server dry run) listed only the two test
fixtures, Postgres and the registry, as violations; those of `try/` meet it too.

The only writable path is `/tmp`, an `emptyDir` of at most 4 GiB on the node's disk. A frontend
upload (raw, apt, maven, pypi, npm) and a proxy repo's fetch stream into the registry and take
none; what is spooled there is a `/v2` chunk of a registry with `uploads.spoolUnknownLength`
on. A full `emptyDir` evicts the pod with every request it serves, so the configuration sets
`server.spoolBudget` to 3 GiB, below the 4 GiB `sizeLimit`: a chunk that would pass it is
answered `503` with `Retry-After`, nothing of it reaches the registry, and the client sends it
again. Keep the budget below the `sizeLimit` when you change either. To spool on a volume of its
own instead, mount it, set `TMPDIR` to its path and the budget below its size: Lajstrom spools
into `$TMPDIR` when it is set, `/tmp` otherwise.

Memory: after a 100 MiB raw upload and its download through one replica, `crictl stats` on the
node showed 32 MB for that replica and 23 MB for the other. `limits.memory: 512Mi` leaves room
for the manifests the OCI facade holds in memory (`server.maxManifestSize`, 4 MiB each); it was
not load-tested.

## Upgrade

Back up the database ([operations.md](../operations.md#backing-up)), run `lajstrom migrate` of
the new release once, as the Job of `migrate-job.yaml`, then set the new image:

```sh
kubectl -n lajstrom delete job lajstrom-migrate --ignore-not-found
kubectl set image --local -f deploy/kubernetes/migrate-job.yaml \
  migrate=ghcr.io/lajstrom/lajstrom:vX.Y.Z -o yaml | kubectl -n lajstrom apply -f -
kubectl -n lajstrom wait --for=condition=complete --timeout=30m job/lajstrom-migrate
kubectl -n lajstrom logs job/lajstrom-migrate
kubectl -n lajstrom set image deployment/lajstrom lajstrom=ghcr.io/lajstrom/lajstrom:vX.Y.Z
kubectl -n lajstrom rollout status deploy/lajstrom
```

The rolling update starts one new replica before it stops an old one (`maxUnavailable: 0`), and
the old ones serve on the migrated schema meanwhile. Without the Job, the first new replica
applies the migrations at start, under the lock. The rules, the rollback and the releases with
a contract migration are in [operations.md](../operations.md#upgrades).

The run below was on OrbStack's Kubernetes 1.36.5 (one node), in the namespace
`migrations-e2e`, with these manifests in an overlay as in the run above (an in-cluster
distribution 3.1.2 and Postgres 17.11), and three images built from one commit, three releases:
`n` (migrations up to 0014), `n1` (and 0015, a new table: expand), `n2` (and 0016, which drops
it, `-- lajstrom:requires 0015`: contract). The steps, with what the Job printed and what the
replicas logged at start (a selection, without the times):

| Step | Commands | What happened |
| --- | --- | --- |
| install `n` | `apply -k` | one replica applied 0001 to 0014 under the lock, the other none: `"applied":[],"schema":14` |
| skip to `n2` | the Job of `n2` | `Failed`: `a pending migration would break the replicas serving on the schema: migration postgres/0016_drop_upgrade_probe.sql needs a binary that knows migration 0015, and the schema is at version 0014; upgrade through the release in between, or stop every replica and run lajstrom migrate --allow-breaking`; nothing applied, `n` serving |
| `n` to `n1` | the Job of `n1`, `set image` | `applied 0015`, while `n` served; the new replicas: `"applied":[],"schema":15` |
| back to `n` | `rollout undo` | `"msg":"the index schema is newer than this binary, and compatible with it","schema":15,"known":14` |
| `n1` to `n2` | `set image` `n1`, the Job of `n2`, `set image` `n2` | `applied 0016`; the new replicas: `"applied":[],"schema":16` |
| back to `n1` | `rollout undo` | `"msg":"the index schema is newer than this binary, and compatible with it","schema":16,"known":15` |
| back to `n` | `set image` `n` | the new pod in `CrashLoopBackOff`: `lajstrom: index: the schema is newer than this binary: schema version 0016 needs a binary that knows migration 0015, and this one knows up to 0014`; `rollout status` timed out, the two `n1` replicas kept serving |
| forward to `n2` | `set image` `n2` | rolled out |

A pod requested a file through the Service four times a second throughout. Without a `preStop`
hook, some requests of every rollout found a stopping replica's listener closed, before its
endpoint was gone: 5 of 194 over two rollouts got no answer. With the hook `deployment.yaml` had
then (a 5-second sleep before `SIGTERM`), none of 228 over the same two rollouts. Since #707,
`server.shutdownDelay` keeps the listener open the same 5 seconds after `SIGTERM` instead, and
`deployment.yaml` has no hook; that was not run again on a cluster.

## What was not verified

- An Ingress; a Gateway other than Envoy Gateway; a multi-node cluster.
- A managed Postgres, `sslmode=require`, and an external registry.
- An upgrade from a published release: the run above built its three releases from one commit
  (#485 tests the binary of the previous release after v0.1.0). A migration that runs long.
