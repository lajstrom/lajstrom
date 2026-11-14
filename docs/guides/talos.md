# Talos Linux from Lajstrom

A Talos Linux fleet needs four kinds of artifact from a registry:
- the installer image, built with the fleet's system extensions;
- the extension images;
- the Kubernetes, etcd, flannel and kubelet images that every node pulls;
- the boot assets: an ISO, a disk image, or a kernel and an initramfs for PXE.

This page puts each in a Lajstrom repo and points the nodes at it, for a self-hosted or
air-gapped network. It also says what is not there yet.

It comes from the spike [#340](https://github.com/lajstrom/lajstrom/issues/340). Each step says
whether it was measured or only read.

What was used, on 2026-11-12:
- Talos v1.14.5, with Kubernetes v1.37.1 and containerd v2.3.7 on the nodes;
- `talosctl` v1.14.5, crane 0.22.1, oras 1.3.6 and curl 8.7, on macOS with OrbStack;
- registry:3.1.2 as the backend;
- for the `oci` passthrough and `raw` repos, Lajstrom main (a7aa4d3e);
- for the `oci` proxy repos:
  - the head of the #442 stack with the #876 fixes (#881, 531e1f00);
  - for the boot with a token on 2026-11-13, the top of the blob-fix stack (`fix/875-oci-transit`,
    fc1a6be5).

  The proxy repos are not on main yet: [the oci proxy guide](../quickstart/oci-proxy.md).

| What | Repo | State |
| --- | --- | --- |
| Installer and extension images | `oci` passthrough, copied in | measured: digests unchanged (§2) |
| Images the nodes pull | `oci` proxy of ghcr.io and registry.k8s.io | measured, with a token: every image of the boot came through Lajstrom, and both nodes and coredns went `Ready` (§3) |
| The installer, fetched on demand | `oci` proxy of factory.talos.dev | measured: manifests and config blobs (§6.3) |
| ISO, disk image, PXE kernel and initramfs | `raw` | measured: `Range`, Basic after a `401`, anonymous read (§4) |
| `talosctl upgrade` from Lajstrom | | not run: needs qemu with KVM (§5) |
| Image Factory on Lajstrom | | read from its docs (§6) |

## 1. The repos and the nodes' credential

```yaml
auth: {anonymousRead: true}          # only so that talos-pxe below can be read without credentials
upstreams:                           # the proxies need the #442 stack
  - {name: ghcr, format: oci, url: "https://ghcr.io"}
  - {name: k8s, format: oci, url: "https://registry.k8s.io"}
  - {name: factory, format: oci, url: "https://factory.talos.dev"}
repos:
  - {name: talos, format: oci, registry: local}       # installers and extensions, copied in (§2)
  - {name: ghcr, format: oci, registry: local, kind: proxy, upstream: {name: ghcr}}
  - {name: k8s, format: oci, registry: local, kind: proxy, upstream: {name: k8s}}
  - {name: factory, format: oci, registry: local, kind: proxy, upstream: {name: factory}}
  - {name: talos-boot, format: raw, registry: local}  # ISO and disk images, with credentials
  - {name: talos-pxe, format: raw, registry: local, anonymousRead: true, policy: {blobRedirect: never}}
```

A Talos node sends one credential per registry host, and the nodes read several repos of it.
They get one deploy token over the mask `oci/*`, with `read` and no expiry
([auth.md](../auth.md#a-deploy-token-over-a-mask), #911). Only an admin makes one; on the
command line, the operator:

```sh
lajstrom admin token create --mask 'oci/*' --name talos-nodes --scopes read | jq -r .secret
```

The nodes log in with it as `deploy` (§3).

- **The mask is the reach.** `oci/*` covers `oci/ghcr`, `oci/k8s`, `oci/factory` and
  `oci/talos`, and every `oci` repo created later, from its creation on. On a server whose other
  `oci` repos the nodes must not read, name the nodes' repos so that one mask covers them alone
  (`*/talos` covers the repo `talos` of every format), or use an exact mask per repo.
- **It does not expire.** Nothing on the nodes needs rotating within a year. To replace it,
  make the next token, roll it out with `talosctl patch machineconfig`, then revoke the old one
  (`lajstrom admin token revoke <id>`, or `DELETE /api/v1/deploy-tokens/<id>`). Talos's docs list
  `.machine.registries` among the changes applied without a reboot. Whether new credentials take
  effect without one was not checked: the docs of Talos 1.0 said containerd needed a reboot.
- **It reads only.** It never pushes, deletes or manages, whatever its mask matches.

Before #911 the nodes had a user of their own, `talos-node`, with `reader` bindings on those
repos and a personal access token with `read`, which expires within 365 days on every node.
That still works, and it is the credential the boots of §3 measured. Since then, the token over
`oci/*` has been tested in `rbac.sh` with oras: it pulls from two `oci` repos, and a repo outside
its mask answers it `404`. No Talos boot has been run with it.

## 2. Installer and extension images in an `oci` passthrough repo

Since Talos 1.10, Image Factory builds installers from `ghcr.io/siderolabs/installer-base`
(its `docs/sources.md`), and `ghcr.io/siderolabs/installer:v1.14.5` does not exist
(`MANIFEST_UNKNOWN`). So installers come from Image Factory. The default `machine.install.image`
of 1.14.5 is
`factory.talos.dev/metal-installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.14.5`,
the empty schematic. The installer measured here is that of the schematic
`ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515`, which has
`siderolabs/qemu-guest-agent`. Its index has amd64 and arm64: Docker v2 manifests, each with two
layers of 31 and 117 MB, or 28 and 107 MB.

The extension images that a Talos version uses are listed in its catalog, in the `image-digests`
file of `ghcr.io/siderolabs/extensions:<v>`. `talosctl image talos-bundle v1.14.5` lists the
catalog, the imager and the installer base. `talosctl image k8s-bundle` lists the images the
nodes pull.

The paths below `oci/talos/` keep the upstream's, so that a mirror of `factory.talos.dev` or
`ghcr.io` with `overridePath` finds them (§3):

```sh
L=lajstrom.example.com
SCH=ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515
crane auth login $L -u ops --password-stdin < ops.pw
crane copy factory.talos.dev/metal-installer/$SCH:v1.14.5 $L/oci/talos/metal-installer/$SCH:v1.14.5
crane copy ghcr.io/siderolabs/qemu-guest-agent:11.1.1     $L/oci/talos/siderolabs/qemu-guest-agent:11.1.1
crane copy ghcr.io/siderolabs/extensions:v1.14.5          $L/oci/talos/siderolabs/extensions:v1.14.5
```

**Measured:**

| Image | Digest at the source and in Lajstrom | Copy |
| --- | --- | --- |
| `metal-installer/ce4c98…:v1.14.5` | `sha256:8a5109843e29…` | 11 s, 283 MB |
| `siderolabs/qemu-guest-agent:11.1.1` | `sha256:c396304665b7…` | 1 s |
| `siderolabs/extensions:v1.14.5` | `sha256:929b67d307c3…` | 1 s |

- `crane manifest` returns the same bytes from Lajstrom as from the source, for each index and
  for each platform's manifest. The index keeps the Docker v2 children and their
  `artifactType`.
- `crane validate --remote` passes for each platform. It reads every layer back through Lajstrom
  and checks its digest. The installer's 117 MB layer, read back with `crane blob`, has its
  digest.
- A second `crane copy` of the installer took 3 s and found the index in place: `existing
  manifest: v1.14.5@sha256:8a5109843e29…`.
- `oras copy -r` gives the same digest.

The tags of an `oci` repo are as mutable as the backend makes them
([oras.md](../quickstart/oras.md#2-push-pull-list-on-an-oci-repo)). So copying again after
Image Factory rebuilt an installer moves the tag, unlike a managed repo, where that is a `409`.

**Signatures.** Sidero's images and Image Factory's installers are signed with Sigstore bundles,
stored as OCI referrers (`application/vnd.dev.sigstore.bundle.v0.3+json`).
- `crane copy` does not copy referrers: after it, `oras discover` in Lajstrom lists none.
- `oras copy -r` does: Lajstrom then lists the bundle with the same digest as ghcr.io, and
  `GET /v2/oci/talos/siderolabs/qemu-guest-agent/referrers/sha256:c396…` returns it.

Copy with `oras copy -r` when anything verifies signatures, such as Image Factory reading from
Lajstrom (§6.1).

## 3. Nodes that pull only from Lajstrom

Each upstream registry gets a mirror, with `overridePath` for the `/v2/oci/<repo>` prefix and
`skipFallback`. A failing mirror then stalls the pull instead of going to the internet, which
is what makes the boot a proof.

`factory.talos.dev` goes to the passthrough repo of §2, so that `machine.install.image` and
`talosctl upgrade --image` keep the names of Image Factory. Use `oci/factory`, the proxy of
§6.3, to fetch installers on demand instead.

Since Talos 1.12, the documents look like this:

```yaml
apiVersion: v1alpha1
kind: RegistryMirrorConfig
name: ghcr.io
endpoints:
  - url: https://lajstrom.example.com/v2/oci/ghcr
    overridePath: true
skipFallback: true
---
apiVersion: v1alpha1
kind: RegistryMirrorConfig
name: registry.k8s.io
endpoints:
  - url: https://lajstrom.example.com/v2/oci/k8s
    overridePath: true
skipFallback: true
---
apiVersion: v1alpha1
kind: RegistryMirrorConfig
name: factory.talos.dev
endpoints:
  - url: https://lajstrom.example.com/v2/oci/talos
    overridePath: true
skipFallback: true
---
apiVersion: v1alpha1
kind: RegistryAuthConfig
name: lajstrom.example.com          # the host of the endpoints, with the port if they have one
username: deploy
password: ljs_dep_…                 # the token over oci/* of §1
```

`machine.registries` takes the same, and is what the boot of #340 used:

```yaml
machine:
  registries:
    mirrors:
      ghcr.io:
        endpoints: ["https://lajstrom.example.com/v2/oci/ghcr"]
        overridePath: true
        skipFallback: true
      registry.k8s.io:
        endpoints: ["https://lajstrom.example.com/v2/oci/k8s"]
        overridePath: true
        skipFallback: true
    config:
      lajstrom.example.com:
        auth: {username: deploy, password: ljs_dep_…}
```

**Requirements:**
- Give a `username` and a `password`, never an `identityToken` alone. containerd first posts
  the OAuth2 form to `/v2/token`, and Lajstrom answers `405`. containerd falls back to `GET`
  with Basic only when it has a username, so an `identityToken` pull fails
  ([#908](https://github.com/lajstrom/lajstrom/issues/908)). `auth` (base64 of
  `user:password`) is a username and a password too.
- The nodes must reach and trust the host of `server.externalURL`, since the token realm is
  there. For a private CA, add a `RegistryTLSConfig` with `ca`. Plain HTTP works only on the
  dev stack, with `server.insecureHTTP` and no `externalURL`. The realm then follows the `Host`
  of the request, which is how the docker provisioner below reaches `host.docker.internal`.

**Measured:**

- **Anonymous read:** a cluster booted with every image through Lajstrom: flannel and kubelet
  from ghcr.io; etcd, kube-apiserver, kube-controller-manager, kube-scheduler, kube-proxy,
  coredns and pause from registry.k8s.io. The setup was `talosctl cluster create docker`, one
  control plane and one worker, with the `machine.registries` mirrors above and
  `anonymousRead: true` repos, at the head of #869 (91a980a9), as reported on #340. That boot
  found [#876](https://github.com/lajstrom/lajstrom/issues/876).
- **With the token**, on 2026-11-13, at fc1a6be5, with a fresh index and a fresh registry:3
  backend. The repos had no anonymous read, and `skipFallback` was on. Two nodes booted with the
  commands below, the personal access token of `talos-node` (§1, before #911), and both
  patches. `machine.registries` covered the proxies,
  and the documents `factory.talos.dev`. Both patches also pass
  `talosctl validate --mode container` together.
  - **Every image of the boot came through Lajstrom with the token,** principal `talos-node`:
    etcd, kube-apiserver, kube-controller-manager, kube-scheduler, kube-proxy, coredns and pause
    from `oci/k8s`; kubelet and flannel from `oci/ghcr`. Both nodes pulled coredns, so the
    scheduler had placed its pods.
  - Anonymous requests got only the `401` challenge.
  - containerd's token flow, as #908 describes it, ran 32 times. Each time, a `POST /v2/token`
    got `405`, then a `GET` with Basic got `200`:

    ```text
    HEAD /v2/oci/k8s/etcd/manifests/v3.7.3  401  anonymous   Bearer realm="http://<Host>/v2/token"
    POST /v2/token                           405  anonymous   grant_type=password: containerd's first try
    GET  /v2/token                           200  user:…      Basic talos-node:<PAT>: containerd's fallback
    HEAD /v2/oci/k8s/etcd/manifests/v3.7.3  200  user:…
    ```

  - containerd abandoned its first pull of etcd mid-way. Two of its blob requests were logged as
    `500` with `context canceled` ([#877](https://github.com/lajstrom/lajstrom/issues/877)). It
    went on with `Range` (`206`) and the pull completed.
  - Talos reported the boot sequence done on both nodes. etcd and kubelet were healthy, and the
    control-plane static pods, kube-proxy and flannel were running.
  - **Not seen:** nodes and coredns `Ready`. About four minutes in, free disk on the shared
    machine fell under 4.5 GB, so the cluster was destroyed. `talosctl` was then still at
    `waiting for all k8s nodes to report`.
  - **`Ready`, on 2026-11-13 again**, at main (dcc6d21f), with the commands below, the
    `machine.registries` patch alone (the proxies and their credential), a fresh SQLite index
    and a fresh registry:3 backend:
    - `talosctl cluster create` returned after 1 minute 40 seconds: both nodes `Ready`
      (Kubernetes v1.37.1, containerd 2.3.7) and all nine pods `Running`, coredns among them;
    - 276 requests by `talos-node`, and no anonymous request past the `401` challenge; the
      token flow ran 31 times, `POST /v2/token` `405` then `GET` `200`, as above;
    - the nine images, from `oci/k8s` and `oci/ghcr` as above, each with its keep-tag in the
      backend;
    - `lajstrom_proxy_fetches_total`: `fetched` 53 (k8s 38, ghcr 15), `mounted` 38,
      `completed` 10, `transit` 8. One `HEAD` was `499`, a client that went away;
    - `~/.kube/config` and `~/.talos/config` had the same shasums before and after.
  - [#876](https://github.com/lajstrom/lajstrom/issues/876) is fixed at this commit:
    - all nine images got their keep-tag in the backend, kube-apiserver and kube-scheduler
      included. The nodes had asked for only 2 of kube-apiserver's blobs at its own path; the
      rest was mounted;
    - `lajstrom_proxy_fetches_total` read `fetched` 61 (k8s 46, ghcr 15), `mounted` 38,
      `transit` 5 and `shared` 2;
    - 63 blobs were fetched from the upstreams, none at more than one path.

The commands, with `secrets/patch-registries.yaml` and `secrets/patch-factory.yaml` holding the
snippets above, the endpoints at `http://host.docker.internal:<port>/v2/oci/<repo>`, and the
token:

```sh
export KUBECONFIG=$PWD/kubeconfig TALOSCONFIG=$PWD/talosconfig
talosctl cluster create docker --name ljs-talos --state $PWD/state --talosconfig-destination $PWD/talosconfig \
  --host-ip 127.0.0.1 --subnet 10.218.0.0/24 \
  --config-patch @secrets/patch-registries.yaml --config-patch @secrets/patch-factory.yaml
kubectl get nodes                                  # Ready
grep -c 'principal=user:<uid of talos-node>' lajstrom.log   # the runs above; with the token over oci/*: principal=token:<id>
talosctl -n 10.218.0.3 image pull --namespace system \
  factory.talos.dev/metal-installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.14.5
talosctl cluster destroy --name ljs-talos --state $PWD/state
```

The docker provisioner's own `ghcr.io/siderolabs/talos` image is pulled by the host's docker,
outside the mirrors.

## 4. Boot assets in `raw` repos

Image Factory serves each asset at `https://factory.talos.dev/image/<schematic>/<version>/<file>`.
The paths below keep that layout:

```sh
B=https://factory.talos.dev/image/$SCH/v1.14.5
for f in kernel-amd64 initramfs-amd64.xz metal-amd64.raw.zst metal-amd64.iso; do
  curl -fsSL -o "$f" "$B/$f"
  curl -u ops -T "$f" "https://$L/raw/talos-boot/$SCH/v1.14.5/$f"
done
```

**Measured**, one file on disk at a time, deleted once checked:

| File | Size | Upload to `raw/talos-boot` |
| --- | --- | --- |
| `kernel-amd64` (the PXE `vmlinuz`) | 19,255,296 | `201` |
| `initramfs-amd64.xz` | 97,353,518 | `201` |
| `metal-amd64.raw.zst` | 233,152,749 | `201` |
| `metal-amd64.iso` | 366,292,992 | `201` |

- For each file, `curl -r 0-1023`, `-r <middle>-<middle+99>` and `-r -512` answered `206` with
  the right `Content-Range`, and the bytes matched those of the file. A whole `GET` had the
  file's sha256.
- The `ETag` is the file's digest. The newest upload of a path is what `GET` serves, and
  `?digest=` pins a version ([raw.md](../quickstart/raw.md#4-upload-it-again-then-change-it-r-02-r-03)).
  A path such as `latest/metal-amd64.iso` can therefore move.
- The four files are far below the default `maxFileSize` of a raw repo, 8 GiB.

**iPXE.** iPXE sends no credentials until a `401` names Basic, then sends those of the URL. An
iPXE script in the repo can point at the files:

```text
#!ipxe
imgfree
kernel https://lajstrom.example.com/raw/talos-pxe/<schematic>/v1.14.5/kernel-amd64 talos.platform=metal console=tty0 slab_nomerge pti=on consoleblank=0 printk.devkmsg=on selinux=1 module.sig_enforce=1 proc_mem.force_override=never
initrd https://lajstrom.example.com/raw/talos-pxe/<schematic>/v1.14.5/initramfs-amd64.xz
boot
```

The kernel arguments are those of Image Factory's own script,
`https://pxe.factory.talos.dev/pxe/<schematic>/v1.14.5/metal-amd64`. For the private repo, the
URLs carry `https://pxe:<password>@…/raw/talos-boot/…`.

Measured with curl, with `--anyauth` for iPXE's order and `User-Agent: iPXE/1.21.1+`. A real
iPXE was not run: there was no VM.

| Request | Answer |
| --- | --- |
| `talos-boot`, no credentials | `401`, `WWW-Authenticate: Basic realm="lajstrom"` |
| `talos-boot`, `--anyauth -u pxe:…`, `pxe` being a `reader` of the repo | `401` then `200`, one request with Basic; kernel and initramfs with their sha256 |
| `talos-boot`, a wrong password | `401` |
| `talos-boot`, a `PUT` by `pxe` | `403` |
| `talos-pxe`, no credentials | `200`; kernel and initramfs with their sha256; `HEAD` `200` with `Accept-Ranges` and `ETag` |
| `talos-pxe`, a `PUT` without credentials | `401` |

`policy: {blobRedirect: never}` on the PXE repo keeps iPXE on Lajstrom. With `auto`, a backend
that hands out download URLs (distribution on S3) makes `GET` a `307` to the storage. iPXE
follows it, but over HTTPS it must trust that host's certificate, which a stock iPXE does not
for a private CA. This was not measured: registry:3 on a filesystem does not redirect.

## 5. Upgrade from Lajstrom

**Blocked here.**
- The docker provisioner cannot upgrade. Talos answers `upgrade is not supported in container
  mode` (`FailedPrecondition`), read in its `internal/app/lifecycle/lifecycle.go`.
- The qemu provisioner needs Linux with KVM, and this machine is macOS.

On a Linux host with KVM:

1. Copy the empty schematic's installer into `oci/talos` next to the one of §2:

   ```sh
   crane copy factory.talos.dev/metal-installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.14.5 \
     $L/oci/talos/metal-installer/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba:v1.14.5
   ```

2. Run Lajstrom on an address the VMs reach, the host side of the bridge being `10.5.0.1`. Use
   `secrets/patch-factory.yaml` of §3, with the endpoint `http://<host address>:<port>/v2/oci/talos`
   and the `RegistryAuthConfig` of `<host address>:<port>`.

3. Create the cluster, install, then upgrade:

   ```sh
   export TALOSCONFIG=$PWD/talosconfig KUBECONFIG=$PWD/kubeconfig
   sudo --preserve-env=HOME,TALOSCONFIG,KUBECONFIG talosctl cluster create qemu --name ljs-talos \
     --state $PWD/state --talosconfig-destination $PWD/talosconfig \
     --talos-version v1.14.5 --schematic-id 376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba \
     --presets iso --workers 0 --config-patch @secrets/patch-factory.yaml
   talosctl -n 10.5.0.2 get extensions        # none; the install pulled factory.talos.dev/metal-installer/3765…:v1.14.5 through Lajstrom
   talosctl -n 10.5.0.2 upgrade \
     --image factory.talos.dev/metal-installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.14.5
   talosctl -n 10.5.0.2 get extensions        # qemu-guest-agent 11.1.1
   sudo --preserve-env=HOME talosctl cluster destroy --name ljs-talos --state $PWD/state
   ```

   The `iso` preset boots from Image Factory's ISO. Only the install and the upgrade go through
   Lajstrom, which the access log shows: the manifests and layers of both installers under
   `/v2/oci/talos/metal-installer/…`, `principal=user:…` of `talos-node`. To upgrade across
   versions, create at an older `--talos-version` and copy the newer version's installer.

## 6. Image Factory

Read from Image Factory's docs and code (v1.8.0, `docs/configuration.md`, `docs/sources.md`,
`docs/air-gapped.md`). Nothing of it was run.

### 6.1 Reading from Lajstrom

`artifacts.core.registry` and `artifacts.core.namespace` say where Image Factory pulls its
sources: `siderolabs/imager`, `installer-base`, `extensions`, `overlays`, `talosctl-all`, and the
extension and overlay images the catalogs name. The namespace is prepended to every one of them,
so a passthrough repo fits:

```yaml
artifacts:
  core:
    registry: lajstrom.example.com
    namespace: oci/talos       # siderolabs/imager is pulled from lajstrom.example.com/oci/talos/siderolabs/imager
```

- Credentials come from the docker `config.json` (go-containerregistry's default keychain).
  go-containerregistry gets its token with `GET` and Basic, which Lajstrom serves, as crane did
  in §2.
- Image Factory verifies the cosign signature of every source. So copy with `oras copy -r`
  (§2). Keyless verification needs Sigstore online, so an air-gapped factory uses
  `containerSignature.publicKeyFile` or turns verification off.

### 6.2 Pushing to Lajstrom

Image Factory writes to three repositories, each set by `registry`, `namespace` and
`repository`:

- **`artifacts.installer.internal`:** the installers it builds. The repository is
  `<namespace>/<repository>/<image>/<schematic>`, and the tag is the version.
  - It pushes the index by digest, then signs it with a cosign signature under the tag
    `sha256-<digest>.sig`, then pushes the version tag.
  - When a cached installer's signature fails to verify, it builds again and pushes the tag
    again.
  - `artifacts.installer.external`, when set, is where it redirects clients by digest. It
    could be Lajstrom's URL of the same repository, so that nodes pull from Lajstrom.
- **`artifacts.schematic`:** the schematics.
- **`cache.oci`:** the boot assets it built, signed with `cache.signingKeyPath`.

An `oci` passthrough repo takes all of this:
- any manifest, by tag or by digest;
- referrers, with the backend's referrers API or the `sha256-<hex>` tag schema;
- `.sig` tags.

Two tags move: `sha256-<digest>.sig`, which each signing writes anew, and the version tag after a
rebuild. So the backend must let tags move. Zot with `capabilities.immutableTags: true`, Harbor
or Quay with tag immutability, and ECR `IMMUTABLE` repositories refuse the second push. A
managed repo (`raw`) refuses it too, by design.

### 6.3 Lajstrom as a cache of factory.talos.dev

The other direction was measured: an `oci` proxy repo whose upstream is
`https://factory.talos.dev`.
- `crane digest` through it gives `sha256:8a5109843e29…`, the factory's digest.
- Each platform's manifest came through, and each config blob with its digest.
- Image Factory serves manifests without a token challenge, and redirects blobs to
  `cdn.factory.talos.dev`, which the proxy followed.

No layer was fetched, to spare the disk. A first pull of the installer through it would build
the installer if the factory has not built it yet, and keep it in the backend.

## 7. What is missing

- Not run: a real iPXE (§4) and the upgrade (§5).
- [#908](https://github.com/lajstrom/lajstrom/issues/908): `POST /v2/token`. containerd wastes
  one round trip per token, and an `identityToken` alone fails.
- [#911](https://github.com/lajstrom/lajstrom/issues/911): a node credential over several repos
  that need not expire within a year is now a deploy token over a mask (§1). It was tested with
  oras in `rbac.sh`. No Talos boot has been run with it: the boots of §3 used a personal access
  token.
- [#876](https://github.com/lajstrom/lajstrom/issues/876), fixed in #880 and #881 on top of #869:
  images whose layers a node already held were never completed in the backend, and the control
  plane images share most of their layers. At fc1a6be5, all nine images of the boot were
  complete (§3).
- [#877](https://github.com/lajstrom/lajstrom/issues/877): a request that containerd abandons
  is logged as a `500` at ERROR. It happened twice during the boot.
- [#875](https://github.com/lajstrom/lajstrom/issues/875): nodes that pull a layer while
  another node's fetch of it runs give up after 30 s. A fleet booting at once does exactly
  that.
- [#886](https://github.com/lajstrom/lajstrom/issues/886): a prefetch of `oci` proxy repos
  (`lajstrom admin repo prefetch oci/<repo> --platform …`, quickstart/oci-proxy.md §5.1) warms
  the backend before a boot; it was not part of this run.
- [#883](https://github.com/lajstrom/lajstrom/issues/883),
  [#884](https://github.com/lajstrom/lajstrom/issues/884),
  [#885](https://github.com/lajstrom/lajstrom/issues/885) and
  [#888](https://github.com/lajstrom/lajstrom/issues/888): the proxy on a poor link to its
  upstream.
- **Sizes:** nothing found. The largest Talos artifact measured is the ISO, 366 MB. The largest
  layer is the installer's, 117 MB. Both are far below the defaults: `maxFileSize` 8 GiB for
  raw and 16 GiB for oci, and `server.maxManifestSize` 4 MiB.
- **Tags Talos expects to move:** none on the nodes' side. Talos and its configuration name
  images by version tag, and extensions by digest. Only Image Factory, as a pusher, moves tags
  (§6.2).
