# Quick start: a terraform repo

A terraform repo serves Terraform modules to `terraform init` and `tofu init` through Terraform's
module registry protocol. You put a module version in with `curl -T`, a `.tar.gz` of the
module's directory, and Terraform finds it by its address. Every version is an OCI artifact in
the registry behind Lajstrom; Lajstrom keeps the index, so `lajstrom reindex` can rebuild it
from the registry.

The repo is the namespace of the address: the module `greet/lajstrom` of the repo
`terraform/infra` is `<host>/infra/greet/lajstrom`. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §3 (terraform), the choices in
[ADR 0023](../adr/0023-terraform-modules-the-repo-is-the-namespace.md), and what lands in the
registry is the [manifest profile](../spec/manifest-profile.md).

**About the outputs on this page.** The commands are those that
[test/e2e/terraform.sh](../../test/e2e/terraform.sh) runs. The outputs show the shape of each
answer, not a captured run: your digests, tokens and times are your own.

## 1. Start the dev stack

You need Go, Docker, `curl`, `jq`, `tar` and `openssl`. Terraform and OpenTofu run in their
containers, so nothing of them is needed on the host.

```sh
make dev-up      # Zot, distribution and Postgres: docs/dev-stack.md
make run         # serves deploy/compose/lajstrom.example.yaml on 127.0.0.1:8080
```

In another terminal, once `make run` has logged `listening`, make the repo `terraform/infra` on the
Zot of the stack, and the first user, an admin:

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create terraform/infra --registry local --config $cfg
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config $cfg
u="admin:$pw"
```

A repo name is a namespace when it has no `.`: Terraform refuses an address like
`host/infra.modules/greet/lajstrom` before it asks anything. Lajstrom still serves such a repo
to `curl`, but not to Terraform.

A request without credentials is `401` unless the repo has `anonymousRead`.

## 2. Make a module archive

A module version is a gzip-compressed tar of the module's directory, its files at the root of the
archive:

```sh
mkdir -p greet
cat > greet/main.tf <<'EOF'
output "greeting" {
  value = "hello from 1.0.0"
}
EOF
COPYFILE_DISABLE=1 tar -czf greet-1.0.0.tar.gz -C greet .   # COPYFILE_DISABLE: no ._ files from macOS
```

Lajstrom reads the archive whole as it comes in. Its entries must stay inside it: no absolute
path, no `..`, links only to paths inside, and at least one regular file. A `.zip` is refused for
now; repack it as a `.tar.gz`.

## 3. Publish it

```sh
curl -sS -u "$u" -i -T greet-1.0.0.tar.gz http://localhost:8080/terraform/infra/greet/lajstrom/1.0.0
```

```text
HTTP/1.1 201 Created
Content-Type: application/json
Docker-Content-Digest: sha256:…
Location: /terraform/infra/greet/lajstrom/1.0.0/greet-lajstrom-1.0.0.tar.gz

{"namespace":"infra","name":"greet","system":"lajstrom","version":"1.0.0","filename":"greet-lajstrom-1.0.0.tar.gz","digest":"sha256:…","tag":"1.0.0"}
```

The path is `/terraform/<repo>/<name>/<system>/<version>`. The name is letters, digits, `-` and
`_`; the system, the last part of a module address (`aws`, `azurerm`), lowercase letters and
digits. The version is a SemVer version without a `v` and without build metadata: `1.2.0`,
`2.0.0-rc.1`. `v1.2.0` and `1.2.0+build.7` are `400`: Terraform asks for a version as its
version library prints it, and takes `1.0.0+a` and `1.0.0+b` for one version.

A version is immutable: the same bytes again are `201` with the same digest, other bytes `409`.
Publish a second version that says so:

```sh
sed -i.bak 's/1.0.0/1.1.0/' greet/main.tf
COPYFILE_DISABLE=1 tar -czf greet-1.1.0.tar.gz -C greet main.tf
curl -sS -u "$u" -T greet-1.1.0.tar.gz http://localhost:8080/terraform/infra/greet/lajstrom/1.1.0
```

## 4. What Terraform asks

Terraform starts with the service discovery of the host, which needs no credentials:

```sh
curl -s http://localhost:8080/.well-known/terraform.json
```

```json
{"modules.v1":"/terraform/"}
```

So the address `<host>/infra/greet/lajstrom` reads `/terraform/infra/greet/lajstrom/…`. The
versions of the module:

```sh
curl -s -u "$u" http://localhost:8080/terraform/infra/greet/lajstrom/versions
```

```json
{"modules":[{"versions":[{"version":"1.0.0"},{"version":"1.1.0"}]}]}
```

Terraform picks the newest version its constraint takes and asks where its archive is:

```sh
curl -s -u "$u" -i http://localhost:8080/terraform/infra/greet/lajstrom/1.1.0/download
```

```text
HTTP/1.1 204 No Content
X-Terraform-Get: /terraform/infra/greet/lajstrom/1.1.0/greet-lajstrom-1.1.0.tar.gz?token=eyJhbGciOi…
```

Terraform fetches the archive itself, with go-getter, which does not send the token of
Terraform's `credentials` block (only what `~/.netrc` has). So the path carries a token of its
own: it downloads this version and nothing else, for 20 minutes, and `/v2` takes nothing from
it. The archive route also takes your usual credentials, and serves a repo with `anonymousRead`
to anyone; the anonymous principal's download has no token.

```sh
loc=$(curl -s -u "$u" -o /dev/null -w '%header{x-terraform-get}' http://localhost:8080/terraform/infra/greet/lajstrom/1.1.0/download)
curl -s "http://localhost:8080$loc" | tar -tzf -     # no -u: the token is the credential
```

## 5. `terraform init`

Terraform reads a registry over `https://` only, and finds it at the host of the module address.
On the dev stack, put a TLS edge in front of `make run`: a certificate for the name
`lajstrom.test`, and nginx on the host's network, where `localhost:8080` is Lajstrom (Linux and
OrbStack):

```sh
mkdir -p edge tf
openssl req -x509 -newkey rsa:2048 -nodes -days 7 -subj /CN=lajstrom.test \
  -addext subjectAltName=DNS:lajstrom.test -keyout edge/key.pem -out edge/cert.pem
cat > edge/nginx.conf <<'EOF'
events {}
http {
  server {
    listen 127.0.0.1:8443 ssl;
    ssl_certificate /etc/nginx/edge/cert.pem;
    ssl_certificate_key /etc/nginx/edge/key.pem;
    client_max_body_size 0;
    location / {
      proxy_pass http://127.0.0.1:8080;
      proxy_set_header Host $http_host;
    }
  }
}
EOF
docker run -d --name lajstrom-edge --network host -v "$PWD/edge:/etc/nginx/edge:ro" nginx:1.30.6-alpine \
  nginx -c /etc/nginx/edge/nginx.conf -g 'daemon off;'
```

Terraform reads with a deploy token of the repo with `read`: it reaches this repo only and never
writes. Put it in a `credentials` block for the host, port included:

```sh
token=$(curl -s -u "$u" -H 'Content-Type: application/json' -d '{"name": "terraform", "scopes": ["read"]}' \
  http://localhost:8080/api/v1/repos/terraform/infra/deploy-tokens | jq -r .secret)
printf 'credentials "lajstrom.test:8443" {\n  token = "%s"\n}\n' "$token" > terraformrc
chmod 600 terraformrc
cat > tf/main.tf <<'EOF'
module "greet" {
  source  = "lajstrom.test:8443/infra/greet/lajstrom"
  version = "~> 1.0"
}

output "greeting" {
  value = module.greet.greeting
}
EOF
```

Run Terraform with the edge's certificate as its trusted roots:

```sh
tf() {
  docker run --rm --network host --add-host lajstrom.test:127.0.0.1 --user "$(id -u):$(id -g)" \
    -e HOME=/tmp -e SSL_CERT_FILE=/w/edge/cert.pem -e TF_CLI_CONFIG_FILE=/w/terraformrc \
    -v "$PWD:/w" -w /w/tf hashicorp/terraform:1.17.1 "$@"
}
tf init
tf apply -auto-approve
```

```text
Initializing the backend...
Initializing modules...
Downloading lajstrom.test:8443/infra/greet/lajstrom 1.1.0 for greet...
- greet in .terraform/modules/greet
…
Terraform has been successfully initialized!
…
Outputs:

greeting = "hello from 1.1.0"
```

On a real host the edge is your ingress or `server.tls` ([http-routing.md](../spec/http-routing.md)
§1), the address `art.example.com/infra/greet/lajstrom`, and the token a personal access token
or a deploy token, in `~/.terraformrc`:

```hcl
credentials "art.example.com" {
  token = "<token>"
}
```

or in the environment, `TF_TOKEN_art_example_com=<token>`. `terraform login` is not supported:
the discovery has no `login.v1`, and Terraform says the host does not support it.

## 6. OpenTofu

`tofu init` speaks the same protocol, with the same `credentials` block:

```sh
docker run --rm --network host --add-host lajstrom.test:127.0.0.1 --user "$(id -u):$(id -g)" \
  -e HOME=/tmp -e SSL_CERT_FILE=/w/edge/cert.pem -e TF_CLI_CONFIG_FILE=/w/terraformrc \
  -v "$PWD:/w" -w /w/tf ghcr.io/opentofu/opentofu:1.13.8 init
```

## 7. Delete a version

A version is deleted through the management API, the `/` of the module's name written `%2F`:

```sh
curl -s -u "$u" -o /dev/null -w '%{http_code}\n' -X DELETE \
  http://localhost:8080/api/v1/repos/terraform/infra/packages/greet%2Flajstrom/versions/1.1.0   # 204
tf init -upgrade && tf apply -auto-approve    # greeting = "hello from 1.0.0"
```

The versions document is rendered again without it. Once deleted, the version may be published
again. `lajstrom admin retention` deletes old versions in bulk ([operations.md](../operations.md#retention)).

## 8. With the FQDN mode

With `server.fqdn.domain` ([http-routing.md](../spec/http-routing.md) §1.1) the repo has a host of
its own, `infra.art.example.com`, whose `/.well-known/terraform.json` points at its root. The
address keeps the repo as its namespace, so only the host changes:
`infra.art.example.com/infra/greet/lajstrom`. Publish there as `PUT
https://infra.art.example.com/greet/lajstrom/1.2.0`.

## 9. OpenTofu without the registry protocol: `oci://`

OpenTofu 1.10 and later also installs a module straight from an OCI registry. That goes through
an `oci` repo and the OCI facade, and needs nothing of the terraform format: the module is a zip
of its directory, pushed with the artifact type OpenTofu expects
([its docs](https://opentofu.org/docs/cli/oci_registries/module-package/)):

```sh
bin/lajstrom admin repo create oci/modules --registry local --config $cfg
(cd greet && zip -r ../greet.zip .)
oras push --artifact-type application/vnd.opentofu.modulepkg art.example.com/oci/modules/greet:1.0.0 greet.zip:archive/zip
```

```hcl
module "greet" {
  source = "oci://art.example.com/oci/modules/greet?tag=1.0.0"
}
```

OpenTofu takes the registry's credentials from `oci_credentials` blocks of its CLI configuration
or from docker's (`docker login art.example.com`). Terraform has no such source. The e2e test
does not run this path.

## What is not there

The provider registry protocol (`providers.v1`) and a network mirror of providers; proxy repos of
registry.terraform.io and groups of terraform repos; `.zip` archives; `terraform login`.
