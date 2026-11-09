# Deploy on Kubernetes

Plain manifests in [deploy/kubernetes/](../../deploy/kubernetes/), applied with `kubectl apply
-k`; the Helm chart is #82. Two replicas of Lajstrom behind one Service; every replica is the
same, since the state is in Postgres and in the registry.

| File | What |
| --- | --- |
| `configmap.yaml` | `lajstrom-config`: the configuration file, mounted at `/etc/lajstrom/lajstrom.yaml` |
| `deployment.yaml` | `lajstrom`: two replicas, probes, the non-root security context, `/tmp` as an `emptyDir` |
| `service.yaml` | the Service `lajstrom` (ports `http` 8080 and `metrics` 9464) and a PodDisruptionBudget |
| `kustomization.yaml` | the three files, for `kubectl apply -k` |

The Secret `lajstrom` is not among them: it is made from the commands below or by your secret
store (External Secrets, Sealed Secrets, Vault), with three keys that the Deployment passes to
the container as environment variables (`envFrom`).

## What the manifests assume

- **Postgres**, reachable from the pods, with a database that Lajstrom's user owns (`serve` and
  `migrate` create and alter its tables): a managed one (RDS, Cloud SQL, Azure Database), an
  operator's (CloudNativePG, Zalando), or one you run. The manifests do not ship one. Every replica shares it; SQLite does not work with more than
  one replica, and these manifests have no volume for it. The Postgres that Lajstrom is tested
  against is 17.11 ([dev-stack.md](../dev-stack.md)).
- **A registry** for the bytes, with a user that may push, pull and delete under a prefix
  ([registries/](../registries/README.md) has what each one needs).
- **An ingress or a Gateway** that terminates TLS for the public host and routes the whole host,
  without a path prefix, to the port `http` of the Service, with unlimited and unbuffered
  request bodies ([http-routing.md](../spec/http-routing.md) §1). What the edge must do in detail
  is #129; no Ingress is in the manifests, since its annotations depend on the controller.

## 1. The Secret

```sh
kubectl create namespace lajstrom
kubectl -n lajstrom create secret generic lajstrom \
  --from-literal=LAJSTROM_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=LAJSTROM_DATABASE_DSN="postgres://lajstrom:$pg@postgres:5432/lajstrom?sslmode=disable" \
  --from-literal=LAJSTROM_REGISTRY_PASSWORD="$reg"
```

`$pg` and `$reg` are the passwords of the database user and of the registry user. The master
key stays a variable here rather than `keys.masterKeyFile`: a Secret volume's files are root's,
`0440` at best for the user 65532, which Lajstrom warns about
([operations.md](../operations.md#the-master-key)). A managed
Postgres wants `sslmode=require` (or `verify-full` with its CA). **Keep a copy of the master
key outside the cluster and apart from the database backups**
([operations.md](../operations.md#the-master-key)): `kubectl -n lajstrom get secret lajstrom -o
jsonpath='{.data.LAJSTROM_MASTER_KEY}' | base64 -d`.

## 2. The configuration

Edit `configmap.yaml` (or patch it from an overlay of your own):

- `server.externalURL`: the public URL of the ingress, `https://art.example.com` in the file.
  The realm of the bearer challenge of `/v2` is built from it.
- `registries[0].url`, `prefix`, `auth.username`: your registry.
- The image: `ghcr.io/lajstrom/lajstrom:v0.1.0` in `deployment.yaml`; pin a digest
  (`ghcr.io/lajstrom/lajstrom:vX.Y.Z@sha256:…`), there or with `images:` in an overlay, as the
  run below did.

## 3. Apply

```sh
kubectl -n lajstrom apply -k deploy/kubernetes
kubectl -n lajstrom rollout status deploy/lajstrom
```

The run below was on kind v0.33.0 (Kubernetes 1.37.0, one node), in the namespace
`lajstrom-docs`, with an overlay that set the snapshot image of the release build (#57), an
in-cluster distribution 3.1.2 with htpasswd and Postgres 17.11 as the registry and the
database, and `externalURL: "http://localhost:18790"` for a port-forward:

```text
configmap/lajstrom-config created
service/lajstrom created
deployment.apps/lajstrom created
poddisruptionbudget.policy/lajstrom created
Waiting for deployment "lajstrom" rollout to finish: 0 out of 2 new replicas have been updated...
Waiting for deployment "lajstrom" rollout to finish: 0 of 2 updated replicas are available...
Waiting for deployment "lajstrom" rollout to finish: 1 of 2 updated replicas are available...
deployment "lajstrom" successfully rolled out
```

Both replicas apply the migrations under one lock at start, and one bearer-token key in the
database serves both (`kubectl -n lajstrom logs deploy/lajstrom`, one pod, a selection, without
the times):

```text
{"level":"INFO","msg":"index migrations applied under the migration lock","database.driver":"postgres","applied":[1,2,3,4,5,6,7,8,9]}
{"level":"INFO","msg":"token signing key made","fingerprint":"7a9638bea065f8a06456d321064f4d7396a73b3421b2fe6cab22146f7d8f1ba9","algorithm":"ES256"}
{"level":"INFO","msg":"registry capabilities","registry":"main","vendor":"unknown","createOnPush":true,"referrers":false,"catalog":true,"deleteManifest":true,"immutableTags":false,"blobRedirect":true,"maxBlobSize":0}
{"level":"INFO","msg":"listening","addr":"[::]:8080","externalURL":"http://localhost:18790"}
{"level":"INFO","msg":"metrics listening","addr":"[::]:9464","pprof":false}
```

## 4. The first admin and the repos

The image has no shell; `kubectl exec` runs the binary itself, with the pod's environment and
configuration:

```sh
printf '%s\n' "$LAJSTROM_ADMIN_PASSWORD" | kubectl -n lajstrom exec -i deploy/lajstrom -- \
  /lajstrom --config /etc/lajstrom/lajstrom.yaml admin user create --name admin --admin --password-stdin
kubectl -n lajstrom exec deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin repo create raw/assets --registry main
kubectl -n lajstrom exec deploy/lajstrom -- /lajstrom --config /etc/lajstrom/lajstrom.yaml admin repo create oci/images --registry main
```

```text
{"uid":"01a1c15b-4142-7d54-b491-a03967ca3c1d","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-09T17:29:05.602748Z"}
{"format":"raw","name":"assets","registry":"main","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-09T17:29:05.682413Z","updatedAt":"2026-11-09T17:29:05.682413Z"}
{"format":"oci","name":"images","registry":"main","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-09T17:29:05.760261Z","updatedAt":"2026-11-09T17:29:05.760261Z"}
```

Repos can also be listed in the ConfigMap (`repos:`, [configuration.md](../configuration.md#repos)),
which creates them at the start of a replica when the database does not have them.

## 5. Check

Through the ingress, or a port-forward as in the run:

```sh
kubectl -n lajstrom port-forward svc/lajstrom 18790:http &
curl -s localhost:18790/-/readyz                                                                  # ok
curl -s -u "admin:$LAJSTROM_ADMIN_PASSWORD" -o /dev/null -w '%{http_code}\n' -T hello.txt localhost:18790/raw/assets/hello.txt   # 201
curl -s -u "admin:$LAJSTROM_ADMIN_PASSWORD" localhost:18790/raw/assets/hello.txt                  # hello kubernetes
printf '%s\n' "$LAJSTROM_ADMIN_PASSWORD" | oras login --plain-http -u admin --password-stdin localhost:18790
oras push --plain-http --artifact-type application/vnd.example.notes localhost:18790/oci/images/hello:1 hello.txt
oras pull --plain-http -o out localhost:18790/oci/images/hello:1
```

```text
Login Succeeded
ArtifactType: application/vnd.example.notes
Digest: sha256:eba236d7b34af898e75dae44e9621be78f3d00cde74cf33ce6f9f60b82dd8533
Digest: sha256:eba236d7b34af898e75dae44e9621be78f3d00cde74cf33ce6f9f60b82dd8533
```

The metrics answer on the pod's port `metrics`, never on `http`
([operations.md](../operations.md#metrics) has the ServiceMonitor):

```sh
kubectl -n lajstrom port-forward svc/lajstrom 18791:metrics &
curl -s localhost:18791/metrics | grep -E '^lajstrom_build_info|^lajstrom_ingest_total'
```

```text
lajstrom_build_info{commit="aa808b3",goversion="go1.27.3",version="v0.0.1-138-gaa808b3"} 1
lajstrom_ingest_total{format="raw",result="published"} 1
```

## Probes

| Probe | Path | Why |
| --- | --- | --- |
| startup | `/-/healthz`, every 2 s, 30 tries | `serve` applies the migrations before it listens; a long migration must not count against liveness |
| liveness | `/-/healthz`, every 10 s | only "the process answers": an outage of the database or the registry must not restart the pods |
| readiness | `/-/readyz`, every 5 s | `503` while the database does not answer or a registry has not answered `GET /v2/` for a minute ([operations.md](../operations.md#readiness)) |

Tried by scaling the registry to zero at 17:29:25 and polling the ready flag and the restart
count of both replicas every 5 seconds: they left the endpoints within 56 seconds and were not
restarted; once the registry was back, they were ready again at the next poll but one.

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
fixtures, Postgres and the registry, as violations.

The only writable path is `/tmp`, an `emptyDir` of at most 4 GiB on the node's disk: an upload
(raw, apt, pypi, maven) is spooled there for the length of its request and removed after. Size
it for the largest concurrent uploads of one replica, or the pod is evicted when it fills.

Memory: after a 100 MiB raw upload and its download through one replica, `crictl stats` on the
node showed 32 MB for that replica and 23 MB for the other. `limits.memory: 512Mi` leaves room
for the manifests the OCI facade holds in memory (`server.maxManifestSize`, 4 MiB each); it was
not load-tested.

## Upgrade

Set the new image; the rolling update starts one new replica before it stops an old one
(`maxUnavailable: 0`). The new replica applies the migrations at start, under the lock, while
the old ones still serve. Not tried: there is no release yet.

## What was not verified

- An Ingress or a Gateway in front, TLS, and a multi-node cluster.
- A managed Postgres, `sslmode=require`, and an external registry.
- Upgrades, and whether an old replica works on the schema of the next version during the
  rolling update.
