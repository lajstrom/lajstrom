# 0015. Declarative management: one owner per object

- Status: accepted
- Date: 2026-11-10
- Deciders: gecube

## Context

The control plane (ADR 0006) is changed in three ways today: the management API, and the UI on
top of it; `lajstrom admin`, straight against the database; and the `repos` section of the
configuration, which creates at every start the repos the database lacks and never changes one
(configuration.md: the database wins). Operators who keep everything else in git want repos,
groups and bindings there too: reviewed in a pull request, applied by CI or by Flux and Argo CD,
and later OIDC group mappings and retention rules. The problem is the second writer: git and the
API both changing one object.

The survey behind this record,
[declarative-management-2026-11.md](../design/declarative-management-2026-11.md), read how Grafana,
Harbor, Artifactory, Nexus, Keycloak, Argo CD, Flux, Kubernetes server-side apply, Backstage,
Crossplane and Terraform do it, with a source for every claim. What it found:

- **What fails is an edit that is allowed and later lost.** Grafana's `allowUiUpdates` and
  `editable`, keycloak-config-cli and the Grafana Operator let the UI change a provisioned object
  and lose the change at an unrelated moment: a file touched, a restart. Two designs work: refuse
  the edit (Harbor's `CONFIG_OVERWRITE_JSON`, Grafana alerting) or revert it continuously and
  visibly (Argo CD, Flux, Crossplane).
- **Grafana 12 rebuilt its model into this shape:** a manager kind and identity on every object,
  writes by anyone else refused, the source path and checksum kept as information, explicit
  takeover, release by an admin, and a choice between remove and release when a repository
  connection goes.
- **Prune must be scoped by ownership, never by absence.** The Nexus CasC plugin's
  `pruneRepositories` deletes every repo its file lacks, an empty Artifactory YAML deletes every
  repository, and keycloak-config-cli's safety switch turned off exactly when its state was empty
  (#1682). Flux deletes only what both its inventory and the object's owner label name.
- **Two sources that claim one object lose data** (Flux kustomize-controller #1724, #732), and so
  does a move between sources done as a delete and a create (Flux #338, Grafana #80055).
- **Adoption must be explicit:** a silent takeover (Grafana classic, Backstage) and an `already
  exists` with no way to import (the Harbor, Nexus and JFrog Terraform providers) are both standing
  complaints.
- **Create-only importers look declarative and are not** (Keycloak's realm import, the Artifactory
  bootstrap). Our `repos` section is one.
- **Secrets in declared state, and the environment interpolated into it, leak** (Terraform state,
  Keycloak placeholders). No control-plane object carries a secret today: ADR 0013 keeps upstream
  credentials in the configuration.

Lajstrom has two things the Kubernetes tools lack: the control plane is one small database, so a
source's whole set can be applied in one transaction; and no controllers co-write an object, so
ownership per object is enough where Kubernetes needs it per field. It must keep four things: the
operator alone decides where Lajstrom sends requests (ADR 0013); a user can always be revoked
(rbac.md §1); every change is audited; and the API's validators are the only judge of an object.
Bindings have no name today, only a server-assigned id that no file can refer to.

## Considered options

**Where ownership is recorded.** In an external state, as Terraform does: the server cannot
enforce what it does not know. Nowhere, the last writer winning, as Artifactory's YAML: the failure
above. On the object, in its control-plane row: chosen. With one database, the owner column is the
inventory too, so no separate list can disagree with it.

**What a write to a managed object gets.** Allowed, and overwritten at the next sync (Grafana
classic). Allowed, and reverted continuously (Argo CD, Flux): honest, but the user learns after the
fact, and it is built for controllers that co-write. Routed to git as a commit or a pull request
(Grafana Git Sync): a product of its own. Refused, naming the owner: chosen.

**Granularity.** One lock per kind (Harbor's environment variable): one repo in git would freeze
them all. Per field (Kubernetes' managed fields): needed only for co-writing controllers. Per
object: chosen, with the fields an operator flips during an incident moved out of the spec.

**Unit of apply.** Per object, eventually consistent, as the Kubernetes tools must: a group and its
members appear in some order, and sync waves exist to order them. A source's whole set in one
database transaction, ordered by dependency: chosen.

**An object of a revision in conflict.** The rest applies (Argo CD, Backstage), or the revision is
refused whole: chosen. A revision is what someone reviewed, and half of it is not.

**Prune.** By absence from the files: the failure above. Of the objects the source owns and has
applied, and no longer declares: chosen. On by default (Flux) or off (Argo CD): off for a watched
directory, and a choice of each source.

**The identity of a repo.** `metadata.name`, as Kubernetes has it (`npm.internal`): two spellings
of one name, and a repo name with `_` could not be a Kubernetes name anyway. `spec.format` and
`spec.name`, the API's names: chosen. The schema is Lajstrom's and not bound by Kubernetes' naming;
the operator, when it comes, maps a resource's `metadata.name`.

**Bindings.** Bare `(principal, role, mask)` tuples as their own keys: a file cannot group, name
or review them, and prune would work tuple by tuple. A named `RoleBinding` that expands to tuples:
chosen. A mode in which git is the only place access is granted (every API binding refused) is a
later configuration switch, not v1.

**Where sources are declared.** Through the API as well: an admin token could then make a new
owner of the control plane. In the configuration only, as ADR 0013 chose for upstreams: chosen.
Their state (suspended, revisions, status) is in the database.

**Operational toggles** (a proxy's `offline`, flipped during an outage of npmjs.org, without a pull
request). Owned per field: the model this record leaves out. Set at creation and not enforced
afterwards (Crossplane's `initProvider`): the file then lies about the live object. A state
sub-resource beside the spec, which no manifest sets: chosen.

**The answer of a refused write.** `403`, as Grafana answers: but the caller may well have the
permission, and the answer is about state. `409` with a code of its own, `MANAGED`, naming the
source: chosen. `409` already means a state conflict in the API, and the code tells it apart from
"exists" and "has versions".

**The `repos` section of the configuration.** Kept as a create-only seed beside the sources: two
declarative channels, one of them not declarative. Made a built-in source: point 14 says why not.
Deprecated in favour of a source: chosen.

## Decision

The survey's open questions (§8) were answered by the lead on 2026-11-10: the identity of a repo
(point 2), named RoleBindings and no git-only mode in v1 (point 10), sources in the configuration
only (point 5), a conflict refuses the whole revision (point 6), prune off by default for watched
directories (point 8), toggles in a state sub-resource (point 11), and `409 MANAGED` (point 4).
Accepted by the lead on 2026-11-10 with the three questions of its PR answered as proposed: a
RoleBinding's identity is `spec.name`, like every kind's key; a held prune (`Orphaned`, waiting
for confirmation or referenced by another owner's group) changes nothing, so the rest of the
revision applies, while a conflict or an invalid object still refuses it whole; and `offline`
moves to the state sub-resource before v0.1.0, so it never ships under `upstream`.

1. **Declarable kinds:** `Repo` (hosted, proxy and group, with its policy) and `RoleBinding`;
   later OIDC group mappings and retention rules. The configuration keeps the infrastructure:
   registries, upstreams, the database, keys, and the sources themselves. Users, passwords and
   tokens are never declared.

2. **One schema.** YAML, several documents to a file, or JSON: `apiVersion: lajstrom.io/v1alpha1`,
   `kind`, `metadata`, `spec`. An object's identity is its kind and its key, and the key is in
   `spec`: a Repo's `<format>/<name>`, a RoleBinding's `name`. `metadata` carries annotations to
   the engine and nothing else. The spec has the keys and values of the API's resource, checked by
   the API's validators, and is the object's whole declared spec: a key left out is its default,
   not "unchanged", so a key removed in git is removed from the object. An unknown key is an
   error. A manifest that changes a repo's `registry` or `kind` is invalid, as the `PATCH` is, and
   never becomes a delete and a create.

3. **One owner per object.** Every declarable object has exactly one owner: `{"kind": "api"}` (the
   API, the UI, `lajstrom admin`, the `repos` bootstrap) or `{"kind": "source", "name": "<name>"}`.
   The owner is a column of the object's row. The path of the file and the revision an object was
   applied from are kept beside it as information, never as identity. A source is a row that the
   configuration makes, so a typo cannot invent an owner.

4. **The owner alone changes the declared spec.** Any other write of it is `409 MANAGED`, on every
   write path: the check runs in the transaction of the write, in the one control-plane write path
   that the API, the UI, `lajstrom admin` and the bootstrap share. That is the `PATCH` and the
   `DELETE` of a managed repo, the `DELETE` of a binding that a RoleBinding grants, and the
   creation of an `api` object with a managed object's identity (a repo, or a tuple a RoleBinding
   grants).

   ```json
   {"error": {"code": "MANAGED",
     "message": "repo npm/all is managed by source ci (npm.yaml at 3f2c1a9): change it there, or have an admin release it",
     "managedBy": {"kind": "source", "name": "ci"},
     "source": {"path": "npm.yaml", "revision": "3f2c1a9"}}}
   ```

   Read-only is the declared spec and nothing else. Allowed on a managed repo, under the same RBAC
   as on any other: reads and publishes, reindex, the purge of a proxy's cache, deleting versions,
   its deploy tokens (objects of their own, owned by `api`), the rotation of its keys, and its
   state (point 11). Revocation is never blocked: disabling, deleting or renaming a user and
   revoking a token work whatever a source declares. A managed tuple of a disabled or deleted user
   grants nothing; it stays, `Unresolved`, until its manifest drops it. A refusal changes nothing
   and writes no audit row. There is no edit of a managed object by force: break-glass is to
   suspend the source and release the object (point 9).

5. **Sources are declared in the configuration:** `sources[]` with `name` (the owner's identity,
   stable and chosen), `kind` (`dir`, a directory or a file that `serve` reads; `push`, written by
   `lajstrom apply` alone; `k8s` later), `path` (of a `dir` source), `prune` (`false` by default)
   and `interval` (`1m` by default). The database has a row per source with its state: suspended or
   not, the revision applied and the one last attempted, the set last applied, its status, and the
   lease of the replica that runs it. A source that the configuration no longer names while it
   still owns objects is reported `SourceMissing` at every start; its objects stay owned and
   read-only until an admin removes it (point 9).

6. **One apply engine.** It takes a source, a revision and the source's whole set, and in one
   database transaction: parses the set, refusing unknown keys; validates each object with the
   API's validators; checks the set (two objects with one key; references to repos, to users, to
   the configuration's registries and upstreams; the owner of every object); plans (create, update,
   adopt, prune, orphan); orders the plan by dependency (hosted and proxy repos before the groups
   that name them, a group changed before its old members are pruned); applies it; writes the
   audit rows and the status; and moves `auth_version` once. A revision is all or nothing: one
   object that is invalid or owned by someone else refuses the whole revision, reported object by
   object, and the source stays at its last applied revision and says why. A `dir` source tries a
   refused revision again at every interval, since a release or a transfer may have unblocked it.
   The API's invariants hold for a revision as for a request: one that would leave no enabled
   user with an admin binding is refused. The applies of one source are serialised on its row.
   The engine acts with the authority of the source, an admin's, so the fields only the admin
   role changes (`group.members`, `upstream.name`) need nothing more.

7. **Three deliveries, one engine.**
   - **`lajstrom apply --source <name> -f <files or directories>`,** a client of
     `POST /api/v1/sources/<name>/apply`, with `--dry-run` (`?dryRun=true`), `--prune`, `--adopt`,
     `--confirm <kind>/<key>`, `--allow-prune-all`, `-o json`, and `--revision`, by default the
     commit of the files' git work tree. It takes the admin role, and its body has a bound of its
     own, 4 MiB, above the 64 KiB of the other endpoints. A real apply goes to a `push` source
     only; a dry run goes to any source, so that CI shows what a pull request would do to a `dir`
     source. The source is always named, never derived from a directory or a flag.
   - **`serve` reads a `dir` source.** Only the replica that holds the source's lease runs it (a
     heartbeat every 30 seconds, stale after 2 minutes, as `reindex_runs`). At every `interval` it
     reads a whole snapshot: the path resolved once (the `..data` link of a ConfigMap volume or of
     git-sync), every `.yaml`, `.yml` and `.json` file under it, names that start with `.`
     skipped. It never acts on the event of one file. A snapshot that cannot be read whole is
     `SourceUnreadable`, and nothing is applied. The revision of a `dir` source is the sha256 of
     its canonical set.
   - **A Kubernetes operator, later:** a `k8s` source whose custom resources have the same `spec`,
     which it sends to the same engine, a whole set or one object at a time, and whose status it
     copies back.

8. **Prune.** An object is a prune candidate when its source owns it, has applied it, and no longer
   declares it; nothing else ever is. An object transferred into a source is not a candidate
   before the source has applied it. A `dir` source prunes when its `prune` is `true`; a `push`
   source when its `prune` is `true` and the run says `--prune`, which is refused on a source
   whose `prune` is `false`. With prune off, a candidate stays, owned and read-only, and is
   reported `Orphaned`. The guards:
   - a revision that would prune every object the source has applied, an empty directory being
     the usual case, is refused unless `--allow-prune-all`, which a `dir` source cannot give: its
     admin runs `lajstrom admin source prune <name> --all`, or removes the source;
   - a repo that has versions, signing keys or deploy tokens is pruned only when it is named:
     `--confirm Repo/<format>/<name>`, or `lajstrom admin source prune <name>
     Repo/<format>/<name>`, which shows what goes and asks. Until then it stays `Orphaned` with
     the reason `PruneNeedsConfirmation`, and the rest of the revision applies: a held prune
     changes nothing, so it refuses nothing;
   - a repo that a group of another owner names is held the same way, `Referenced`, naming the
     groups.

9. **Ownership moves only by explicit verbs,** each planned before it acts (by the dry run of the
   revision that makes it, or by the verb's own `--dry-run`), audited, and for the admin role:
   - **adopt** (`api` to a source): a manifest object whose identity an `api` object has is a
     conflict, unless it carries the annotation `lajstrom.io/adopt: "true"` or the run says
     `--adopt`. The plan shows the owner's change and every field the adoption changes. A source
     never adopts another source's object: that is a transfer;
   - **release** (a source to `api`): `lajstrom admin source release <name> <kind>/<key>…`. The
     object keeps its values and becomes editable. While the source still declares it, the
     source's revisions are refused, naming it, until the manifest drops it or adopts it again;
   - **transfer** (one source to another): `lajstrom admin source transfer <from> <to>
     <kind>/<key>…`, one transaction that changes the owner and nothing else, never a delete and
     a create;
   - **remove a source** that the configuration no longer names: `lajstrom admin source delete
     <name> --release | --delete`, one of the two required. `--delete` keeps the guards of point 8;
   - **suspend and resume** a source: suspended, it neither applies nor corrects drift, and its
     objects stay owned and read-only.

   Each verb has its endpoint under `/api/v1/sources/<name>`, and `lajstrom admin source` runs it
   straight against the database, like the other admin commands, so that break-glass works with
   the server down.

10. **RoleBindings.** A named object where the API has bare tuples:

    ```yaml
    apiVersion: lajstrom.io/v1alpha1
    kind: RoleBinding
    spec:
      name: frontend-publishers
      role: writer
      repoMasks: [npm/internal, npm/all]   # write on the member, read on the group (rbac.md §7)
      subjects: [user:alice, user:bob]
    ```

    It expands to one binding per subject and mask, under the rules of rbac.md §3, each owned by
    the RoleBinding's owner. A subject names a user by its name or its uid and is resolved when its
    tuple is written; the tuple keeps the uid. A name that later resolves to another user, or to
    none, never moves the grant: the tuple is `Unresolved` until the manifest is fixed, and naming
    by uid avoids it. `group:<name>` comes with OIDC. Bindings made through the API stay bare
    tuples owned by `api`. An `api` tuple equal to a declared one is a conflict, resolved by
    adopting it into the RoleBinding: a grant that git does not show must not survive its removal
    from git. Two RoleBindings may grant one tuple, since both are in git. A RoleBinding released
    leaves its tuples to `api` and loses its name. `GET /api/v1/rolebindings[/<name>]` shows the
    RoleBindings with their status, and `/api/v1/bindings` shows each tuple's `roleBinding`.

11. **Operational state is not spec.** A field that an operator must be able to flip during an
    incident without a change in git, and that changes neither who may do what nor what an object
    is, moves to `PATCH /api/v1/repos/<format>/<name>/state`: `manage` on the repo, whoever owns
    it, audited as `repo.state`, never in a manifest, never compared for drift. The first, and
    today the only one, is a proxy's `offline`, which leaves `upstream`. Each new field of a
    declarable kind is classified by this rule in its own issue.

12. **No secrets, no interpolation.** No declarable kind has a field that holds a secret, a file
    path or the name of an environment variable, and since unknown keys are errors, `password`,
    `token` and their kind are refused. Values are literal: nothing is expanded, and a string that
    contains `${` is refused, so that a template left unrendered does not pass as a value.
    Templating belongs upstream (Kustomize, Helm, CUE). A future kind that needs a secret names
    one that the configuration declares, as a proxy names an upstream.

13. **Status, drift and audit.**
    - **Per object,** on its API resource: `managedBy`, `source` (`path`, `revision`, `appliedAt`)
      and `status`: `state` (`Synced`, `Drifted`, `Orphaned`, `Unresolved`), a reason, a message,
      the observed revision, and the time of the last transition.
    - **Per source,** `GET /api/v1/sources[/<name>]`: `ready` with a reason (`Applied`, `Refused`,
      `SourceUnreadable`, `Suspended`, `SourceMissing`), the applied and the attempted revision, the
      objects counted by state, and the refusals of the last attempt: per object, `Conflict` naming
      the owner or `Invalid` with the validator's message; for the revision, `WouldPruneAll`.
    - **Metrics,** labelled by the source's name from the configuration: `lajstrom_source_ready`,
      `lajstrom_source_objects{state}`, `lajstrom_source_last_applied_timestamp_seconds`,
      `lajstrom_source_apply_errors_total{reason}`.
    - **Drift** is rare once writes are refused, but a restore or a direct edit of the database
      makes it. At every `interval` the lease holder compares the source's set, read again or as
      last applied, with the live objects, also when the revision has not changed (never by a hash
      of the last apply), corrects what differs, and reports it.
    - **Audit:** a change made by a source is the change's usual row (`repo.create`,
      `binding.delete`, …) with `details.source` `apply:<name>`, the revision and the path. Its
      principal is the user who pushed, or `source:<name>` for a `dir` source and a correction of
      drift. Adopt, release, transfer, prune by an admin, suspend, resume and the removal of a
      source are rows of their own (`source.*`).
    - **The UI** shows the owner of every object with the path and the revision, disables the
      controls of a managed spec before anyone edits, keeps the operations of point 4 enabled,
      offers "Copy as manifest" on every object, and "Release" to admins.

14. **`lajstrom admin` and the `repos` bootstrap obey ownership.** `admin repo` and `admin binding`
    get the `MANAGED` refusal, with exit status 1; `admin user`, `admin token` and `admin key`
    are never blocked. The bootstrap stays create-only and makes `api` objects; an entry whose
    repo a source owns is skipped with an info line naming the source, in place of the warning
    about differences. `repos` is deprecated from the release that brings `dir` sources:
    `serve` warns while the list is not empty and names `lajstrom export --owner api --adopt` as
    the way out, and a later minor release before 1.0 removes it. It does not become a built-in
    source, because:
    - it would be a second schema for repos, flat configuration keys beside the manifests: one
      kind with two sets of rules, a failure the survey lists (§5, F11);
    - every replica reads its own copy of the file at start, so during a rolling update two
      replicas would apply two revisions of one source in turn, which the lease and the snapshot
      of a `dir` source exist to prevent;
    - it would turn "the database wins" into "the file wins" on every install that has the list:
      a repo created from it and changed through the API since would be overwritten at the next
      start unless adopted, and adopting is the migration anyway;
    - a `dir` source whose `path` is one file beside the configuration gives a quickstart the same
      convenience with one file more.

15. **Export.** `lajstrom export [--kind <kind>] [--owner api|<source>] [--adopt]` writes the
    declarable objects in this schema, an `api` binding as a RoleBinding of its own named from its
    id, for a reviewer to group. That is the control-plane export ADR 0006 promised, for the
    declarable part, and the format of its later snapshot into the registry. Users, tokens and keys
    are not configuration, and are backed up with the database. Backup, export, migration and
    declaration share one format, and the git repository of a source is a second backup of what it
    declares.

16. **Left out of v1.**

    | Left out | Why |
    | --- | --- |
    | Ownership per field | Kubernetes needs it for controllers that co-write an object, and Lajstrom has none. Toggles go to the state sub-resource. |
    | Editable but managed (Grafana's `allowUiUpdates`, `managerAllowsEdits`) | The edit lost at an unrelated moment: the failure the survey found everywhere. |
    | Write-back to git, a commit or a pull request from the UI | A product of its own: git credentials, branch protection, conflicts. "Copy as manifest" covers most of the need. |
    | Templating and interpolation | It reads the server's environment (Keycloak's warning) and is one more language; upstream tools do it. |
    | Implicit adoption of an object with the same identity | A silent takeover (Grafana classic, Backstage). |
    | Force-edit of a managed object | It undoes the model; break-glass is suspend and release. |
    | Sync waves and hooks | One transaction ordered by dependency needs neither. |
    | The operator | Later, as a `k8s` source. Identity in `spec` and an engine that also takes one object at a time keep its place. |

### An example

The configuration names the upstream and the sources:

```yaml
upstreams:
  - {name: npmjs, format: npm, url: "https://registry.npmjs.org/"}
sources:
  - name: platform
    kind: dir
    path: /etc/lajstrom/manifests   # on Kubernetes, a ConfigMap that Flux or Argo CD writes
    # prune: false, the default: a repo removed in git stays, Orphaned
  - name: ci
    kind: push                      # lajstrom apply --source ci, with an admin's token
    prune: true
```

`manifests/npm.yaml`, applied to the source `ci`: a hosted repo, a proxy of npmjs.org, and a group
of both, which the `repos` section created earlier and the source takes over. The RoleBinding of
point 10 is in the same directory.

```yaml
apiVersion: lajstrom.io/v1alpha1
kind: Repo
spec:
  format: npm
  name: internal
  registry: local
  policy: {maxFileSize: 104857600}
---
apiVersion: lajstrom.io/v1alpha1
kind: Repo
spec:
  format: npm
  name: npmjs
  registry: local
  kind: proxy
  upstream:
    name: npmjs           # upstreams[].name: the URL and any credentials stay in the configuration
    metadataTTL: 10m
    deny: ["@acme/*"]     # never ask npmjs.org for our own scope; offline is state, not here
---
apiVersion: lajstrom.io/v1alpha1
kind: Repo
metadata:
  annotations: {lajstrom.io/adopt: "true"}
spec:
  format: npm
  name: all
  kind: group
  group: {members: [internal, npmjs]}
```

### The dry run

```text
$ lajstrom apply --source ci -f manifests/ --prune --dry-run
source ci: revision 3f2c1a9, applied 9b07e44
  + Repo         npm/npmjs            create
  ~ Repo         npm/internal         update    policy.maxFileSize: null → 104857600
  ~ Repo         npm/all              adopt     owner: api → source:ci
                                                group.members: [internal] → [internal, npmjs]
  ~ RoleBinding  frontend-publishers  update    subjects: [user:alice] → [user:alice, user:bob]
  - RoleBinding  legacy-readers       prune     user:eve reader raw/*, user:eve reader npm/*
  = Repo         apt/old              orphan    PruneNeedsConfirmation: 1234 versions, 1 signing key
  ! Repo         pypi/libs            conflict  managed by source platform (pypi.yaml at 77ab0e2)
Plan: 1 create, 2 update, 1 adopt, 1 prune, 1 orphan. Refused: 1 conflict.
```

The exit status is `terraform plan -detailed-exitcode`'s: `0` nothing would change, `2` changes
would apply, `1` an error or a refusal, as here. A real apply exits `0` when the revision applied
and `1` when it was refused. `-o json` is the same plan for CI and the UI, with the field paths of
the validators' messages, so that one vocabulary covers errors and diffs; a real apply answers the
same document with `"dryRun": false`:

```json
{"source": "ci", "revision": "3f2c1a9", "appliedRevision": "9b07e44",
 "dryRun": true, "result": "refused",
 "changes": [
   {"action": "create", "kind": "Repo", "key": "npm/npmjs", "path": "npm.yaml"},
   {"action": "adopt", "kind": "Repo", "key": "npm/all", "path": "npm.yaml",
    "owner": {"from": {"kind": "api"}, "to": {"kind": "source", "name": "ci"}},
    "fields": [{"field": "group.members", "from": ["internal"], "to": ["internal", "npmjs"]}]},
   {"action": "orphan", "kind": "Repo", "key": "apt/old", "reason": "PruneNeedsConfirmation",
    "versions": 1234, "signingKeys": 1, "deployTokens": 0}],
 "refused": [
   {"kind": "Repo", "key": "pypi/libs", "path": "pypi.yaml", "reason": "Conflict",
    "owner": {"kind": "source", "name": "platform"},
    "message": "repo pypi/libs is managed by source platform (pypi.yaml at 77ab0e2)"}]}
```

`result` is `unchanged`, `changes` or `refused` for a dry run, and `applied` or `refused` for an
apply; the other changes of the plan are elided here.

## Consequences

- Repos, groups and bindings can live in git, be reviewed in a pull request and be applied by CI,
  Flux or Argo CD, with a plan before and a status after. An edit through the UI or the API is
  never lost to git later, because it is refused at once and says where to make it.
- A group and its members change in one transaction: there is no moment in which a group names a
  member that does not exist yet, and no sync waves to write.
- A migration is one reviewed change: `lajstrom export --owner api --adopt`, then a pull request.
- Break-glass on a managed object takes two admin steps, suspend and release, and a fix in git
  afterwards, and whoever is on call must know them. The state sub-resource covers the one case
  known today (`offline`); a new urgent toggle needs the rule of point 11 applied to it.
- All or nothing means one bad object holds back every other change of its source until it is
  fixed. Sources should be cut along what one team reviews; several sources are the remedy, not
  partial applies.
- After a release or a transfer, the old source's revisions are refused until its manifest drops
  the object, so a move takes a change in git in step with the verb.
- A change of the lease's holder during a ConfigMap update can apply the previous revision once
  more, until the new holder's volume catches up (the kubelet's sync, about a minute); the audit
  shows both applies. A content hash has no order by which to refuse the older one.
- Whoever can write the files under a `dir` source's path, or holds the admin token that a `push`
  source's CI uses, can grant access: both are as sensitive as the configuration. The threat
  model gets a review of these two surfaces when the watcher lands.
- `repos` is deprecated: every install that uses it migrates before the release that removes it,
  and the quickstarts and the Compose example move to a `dir` source.
- `offline` leaves `upstream` for the state sub-resource before v0.1.0, so no release carries it
  under `upstream`: a change of api.md, configuration.md and proxy-and-group.md §2.2.
- `lajstrom.io/v1alpha1` is a new contract: it changes under the same rule until it is `v1`. The
  owner columns, the `sources` table, the named RoleBindings, the `MANAGED` code, the new members
  of the error envelope and of the resources, and the endpoints of `/api/v1/sources` change
  index-schema.md, api.md, rbac.md and configuration.md, each in its issue under `contract`.
- Follow-up issues, in this order:
  1. **Schema, API and migration of ownership:** the owner columns on repos and bindings, the
     `sources` and RoleBinding tables in both dialects, `sources[]` in the configuration, the
     enforcement in the one write path for the API, `lajstrom admin` and the bootstrap, `409
     MANAGED`, `managedBy`, `source` and `status` on the resources, the state sub-resource with
     `offline`, and a normative spec of the manifest schema.
  2. **The apply engine:** validation by the API's validators, the set checks, the plan, the
     order, one transaction, prune and its guards, adoption, status and audit, the drift check,
     and the ownership verbs with their endpoints and `lajstrom admin source`.
  3. **`lajstrom apply`:** the client, the dry run in text and JSON, the exit statuses, the
     revision from git, and an offline check of the schema with a published JSON Schema.
  4. **The directory watcher:** `dir` sources in `serve`, the lease, snapshot reads, the
     metrics, and the deprecation warning of `repos`.
  5. **Export:** `lajstrom export` in this schema, the control-plane export of ADR 0006.
  6. **The UI's display:** the owner on every object, controls disabled before an edit, "Copy as
     manifest", "Release", and a page per source.
