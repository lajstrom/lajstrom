<!-- The survey behind ADR 0015, made on 2026-11-10. -->
# Declarative ("GitOps") management of the Lajstrom control plane: how comparable products do it

- Date: 2026-11-10
- Purpose: input for ADR 0015. In scope: repos (hosted, proxy, group, policies) and bindings now,
  and later OIDC groups and retention rules, declared in files. Registries, upstreams, the
  database and keys stay in the process's config file.
- Method: product documentation first. Where the docs say nothing, I read the source code
  (Grafana, Flux, keycloak-config-cli) and the issue trackers. Every claim links to its source.
  "(inference)" marks a conclusion I drew myself where no source states it outright.
- Lajstrom context used: `docs/architecture.md`, ADR 0006, ADR 0007, ADR 0013,
  `docs/configuration.md` (§repos, the create-only bootstrap), `docs/api.md` (repos, bindings,
  error envelope, audit `source`), `docs/spec/proxy-and-group.md` §2.

---

## 0. Summary

### Key findings

1. **Grafana has already rebuilt its own model into your draft rule.** Classic file provisioning
   is the hybrid the lead fears: three resource types with three different rules, and switches
   (`allowUiUpdates`, `editable`) that allow UI edits without moving ownership. Since v12,
   Grafana's unified storage puts the owner on every object: `grafana.app/managedBy` (kind),
   `grafana.app/managerId` (identity), `managerAllowsEdits`, `managerSuspended`, and
   `sourcePath`/`sourceChecksum`/`sourceTimestamp`. The server refuses to change the manager
   kind or identity in place ("remove the existing manager first, then add the new one"). Writes to
   repo-managed objects are refused or routed to git. A sync refuses to overwrite an unmanaged
   object unless it is on an explicit takeover allowlist. An admin can release an object.
   Deleting a repository connection makes you choose between *remove* and *release* for its
   objects. This is the best evidence that the draft is the right shape: the product that
   suffered most from the problem converged on it.
2. **What fails is a UI edit that is allowed and then silently loses to the file at some
   unrelated moment.** Examples: Grafana dashboards with `allowUiUpdates` (the edit is kept until
   the file's checksum changes), Grafana datasources with `editable: true` (overwritten at
   restart, grafana#75722), keycloak-config-cli (the import is skipped while the file checksum
   is unchanged, so UI drift survives), and the Grafana Operator (a hash shortcut skipped the
   live comparison, grafana-operator#2365). Two designs work. One refuses the edit: Harbor's
   `CONFIG_OVERWRITE_JSON`, Grafana alerting provenance, Grafana 12. The other reverts it
   continuously and visibly: Argo CD self-heal, Flux drift correction, Crossplane. Lajstrom has
   no Kubernetes-style co-writing controllers, so refusing is the simpler and more honest choice.
3. **Prune must be scoped by ownership, never by "not in the file".** Examples of unscoped prune:
   the Nexus CasC plugin's `pruneRepositories: true` deletes every repo absent from the file,
   Artifactory warns that an empty YAML deletes all repositories, and kubectl's allowlist
   `--prune` warns about deleting objects unexpectedly. keycloak-config-cli's remote state
   protected nothing in exactly the case where it mattered: an empty state deleted every
   manually created group (#1682). Flux has the safe shape. Its delete requires both the source's
   inventory entry and the owner label on the live object (`fluxcd/pkg/ssa` `DeleteAll` with
   `Inclusions`). Kubernetes ApplySets make "each object belongs to at most one set" a rule.
4. **Silent co-ownership between two sources causes data loss in practice.** In Flux
   kustomize-controller#1724 and #732, two Kustomizations relabel the same object on every
   reconcile, and whether a namespace survived "depended only on which Kustomization applied
   last". Argo CD has `SharedResourceWarning` and `FailOnSharedResource` for the same reason.
   Moving an object between sources must not delete and recreate it (Flux #338; Grafana #80055,
   where identity was keyed on the file path).
5. **Every mature system makes adoption and release explicit.** Crossplane observes first and
   then switches `managementPolicies` to `*`. Terraform has `import` and `removed { lifecycle {
   destroy = false } }`. Grafana has the takeover allowlist and the remove/release finalizers.
   Flux has `deletionPolicy: Orphan`. Implicit adoption causes the opposite complaints:
   "already exists" errors in the Harbor, Nexus and JFrog providers, silent overwrite in Grafana
   ("You can overwrite existing dashboards with provisioning"), and Backstage's rule that an
   unkeyed entity is taken over by the next location.
6. **Create-only importers look declarative and are not.** Keycloak `--import-realm`, the
   `KeycloakRealmImport` CR and the Artifactory bootstrap YAML all create once and ignore every
   later change. Lajstrom's `repos:` bootstrap is the same kind of thing. ADR 0015 has to say how
   it relates to the new mechanism, or users will see two declarative channels.
7. **Secrets:** systems that put secrets into declared state regret it. Terraform state stores
   them in cleartext (the Vault provider's own warning), and the Harbor and JFrog providers are
   adding write-only arguments. Systems that interpolate the environment into manifests leak it:
   Keycloak warns that placeholders give "access to all environment variables even sensitive
   ones". Lajstrom's control-plane objects carry no secrets today (ADR 0013 keeps upstream
   credentials in the config). Keep it that way by schema.
8. **Reporting:** the systems people trust report per-object and per-source conditions:
   Flux `Ready`/`Reconciling`/`Stalled` with reasons, Crossplane `Synced`/`Ready`, Backstage
   per-entity errors and orphan markers, Argo CD `OutOfSync`/`SharedResourceWarning`. Systems that
   only log (Grafana provisioning, keycloak-config-cli) generate "why did my change not apply"
   issues.

### The draft rule in one line each

| Clause | Verdict |
| --- | --- |
| Every object has exactly one owner (`api` or `gitops:<source>`) | **Right.** Record kind and identity separately, as Grafana does. Make sources first-class named objects. `gitops:` is a misnomer for `lajstrom apply` from CI or for the operator, so call them `source:<name>`. |
| GitOps-owned objects are read-only through the API and UI (409 naming the source) | **Right**, with carve-outs. Operations (reindex, cache purge, version delete, key rotation, deploy tokens) stay allowed. Revocation (disabling or deleting a user, revoking tokens) is never blocked. `lajstrom admin` (which goes straight to the database) must enforce the rule too. Add a distinct error code. |
| Prune only touches the owner's own objects | **Right.** Add an empty-set guard, a content guard for repos (versions, signing keys), and a visible `Orphaned` state when prune is off. |
| Ownership changes only by an explicit command | **Right.** Define the three verbs (adopt, release, transfer) and source removal (release or delete). All of them must be plannable in the diff and audited. |
| One manifest schema, three delivery mechanisms | **Right.** Add one server-side apply engine that all three call: full-set semantics per source, applied in one database transaction. |
| (missing) | Sources registry; suspend; per-object and per-source status; dry-run diff format and exit codes; ordering inside the transaction; names for bindings; operational toggles separated from declared spec; secrets forbidden in manifests; relationship to the config bootstrap and the ADR 0006 export. |

---

## 1. Vocabulary used below

- **Owner (manager):** the single party allowed to change an object's declared spec. In Lajstrom
  that is `api` (UI, REST, `lajstrom admin`, the config bootstrap) or a **source**.
- **Source:** a named producer of desired state: a directory `serve` watches, a push target for
  `lajstrom apply`, or later a Kubernetes namespace that an operator mirrors.
- **Inventory:** the set of objects a source applied at its last revision.
- **Drift:** live state differs from what the owner declared.
- **Prune:** deleting objects the owner no longer declares. **Orphan:** an owned object its
  source no longer declares and that was not pruned.
- **Adopt:** move an object from `api` to a source. **Release:** move it from a source to `api`.
  **Transfer:** move it from one source to another.
- **Suspend:** a source stops reconciling and keeps its ownership.

---

## 2. Products

### 2.1 Grafana

Grafana is the cautionary tale and also the clearest positive example, because it changed its
model.

#### 2.1.1 Classic file provisioning (dashboards, datasources, alerting)

**Model.** YAML provider files under `provisioning/` are read at startup and then polled
([provisioning docs](https://grafana.com/docs/grafana/latest/administration/provisioning/)).
Each resource type has its own ownership record:

- Dashboards: a `dashboard_provisioning` row keyed by the **file path** that holds the file's md5
  checksum. `upToDate = jsonFile.checkSum == provisionedData.CheckSum`
  ([file_reader.go](https://github.com/grafana/grafana/blob/main/pkg/services/provisioning/dashboards/file_reader.go)).
- Datasources: matched by name, with an `editable` flag and a `version` that is "used to compare
  versions when updating". With several instances, "Grafana only updates data sources with the
  same or lower version number".
- Alerting: a **provenance** value per resource (`file`, `api`)
  ([HTTP API provisioning](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/http-api-provisioning/)).

**Conflicts with the UI.** There are three different rules:

- Dashboards with `allowUiUpdates: false`: "you can't save changes to a provisioned dashboard".
  The user edits first and only learns at save time that the save is blocked (the "Cannot save
  provisioned dashboard" dialog). With `allowUiUpdates: true`, "Grafana persists the changes to
  its database", but "its not possible to automatically save the changes back to the
  provisioning source", and when the source changes "Grafana always overwrites the database
  dashboard with the one from the provisioning file". Because the check is a checksum of the
  file, a UI edit survives restarts until somebody touches the file, and then it disappears
  whole (inference from `file_reader.go`).
- Datasources with `editable: true` can be edited in the UI and are reconfigured to match the
  file at every startup. Users report it as a bug: "All the changes made through the UI are
  gone" ([grafana#75722](https://github.com/grafana/grafana/issues/75722)).
- Alerting: "You cannot edit provisioned resources from files in the Grafana UI"
  ([file provisioning](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/file-provisioning/)).
  Resources provisioned through the API are locked too unless the writer sends
  `X-Disable-Provenance: true` ("Allows editing of provisioned resources in the Grafana UI").
  The UI labels them "Provisioned"
  ([alerting provisioning](https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/)).

**Prune and deletion.** There are three rules here too:

- Dashboards: a file that is gone means "Grafana deletes the dashboard in the database unless
  you have set the option `disableDeletion`". With `disableDeletion` the dashboard is
  *unprovisioned* (released) instead (`UnprovisionDashboard` in `file_reader.go`).
- Datasources: an explicit `deleteDatasources` list, or `prune: true`, which removes provisioned
  datasources dropped from the file.
- Alerting: explicit `deleteRules`, `deleteContactPoints` and similar lists. The notification
  policy tree is one object that provisioning "will overwrite".

**Secrets.** `secureJsonData` and `secure_settings` are encrypted in the database. Values come
from `$ENV_VAR` interpolation, which "you can use … in all provisioning configuration". A literal
`$` needs escaping as `$$`.

**Drift.** Nothing compares the live object with the file. Dashboards compare file checksums,
datasources are re-applied at start, and nothing is reported beyond logs.

**Complaints.**

- [#80055](https://github.com/grafana/grafana/issues/80055): identity is keyed by the resolved
  file path. A `git-sync` symlink swap makes Grafana "delete … and re-create all the dashboards",
  causing 404s and losing stars.
- [#137468](https://github.com/grafana/grafana/issues/137468) (opened today): a provisioned
  dashboard cannot be deleted directly, but deleting its parent folder deletes it, and it does not
  show up in "Recently deleted". The read-only rule had a hole in one write path.
- [#109229](https://github.com/grafana/grafana/issues/109229): in 12.1, formerly provisioned
  dashboards remain after the config is removed (regression).
- [#111525](https://github.com/grafana/grafana/issues/111525) and
  [#114281](https://github.com/grafana/grafana/issues/114281): `allowUiUpdates` stopped working
  in 12.1.2 and 12.2. The semantics were never a written contract, so they drifted between
  versions.
- [#13823](https://github.com/grafana/grafana/issues/13823) (2018): "The user can edit the
  dashboard in the browser but cannot save it. To update the dashboard the user has to copy the
  edited JSON to the files themselves". This is the original UX complaint, and it led to Git Sync.

#### 2.1.2 Grafana 12+: managed resources and Git Sync (the fixed model)

Unified storage records a **manager** on every object
([manager.go](https://github.com/grafana/grafana/blob/main/pkg/apimachinery/utils/manager.go),
[meta.go](https://github.com/grafana/grafana/blob/main/pkg/apimachinery/utils/meta.go)):

```text
grafana.app/managedBy           kind: repo | terraform | kubectl | plugin | grafana | file-provisioning | classic-*
grafana.app/managerId           identity of the manager instance (e.g. the repository name)
grafana.app/managerAllowsEdits  "true" lets others edit
grafana.app/managerSuspended    "true": the manager skips updates
grafana.app/sourcePath | sourceChecksum | sourceTimestamp   where it came from (separate from who owns it)
```

The server enforces it
([managed.go](https://github.com/grafana/grafana/blob/main/pkg/storage/unified/apistore/managed.go),
[ownership.go](https://github.com/grafana/grafana/blob/main/pkg/registry/apis/provisioning/resources/ownership.go)):

- A repo-managed object can be written only by the provisioning service identity. Otherwise the
  error is "this resource is managed by a repository", and the write is *routed* to the
  repository (a commit, or a pull request when the branch is protected; see the
  [Git Sync permissions page](https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/permissions-grafana/)).
- Changing the manager kind or identity in place is `403`: "Cannot change resource manager kind
  … remove the existing manager first, then add the new one".
- Removing the manager needs authority over the old manager. As an exception, admins may release
  repo-managed objects, because "There is no reconciliation loop that automatically releases
  orphaned resources, so admins need the ability to detach manually".
- A sync that meets an existing **unmanaged** object of the same name fails with a conflict,
  unless the object is on a `TakeoverAllowlist`, which is built only during migration. This is
  explicit adoption.
- An object inside a repo-managed folder must have the same manager as the folder
  (`ensureSameRepoManager`): containment cannot cross owners.
- Deleting a repository connection carries a finalizer choice:
  [`remove-orphan-resources`](https://github.com/grafana/grafana/blob/main/apps/provisioning/pkg/repository/finalizers.go)
  ("removes everything this repo created") or `release-orphan-resources` ("removes the metadata").
- One wart: Terraform first used the HTTP User-Agent as the manager identity, which "Changes
  with every Terraform/provider version update". Grafana now carries a special-case migration
  hack. Lesson: a manager identity must be a stable name the user chooses.
- Permissions are **not** synced to git: "Folder and dashboard permissions are managed in Grafana
  only". Grafana's Terraform provider offers both an authoritative
  [`grafana_folder_permission`](https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/folder_permission)
  ("any permission that isn't listed is removed") and an additive
  [`grafana_folder_permission_item`](https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/folder_permission_item).
  The authoritative-or-additive choice matters for Lajstrom bindings (§7.9).

#### 2.1.3 Grafana Operator

- **Model:** a CR per dashboard or datasource. The operator polls Grafana because Grafana has no
  change notifications.
- **Drift:** with `resyncPeriod` (default `10m`), the operator re-applies the manifest and
  overwrites UI edits. `0m` disables polling. `suspend: true` "will ignore any changes"
  ([common options](https://grafana.github.io/grafana-operator/docs/examples/common_options)).
  The recommended workflow concedes the UX problem: "duplicate the dashboard … work on the copy …
  export the changes and update the manifest". Users asked for this to be documented because the
  operator "is overriding changes made to dashboards using grafana ui"
  ([grafana-operator#1597](https://github.com/grafana/grafana-operator/issues/1597)).
- **Bug class:** the reconciler returned early when the CR's hash was unchanged, so live drift
  in Grafana was never checked ([#2365](https://github.com/grafana/grafana-operator/issues/2365)).
  Non-canonical JSON hashing caused spurious updates in the opposite direction
  ([#2953](https://github.com/grafana/grafana-operator/issues/2953)). Lesson: detect drift as
  desired vs live, never as desired vs last-applied hash, and hash canonical forms.
- **Deletion:** finalizers delete the object from Grafana when the CR is deleted. If the
  operator is down, the CRs cannot be deleted.
- **Secrets:** taken from Kubernetes Secrets (`valuesFrom`). `allowCrossNamespaceImport` is
  one-way mutable, "to avoid exposing possibly sensitive resources by accident".

#### 2.1.4 Grafana Terraform provider

The usual Terraform model: the state file is the ownership record and `terraform plan` shows
drift. `grafana_dashboard.overwrite` lets Terraform overwrite "existing dashboard with newer
version, same dashboard title in folder or same dashboard uid"
([docs](https://github.com/grafana/terraform-provider-grafana/blob/main/docs/resources/dashboard.md)),
which is opt-in silent adoption. In unified storage, Terraform-written objects are marked
`managedBy: terraform`.

#### 2.1.5 Why classic Grafana is the "worst hybrid", in one list

1. Edits are allowed without an ownership change (`allowUiUpdates`, `editable`), so who wins
   depends on an unrelated event: a file touch or a restart.
2. Each resource type has its own owner record, its own lock and its own delete semantics.
3. Identity is the file path, not the object name.
4. Provisioning silently overwrites an existing UI object with the same uid or title.
5. Not every write path checks the lock (folder delete).
6. The semantics are not a contract, so they regress between versions.
7. Users discover the lock at save time, not when they open the editor.

For unified-storage resources, Grafana 12 removed points 2 to 4. It turned point 1 into an
explicit, recorded property (`managerAllowsEdits`; classic `allowUiUpdates` maps onto it in
`dashboard_service.go`) rather than a provider switch. Lajstrom can start from the fixed model
and leave out `allowsEdits`.

### 2.2 Harbor

- **Terraform provider**
  ([goharbor/terraform-provider-harbor](https://github.com/goharbor/terraform-provider-harbor)).
  - Declarable: `project`, `project_member_user` and `project_member_group`, `robot_account`,
    `registry` (replication endpoints), `replication`, `retention_policy`, `immutable_tag_rule`,
    `label`, `user`, `group`, `config_auth`, `config_system`, `config_security`,
    `garbage_collection`, `purge_audit_log`, `interrogation_services`, `preheat_instance`,
    `project_webhook`, `tasks`.
  - Not declarable: artifacts, scan results, and anything about content.
  - Ownership: the Terraform state. Harbor knows nothing about it, and the UI can edit anything.
  - Drift: only what the provider re-reads. In
    [#288](https://github.com/goharbor/terraform-provider-harbor/issues/288), robot permissions
    changed in the UI were not detected. In
    [#317](https://github.com/goharbor/terraform-provider-harbor/issues/317),
    `harbor_config_system` "doesn't detect drift".
  - Adoption: a 409 when the object exists. See
    [#39](https://github.com/goharbor/terraform-provider-harbor/issues/39) ("if project already
    exist (and not created with terraform) => error") and
    [#603](https://github.com/goharbor/terraform-provider-harbor/issues/603), which asks to
    "adopt it into state rather than failing".
  - Secrets: robot secrets and user passwords sit in state; write-only arguments were requested
    ([#572](https://github.com/goharbor/terraform-provider-harbor/issues/572)).
- **`CONFIG_OVERWRITE_JSON`** (Harbor ≥ 2.3,
  [docs source](https://github.com/goharbor/website/blob/main/docs/install-config/configure-system-settings-cli.md)):
  "Once the `CONFIG_OVERWRITE_JSON` variable is set, you can only update or remove the
  configuration by updating the `CONFIG_OVERWRITE_JSON` and restarting the container. You will not
  be able to update the configuration in the Harbor interface or in the commandline." This is a
  hard, **kind-level** lock for system settings, with the API answering "current config is init
  by env variable". It is clear but coarse. (The settings it covers are what Lajstrom puts in its
  config file.)
- **harbor-operator** ([repo](https://github.com/goharbor/harbor-operator), archived and marked
  deprecated). It deployed Harbor itself. Its `HarborConfiguration` CR applied system settings
  and reported `status.lastApplyTime` and `lastConfiguration`. Four secret fields (for example
  `oidc_client_secret`) are references to Kubernetes Secrets
  ([day2 configurations](https://github.com/goharbor/harbor-operator/blob/main/docs/day2/day2-configurations.md)).
  There are no CRDs for projects or robots, so Harbor never had a native GitOps story for its
  control plane.

### 2.3 JFrog Artifactory

- **Configuration YAML via REST:** `PATCH /api/system/configuration` with YAML
  ([apply YAML changes](https://docs.jfrog.com/installation/docs/apply-artifactory-yaml-configuration-changes)).
  Repositories are declared under `localRepositories`, `remoteRepositories`, `virtualRepositories`
  and `federatedRepositories`, keyed by repo key
  ([repositories in YAML](https://docs.jfrog.com/installation/docs/repositories-configurations-in-artifactory-yaml)).
  - Deletion is explicit: `key: null` or `~` removes a repository or a field
    ([delete an element](https://docs.jfrog.com/installation/docs/delete-an-existing-element-in-artifactory-yaml)).
  - The docs warn: "Do not set the yaml to not contain any elements, as this will delete all the
    local/remote/virtual repositories". An empty document is unscoped prune.
  - Ownership: none. It is a merge patch into the one configuration that the UI also edits, so
    the last writer wins.
- **Bootstrap:** `artifactory.config.import.yml` is loaded only on a fresh instance ("No
  repositories can exist yet") and "After loading, the YAML configuration file is deleted"
  ([bootstrap YAML](https://docs.jfrog.com/installation/docs/artifactory-bootstrap-yaml-file)).
  `artifactory.config.import.xml` "override[s] its global configuration" at startup
  ([descriptors](https://docs.jfrog.com/installation/docs/artifactory-configuration-descriptors)).
  Since 7.49, repository configuration has lived outside the descriptor and is managed through the
  REST API or UI. These are one-shot seeds, not reconciliation.
- **Terraform provider** ([jfrog/terraform-provider-artifactory](https://github.com/jfrog/terraform-provider-artifactory)):
  one resource per repository kind and format (for example `artifactory_virtual_maven_repository`
  with an ordered `repositories` list). It needs a licensed Pro or Enterprise edition. Terraform
  state is the ownership record, and the UI stays fully writable.
- **Complaints:**
  - Validation happens only at apply time, for example the lowercase rule on keys
    ([#1355](https://github.com/jfrog/terraform-provider-artifactory/issues/1355),
    [#972](https://github.com/jfrog/terraform-provider-artifactory/issues/972)).
  - No bulk adoption ([#621](https://github.com/jfrog/terraform-provider-artifactory/issues/621),
    "Mass import existing resources").
  - Two pipelines against federated clusters collide because the platform itself replicates
    repos: two writers for one object
    ([#1102](https://github.com/jfrog/terraform-provider-artifactory/issues/1102)).
  - Remote repository passwords in state
    ([#1346](https://github.com/jfrog/terraform-provider-artifactory/issues/1346), write-only
    request).

### 2.4 Sonatype Nexus Repository

- **No first-party configuration as code.** The Groovy **Script API** was the provisioning hook.
  It is "disabled by default" since 3.21.2, execution is behind `nexus.scripts.allowExecution`
  since 3.98.0, and it is "scheduled to reach end of life in December 2026". Sonatype's
  recommendation: "Use the Nexus Repository UI and REST APIs"
  ([script API](https://help.sonatype.com/repomanager3/integrations/rest-and-integration-api/script-api)).
- **Terraform** comes from the community provider
  ([datadrivers/terraform-provider-nexus](https://github.com/datadrivers/terraform-provider-nexus)).
  Pain points:
  - Objects that already exist: "allow updating or ignoring existing resources instead of
    erroring" ([#534](https://github.com/datadrivers/terraform-provider-nexus/issues/534)).
  - The default `maven-public` group cannot be taken over
    ([#107](https://github.com/datadrivers/terraform-provider-nexus/issues/107)).
  - Objects deleted outside Terraform break refresh
    ([#617](https://github.com/datadrivers/terraform-provider-nexus/issues/617)).
- **Community CasC plugin**
  ([AdaptiveConsulting/nexus-casc-plugin](https://github.com/AdaptiveConsulting/nexus-casc-plugin)):
  a YAML file applied at startup, with `${ENV}` and `${file:/path}` interpolation.
  - `pruneRepositories: true` means "True to delete repositories not present in this
    configuration file". `pruneUsers`, `pruneBlobStores` and `pruneCleanupPolicies` are the same.
    This is prune by absence, with no ownership scope: a repo created in the UI is deleted at the
    next restart.
  - `updateExistingPassword: false` makes a secret create-only, which is a reasonable pattern.

### 2.5 Keycloak

- **Server import at startup:** `--import-realm` reads `data/import`. "If a realm already exists
  in the server, the import operation is skipped … to avoid re-creating realms and potentially
  lose state". The `import` command overrides by default (`--override true`). Placeholders read
  any environment variable, and the docs warn that "There are currently no restrictions on what
  environment variables may be referenced"
  ([import/export](https://www.keycloak.org/server/importExport)).
- **Operator `KeycloakRealmImport`:** "only supports creation of new realms and does not update or
  delete those". Placeholders come from Secrets in the same namespace and "give access to all
  environment variables even sensitive ones". Status conditions are `Done`, `Started` and
  `HasErrors` ([realm import](https://www.keycloak.org/operator/realm-import)). The operator is
  create-only by design.
- **keycloak-config-cli**
  ([adorsys/keycloak-config-cli](https://github.com/adorsys/keycloak-config-cli)) is the de facto
  GitOps tool:
  - Ownership: "remote state" stored as realm attributes listing what it created
    ([MANAGED.md](https://github.com/adorsys/keycloak-config-cli/blob/main/docs/MANAGED.md)). With
    `import.remote-state.enabled: true` (the default) it "will purge only resources they created";
    without it, "all existing entities if they are not defined in import json".
  - Scope by key presence: "If you don't define any `groups` … Keycloak does not touch any
    `groups`", but "If you define `groups` but set an empty array, Keycloak will delete all groups".
    Per type, `import.managed.<type>=full|no-delete`. Some Keycloak defaults have to be copied into
    the file, because the tool "can't detect if the entity comes from a user or is auto-created by
    Keycloak itself".
  - Drift: none while the file is unchanged. With the cache enabled (the default), a realm whose
    import checksum is unchanged is skipped
    ([RealmImportService.updateRealmIfNecessary](https://github.com/adorsys/keycloak-config-cli/blob/main/src/main/java/de/adorsys/keycloak/config/service/RealmImportService.java)),
    so a UI edit lives until the next file change. The Grafana pattern again.
  - Complaints:
    - [#1682](https://github.com/adorsys/keycloak-config-cli/issues/1682): with remote state
      enabled, the first import carrying `groups: []` "deletes every group in the realm, including
      groups created manually", because `useRemoteState = enabled && !managedGroupNames.isEmpty()`.
      The safety switch turns off exactly when the state is empty.
    - [#826](https://github.com/adorsys/keycloak-config-cli/issues/826): users did not know that
      LDAP federations are deleted and recreated, losing federated users and offline tokens.
    - [#728](https://github.com/adorsys/keycloak-config-cli/issues/728): including `attributes`
      in an update payload reset unrelated properties. Partial updates of a large object are
      error-prone.

### 2.6 Argo CD

- **Its own configuration is declarative.** `argocd-cm` and `argocd-rbac-cm` are ConfigMaps,
  repositories are Secrets labelled `argocd.argoproj.io/secret-type: repository` (or `repo-creds`
  templates matched by URL prefix), and Applications and AppProjects are CRs. "Argo CD is able to
  manage itself", and then "you must enable the ServerSideApply=true sync option"
  ([declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/)).
  Applications created in the UI coexist with declared ones in the same namespace, and **nothing
  locks** a declared Application against UI edits. If the parent app self-heals, the edit is
  reverted (inference: the docs only advise "rather than manually modifying the live resources").
- **Ownership marker:** the annotation `argocd.argoproj.io/tracking-id:
  <app>:<group>/<kind>:<ns>/<name>` is the default. The old `app.kubernetes.io/instance` label was
  "truncated to 63 characters" and "Other external tools might write/append to this label".
  `installationID` separates several Argo CD instances
  ([resource tracking](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/)).
- **Self-heal and prune**
  ([auto sync](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/)):
  - "By default (and as a safety mechanism), automated sync will not delete resources."
  - `allowEmpty` protects "when there are no target resources".
  - Self-heal retries after `--self-heal-timeout-seconds` (5 s).
  - Automated sync does "not reattempt a sync if the previous sync attempt against the same
    commit-SHA … had failed".
- **Per-object safety**
  ([sync options](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-options/)):
  `Prune=false`, `Prune=confirm` ("critical and should not be pruned without confirmation"),
  `Delete=false|confirm`, `PruneLast`, `FailOnSharedResource` ("fail the sync whenever it finds a
  resource … already applied in the cluster" by another app), and `ServerSideApply`.
- **Partial ownership:** `ignoreDifferences` by JSON pointer, jq expression or
  `managedFieldsManagers` ("ignore differences from fields owned by specific managers defined in
  `metadata.managedFields`") ([diffing](https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/)).
  ApplicationSet adds `applicationsSync: create-only | create-update | create-delete | sync` and
  `ignoreApplicationDifferences`, whose stated use case is "temporarily toggling auto-sync". It
  works poorly on lists: "existing lists will be completely replaced"
  ([controlling resource modification](https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Controlling-Resource-Modification/)).
- **Secrets:** Kubernetes Secrets, or a plugin or external operator. Nothing is in git in clear.
- **Complaints:**
  - Shared resources between apps: `SharedResourceWarning`
    ([#18163](https://github.com/argoproj/argo-cd/issues/18163)). A force-sync of a non-owning app
    hard-replaces a shared Namespace, "ignoring Prune=false"
    ([#29739](https://github.com/argoproj/argo-cd/issues/29739)).
  - A generated Application "takes over all resources managed by ArgoCD, and then deleted"
    ([#20440](https://github.com/argoproj/argo-cd/issues/20440)).
  - Lesson: refuse multi-ownership at the time of claiming, not as a warning.

### 2.7 Flux

- **Ownership:** a two-key record. The Kustomization keeps an **inventory** in
  `.status.inventory` (`<namespace>_<name>_<group>_<kind>_<version>`), and every applied object is
  labelled `kustomize.toolkit.fluxcd.io/name` and `/namespace`
  (`SetOwnerLabels` in [fluxcd/pkg ssa](https://github.com/fluxcd/pkg/blob/main/ssa/manager.go)).
  Garbage collection deletes a stale inventory entry only if the live object still carries this
  Kustomization's labels (`Inclusions`) and lacks `kustomize.toolkit.fluxcd.io/prune: disabled`
  (`Exclusions`). Otherwise it is `SkippedAction`
  ([manager_delete.go](https://github.com/fluxcd/pkg/blob/main/ssa/manager_delete.go)).
- **Conflicts:** drift is detected with a server-side apply dry-run every interval and corrected:
  "all changes will be reverted when the controller reconciles"
  ([Kustomization API](https://fluxcd.io/flux/components/kustomize/kustomizations/)). There is no
  lock on `kubectl edit`; the revert is the enforcement. Per object, `kustomize.toolkit.fluxcd.io/ssa`
  takes `Override` (default), `Merge` (fields from other managers survive), `IfNotPresent`
  ("only apply … if they are not present", for objects others mutate), or `Ignore`.
- **Deletion:** `prune: true|false`. `deletionPolicy` takes `MirrorPrune` (the default), `Delete`,
  `WaitForTermination` or `Orphan`.
- **Suspend:** "new Source revisions are not applied", and drift correction stops too.
- **Ordering:** `dependsOn` between Kustomizations, plus health checks and `wait`. "Circular
  dependencies … must be avoided".
- **Status:** `Ready` (with reasons `ReconciliationSucceeded`, `PruneFailed`, `BuildFailed`,
  `HealthCheckFailed`, `DependencyNotReady`…), `Reconciling` and `Stalled`. Events carry the
  revision.
- **Secrets:** SOPS in git (only values encrypted, "You must leave `metadata`, `kind` or
  `apiVersion` in plain text"), decrypted by the controller.
- **Complaints:**
  - [kustomize-controller#1724](https://github.com/fluxcd/kustomize-controller/issues/1724)
    (open): all Kustomizations share one field manager, so two Kustomizations rendering the same
    object "silently replaces the labels and annotations of the other". A namespace was deleted
    because "Whether the namespace survived depended only on which Kustomization applied last".
  - [#732](https://github.com/fluxcd/kustomize-controller/issues/732): two Kustomizations
    flip-flopped a Deployment's version every 10 minutes. The proposed fix was to refuse when the
    owner label names another Kustomization.
  - [#338](https://github.com/fluxcd/kustomize-controller/issues/338): moving a resource to a
    nested Kustomization deletes and recreates it. A transfer is not a delete plus a create.

### 2.8 Kubernetes server-side apply and ApplySets

- **Field managers**
  ([SSA](https://kubernetes.io/docs/reference/using-api/server-side-apply/)):
  - `metadata.managedFields` records, per manager and per field, who last asserted a value.
  - An apply that changes a field another manager owns is a **conflict**, which "prevents an
    applier from unintentionally overwriting the value set by another user".
  - There are three ways out. Force ("changes the value of the field, and removes the field from
    all other managers' entries"). Drop the field from your config ("causes the field to be removed
    from the applier's entry"). Set the same value and become a shared manager.
  - Non-apply updates never conflict ("if you make a change using **update** … a conflict never
    provokes failure"). Imperative edits silently take fields from declarative owners.
  - "Transferring ownership" is a documented multi-step dance (the HPA and `replicas` example)
    with a race in it.
  - A field removed from the applied config is deleted only "if the field is not owned by any
    other field managers".
- **ApplySet pruning**
  ([declarative config](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/declarative-config/)):
  - A parent object identifies the set, and members carry `applyset.kubernetes.io/part-of=<id>`.
  - "Each object may be a member of at most one set."
  - The older allowlist prune is still alpha "due to usability, correctness and performance
    issues". Its warning: "if flag values are changed between invocations, this can lead to objects
    being unexpectedly deleted", and running it against a sub-directory deletes what is not there.
  - The docs still call `kubectl delete -f` the *recommended* deletion, because it "is more
    explicit about what is being deleted".
- **Diff:** `kubectl diff` is a server-side dry-run and "requires granting `PATCH`, `CREATE`, and
  `UPDATE` permissions".

Takeaways for Lajstrom. Per-field ownership exists in Kubernetes because many controllers
legitimately co-write one object (HPA and `replicas`, mutating webhooks). Lajstrom's control-plane
objects have no co-writing controllers, so per-object ownership is enough. ApplySet's "at most
one set" and its explicit set identity (not inferred from the directory) are directly applicable.

### 2.9 Backstage catalog

- **Model:** entities come from registered *locations* or *entity providers*. The catalog stamps
  `backstage.io/managed-by-location` and `backstage.io/managed-by-origin-location` ("the location,
  whose registration lead to the creation of the entity")
  ([well-known annotations](https://backstage.io/docs/features/software-catalog/well-known-annotations/)).
  Entities are read-only in the catalog. You edit the YAML at the source, and the entity page
  links to it.
- **Conflicts:** "no two providers can try to output the same entity"
  ([life of an entity](https://backstage.io/docs/features/software-catalog/life-of-an-entity)).
  A `locationKey` decides: "If the existing entity has no location key, the new entity wins. If
  the existing entity has a location key, the new entity only wins when the location keys match"
  ([entity providers](https://backstage.io/docs/features/software-catalog/external-integrations/entity-providers)).
  The first rule is implicit adoption of unowned objects; Lajstrom should not copy it.
- **Full vs delta mutations:** a provider either "replaces the entire bucket contents" (its own
  bucket, which is set semantics with owner-scoped prune) or "upserts or deletes specific
  entities". This maps exactly onto `apply --prune` from a directory (full) and an operator
  reacting to single CR events (delta).
- **Orphans:** when no parent emits an entity, stitching adds `backstage.io/orphan: 'true'`. The
  older annotation docs say orphans are kept "for safety reasons" and that "user action is
  required". The current life-of-an-entity page says the default is to "automatically remove
  orphaned entities", with `orphanStrategy: keep` to opt out. Deleting an entity that a location
  still emits is pointless: "it would just reappear again shortly". Unregistering the location
  removes its whole subtree.
- **Errors** are attached to the entity as status items.

### 2.10 Crossplane

- **Model:** a managed resource (MR) in Kubernetes is the owner of one external resource, linked
  by `crossplane.io/external-name`
  ([managed resources](https://docs.crossplane.io/latest/managed-resources/managed-resources/)).
- **Conflicts:** "Crossplane overrides any changes made to an external resource outside of
  Crossplane" on each poll. `initProvider` values apply at creation only and are not enforced
  afterwards, so a field can be owned only at create time.
- **Adoption** is a documented two-step process
  ([import existing resources](https://docs.crossplane.io/latest/guides/import-existing-resources/)).
  Start with `managementPolicies: ["Observe"]` ("never changes or deletes the resources"), then
  switch to `["*"]` after copying `status.atProvider` into the spec.
- **Pause:** `crossplane.io/paused: "true"`, and paused resources cannot be deleted. Status
  conditions are `Synced` (`ReconcileSuccess`, `ReconcileError`, `ReconcilePaused`) and `Ready`.
  Deletion: the external resource is removed only with the `Delete` policy, and otherwise
  orphaned.

### 2.11 Terraform as a pattern (with notes on Vault and GitLab)

- The **state file** is an external ownership record. The managed system knows nothing, so
  locking is impossible and drift is only what `plan` refreshes.
- Adoption: `import` blocks, with `-generate-config-out` to write HCL for imported objects
  ([import](https://developer.hashicorp.com/terraform/language/import)).
- Release: `removed { lifecycle { destroy = false } }`, "remove the resource from state without
  destroying the actual resource", useful for "handing management to another tool or team"
  ([removed](https://developer.hashicorp.com/terraform/language/block/removed)).
- **Vault:** `vault_generic_secret`: "All data provided in the resource configuration will be
  written in cleartext to state and plan files"
  ([doc](https://github.com/hashicorp/terraform-provider-vault/blob/main/website/docs/r/generic_secret.html.md)).
  This is the canonical warning against declaring secrets.
- **GitLab:** `gitlab_application_settings` "does not implement any destroy logic, it's a no-op"
  ([doc](https://github.com/gitlabhq/terraform-provider-gitlab/blob/main/docs/resources/application_settings.md)).
  Singleton settings cannot be "pruned". Lajstrom avoids this by keeping singletons in the config
  file.

---

## 3. Side by side

| System | Ownership record | UI/API edit of a managed object | Prune scope | Adoption of existing | Drift detection | Secrets |
| --- | --- | --- | --- | --- | --- | --- |
| Grafana classic, dashboards | path-keyed row with file checksum | blocked, or allowed and lost when the file changes (`allowUiUpdates`) | files missing from the provider dir; `disableDeletion` releases instead | silent overwrite by uid or title | file checksum, not live state | env interpolation |
| Grafana classic, datasources | none (by name) | `editable`: allowed, lost at restart | `deleteDatasources` list or `prune: true` (provisioned only) | silent overwrite | none (re-applied at start) | `secureJsonData` encrypted in DB |
| Grafana classic, alerting | provenance per object | refused (unless `X-Disable-Provenance`) | explicit delete lists | conflict error | none | env interpolation |
| Grafana 12 unified storage, Git Sync | `managedBy` + `managerId` on the object, plus source path and checksum | refused, or routed to git as a commit or PR; manager change forbidden | per repository; on repository delete, *remove* or *release* | conflict unless on the takeover allowlist | sync jobs | n/a |
| Grafana Operator | the CR | allowed, reverted at resync (10 m) | finalizer on CR delete | overwrite by uid | CR hash plus remote check (buggy, #2365) | Kubernetes Secrets |
| Harbor + Terraform | TF state | allowed; plan reverts if the provider reads the field | TF state | `import`; create returns 409 | provider refresh (gaps #288, #317) | in state; write-only args requested |
| Harbor `CONFIG_OVERWRITE_JSON` | the env var | refused (kind-level lock) | n/a | n/a | n/a | in env |
| Artifactory YAML PATCH | none | both write; last writer wins | explicit `null`; an empty doc wipes repos | n/a | none | in YAML |
| Artifactory + Terraform | TF state | allowed | TF state | one-by-one `import` (#621) | refresh | in state (#1346) |
| Nexus CasC plugin | none | allowed until restart | everything not in the file (`pruneRepositories`) | implicit | startup only | `${file:}`, create-only passwords |
| Keycloak import / RealmImport CR | none (create-only) | n/a | none | skipped if it exists | none | env or Secret placeholders (all env reachable) |
| keycloak-config-cli | remote state in realm attributes | allowed; lost on the next file change | per type when the key is present; owned-only with remote state (bug #1682) | implicit update by name | file checksum | variable substitution |
| Argo CD | `tracking-id` annotation (+ `installationID`) | allowed; self-heal reverts in about 5 s; no lock | off by default; `Prune=confirm`; `allowEmpty` guard | silent if tracking matches; `FailOnSharedResource` opt-in | continuous live diff; `ignoreDifferences` | Kubernetes Secrets or plugins |
| Flux | inventory in status + owner labels | allowed; reverted each interval; `ssa: Merge` / `IfNotPresent` | inventory ∩ owner labels − `prune: disabled`; `deletionPolicy` | last applier wins silently (#1724) | SSA dry-run per interval | SOPS |
| Kubernetes SSA + ApplySet | `managedFields` per field; `part-of` label | apply conflicts with 409; plain update never conflicts | set members only; one set per object | force, or share by applying the same value | `kubectl diff` (server dry-run) | Secrets |
| Backstage | `managed-by-location`, `locationKey` | read-only (edit at the source) | provider bucket (full mutation); orphans marked | unkeyed objects are taken over; keyed only by the same key | refresh loop; per-entity errors | n/a |
| Crossplane | the MR + `external-name` | external edits overwritten on poll | `Delete` vs orphan | Observe-only, then `*` | poll; `Synced`/`Ready` | connection Secrets |
| Terraform (generic) | external state | allowed | state | `import` blocks | `plan` | cleartext in state |

---

## 4. The design space

Each dimension lists its options with the products that chose each, and the choice recommended
for Lajstrom.

| # | Dimension | Options (who) | For Lajstrom |
| --- | --- | --- | --- |
| D1 | Where ownership is recorded | external state (Terraform); in the source only (Grafana classic dashboards: a path row); on the object (Grafana 12, Argo, Backstage); on the object + source inventory (Flux) | **On the object**, in the control-plane row, plus a `sources` row per source. One database means the owner column *is* the inventory (`WHERE owner = 'source:x'`), so no separate list can disagree with it. |
| D2 | Granularity | kind-level lock (Harbor env, Nexus CasC prune flags); per object (Grafana 12, Backstage, Crossplane); per field (Kubernetes SSA, Argo `ignoreDifferences`, Flux `ssa: Merge`) | **Per object.** Optional kind-level exclusivity for bindings (§7.9). No per-field ownership in v1; separate operational toggles from the spec instead (§7.10). |
| D3 | A UI/API edit of an owned object | allowed and lost later (Grafana classic, keycloak-config-cli, Grafana Operator); allowed and reverted continuously (Argo, Flux, Crossplane); refused (Harbor env, Grafana alerting, Grafana 12, Backstage); routed to git (Grafana Git Sync) | **Refused**, 409 naming the source, with a way forward. Routing to git is a later, separate feature. |
| D4 | Reconcile trigger, basis of drift | startup only (Keycloak, Nexus CasC); source checksum change (Grafana dashboards, keycloak-config-cli); periodic live diff (Flux, Argo, Crossplane) | **On source revision change plus a periodic live diff.** Live diffs are cheap: the control plane is small. |
| D5 | Prune scope | none (Keycloak import); explicit tombstones (Grafana datasources and alerting, Artifactory `null`); everything absent (Nexus CasC, Artifactory empty YAML, keycloak-config-cli without remote state); owned and absent (Flux, Backstage buckets, ApplySet, keycloak-config-cli remote state) | **Owned by this source and absent from its current set**, with guards (§7.6). |
| D6 | When the source is removed | delete everything (Flux `Delete`, Grafana `remove-orphan-resources`); release (Flux `Orphan`, Grafana `release-orphan-resources`, Terraform `removed destroy=false`); keep owned and mark orphan (Backstage) | **Explicit choice at removal**, release by default (§7.7). |
| D7 | Adoption of an existing same-named object | silent overwrite (Grafana classic, Grafana TF `overwrite`, Backstage unkeyed); error (Terraform providers, Grafana 12); explicit import (Terraform `import`, Grafana takeover allowlist); observe then manage (Crossplane) | **Error by default; explicit adopt**, shown in the diff. |
| D8 | Transfer between owners | implicit last writer (Flux #1724); delete and recreate (Flux #338); forbidden in place, remove then add (Grafana 12); force (SSA) | **One atomic `transfer` verb**: no delete, no recreate, audited. |
| D9 | Multiple sources | silent (Flux); warning (Argo `SharedResourceWarning`); refused per object (Backstage `locationKey`, ApplySet, Argo `FailOnSharedResource`) | **Refused per object**, condition `Conflict` naming the current owner. |
| D10 | Pause | per source (Flux `suspend`, Grafana Operator `suspend`); per object (Crossplane `paused`, Flux `reconcile: disabled`); per manager (Grafana `managerSuspended`) | **Per source.** Per object is covered by release. |
| D11 | Reporting | logs only (Grafana classic, keycloak-config-cli); CR status (Harbor operator); per-object and per-source conditions + events (Flux, Crossplane, Argo, Backstage) | **Per object and per source**, in the API, the UI, metrics and audit (§7.13). |
| D12 | Secrets | inline (Nexus CasC, Artifactory YAML, Terraform state); env interpolation (Grafana, Keycloak); encrypted in git (Flux SOPS); references (Argo, Grafana Operator, harbor-operator); write-only arguments (recent Terraform; requested in the Harbor, JFrog and Nexus providers) | **None in manifests**, enforced by the schema. If ever needed, refer by name to a secret declared in the config file (§7.12). |
| D13 | Unit of apply | per object, eventually consistent (Kubernetes, Argo, Flux, Terraform); per file (Grafana classic) | **One database transaction per source revision.** Lajstrom's control plane is one database, which Kubernetes tools do not have. |
| D14 | Validation | at apply only (JFrog #1355); server-side dry-run (`kubectl diff`, Argo); plan (Terraform) | **Server-side dry-run with the same validators as the API**, plus an offline schema check for CI. |

---

## 5. What works and what fails

### Works

- **W1. One owner per object, recorded on the object, enforced by the server on every write
  path.** Grafana 12 (`managedBy`/`managerId`, checks in the storage layer), Argo `tracking-id`,
  Backstage `managed-by-location`.
- **W2. Refuse, or revert continuously; never "allow and lose later".** Harbor env lock, Grafana
  alerting provenance, Grafana 12 (refuse); Argo self-heal, Flux, Crossplane (continuous revert).
- **W3. Owner-scoped prune with two keys:** the source's inventory and the object's owner marker
  (Flux `Inclusions`). Plus "one set per object" (ApplySet).
- **W4. Prune safety rails.** Off by default (Argo), refusal on an empty set (`allowEmpty`),
  per-object `Prune=confirm` and `prune: disabled`, and a recommended explicit
  `kubectl delete -f`.
- **W5. Explicit adoption:** Crossplane observe-then-manage, Terraform `import` with config
  generation, Grafana's takeover allowlist.
- **W6. An explicit choice between release and delete when a source goes away:** Grafana
  finalizers, Flux `deletionPolicy`, Terraform `removed`.
- **W7. Suspend per source:** Flux, Grafana Operator, Grafana `managerSuspended`.
- **W8. Conditions with reasons and the source revision,** per object and per source: Flux,
  Crossplane, Backstage.
- **W9. A server-side dry-run diff** that uses the real validators: `kubectl diff`,
  `terraform plan`.
- **W10. A read-only UI that links to the source and offers export:** Backstage links the YAML;
  Grafana's export to copy JSON and the operator's "duplicate, edit, export" workflow.

### Fails

- **F1. An edit allowed without an ownership change, lost at an unrelated moment.** Grafana
  `allowUiUpdates` and `editable` (#75722); keycloak-config-cli checksum skip; Grafana Operator
  hash shortcut (#2365).
- **F2. Identity from something other than the object's name:** the file path (Grafana #80055),
  a tool's User-Agent (Grafana's Terraform hack).
- **F3. Prune by absence instead of by ownership:** Nexus CasC `pruneRepositories`; Artifactory
  empty YAML; kubectl allowlist prune on a sub-directory; keycloak-config-cli with an empty remote
  state (#1682).
- **F4. Silent co-ownership between sources:** Flux #1724 and #732; Argo #29739 and #20440.
- **F5. Transfer as delete plus recreate:** Flux #338; Grafana #80055.
- **F6. Silent adoption or overwrite of existing objects:** Grafana classic, Grafana TF
  `overwrite`, Backstage unkeyed entities. The opposite, a hard error with no adopt path, is a
  constant complaint too (Harbor #39 and #603, Nexus #534 and #107, JFrog #621).
- **F7. Create-only importers mistaken for GitOps:** Keycloak `--import-realm` and
  `KeycloakRealmImport`, the Artifactory bootstrap.
- **F8. A write path that skips the lock:** Grafana #137468, where deleting the folder deleted the
  provisioned dashboard.
- **F9. Secrets in declared state** (Terraform state, Vault warning) and **environment
  interpolation that can read any variable** (Keycloak).
- **F10. Validation only at apply time:** JFrog #1355 and #972.
- **F11. Different semantics per resource type:** Grafana's dashboards, datasources and alerting.
- **F12. "Presence of a key switches on management of the whole type":** keycloak-config-cli,
  where `groups: []` deletes all groups. One key in a file should never change the scope of
  ownership.

---

## 6. The draft rule, clause by clause

| Draft clause | Verdict | Evidence | Refinement |
| --- | --- | --- | --- |
| Every object has exactly one owner, `managedBy: api \| gitops:<source>` | Right | W1, F4; Grafana 12; ApplySet "at most one set" | Record **kind** and **name** separately (`{kind: api}` / `{kind: source, name: platform}`), plus **source metadata** (path, revision, appliedAt) as information, not identity. Use `source:` rather than `gitops:`: `lajstrom apply` from a laptop and a future operator are sources too. A source must be a registered object, so a typo cannot invent a new owner. `api` covers the REST API, the UI, `lajstrom admin` and the config bootstrap. |
| Owned objects are read-only through the API and UI, 409 naming the source | Right | W2, F1, F8 | Read-only means the **declared spec**, not operations (§7.3 lists the carve-outs). **Revocation is never blocked**: disabling or deleting a user and revoking a token always work, even when a source's binding names that user. The check lives in the service layer that the API, `lajstrom admin` and the bootstrap all share (one control-plane write path, like the one ingest path for artifacts). 409 with a new code `MANAGED` and the source, path and revision in the body. Grafana uses 403; 409 fits Lajstrom's envelope, where 409 already means "state conflict". |
| Prune touches only the owner's objects | Right | W3, W4, F3, keycloak-config-cli #1682 | Add an empty-set guard (refuse a revision that would prune everything unless `--allow-empty`). Add a content guard: a repo with versions or signing keys is pruned only with an explicit confirmation, because deleting a repo drops its signing keys and deploy tokens (api.md). With prune off, an absent object becomes `Orphaned`: still owned, visible, and released or pruned by an explicit action. |
| Ownership changes only by an explicit command | Right | W5, W6, F5, F6; Grafana "remove the existing manager first" | Three verbs (adopt, release, transfer) and one source-removal choice (release or delete). Each is plannable (it appears in `--dry-run`) and audited. Adoption can also be declared per object in the manifest (`lajstrom.io/adopt: "true"`), so a migration is one reviewed change. |
| One schema; `apply` (diff, `--prune`), `serve` watching a directory, later an operator | Right | Backstage full and delta mutations; W9 | **One server-side apply engine** that all three call. A source revision is a full set applied in **one transaction** (all or nothing). The operator later uses the same engine with delta mutations. The schema is also the control-plane export format of ADR 0006 (§7.17). |
| (not in the draft) | Add | D10, D11, D14 | Sources registry; suspend; status and conditions; diff format and exit codes; ordering inside the transaction; names for bindings; operational toggles outside the spec; secrets forbidden; the relationship to the `repos:` bootstrap. |

Where the draft could go wrong if taken literally:

1. **"Read-only" applied to everything about a repo** would block reindex, cache purge, version
   deletion, deploy tokens and key rotation on managed repos. These are not spec.
2. **"Read-only" applied to bindings that name a user** must not block offboarding. The API
   today deletes a user's bindings when the user is deleted (api.md, users). With managed
   bindings, the user is deleted, the binding stays and grants nothing (condition
   `Unresolved`), and the source shows it until git is fixed.
3. **Bindings have no name** (the server-assigned `id` cannot appear in a manifest). The draft
   needs a natural key or a named container object (§7.9).
4. **A watched directory is not an atomic unit by default.** ConfigMap volumes and `git-sync`
   swap a symlink (Grafana #80055), and replicas see a new ConfigMap at different times.
   Without leader-only apply and snapshot semantics, two replicas can apply two revisions
   alternately.

---

## 7. Recommendations for ADR 0015

### 7.1 Principles (candidate "Decision" bullets)

1. **One owner per object:** `api` or one named source. The owner is stored on the object.
2. **The owner alone changes the declared spec.** Every other writer gets `409 MANAGED`, on every
   write path.
3. **Sources apply full sets, transactionally.** A revision is applied whole or not at all.
4. **Prune is owner-scoped and guarded.**
5. **Ownership moves only by explicit, plannable, audited verbs.**
6. **Status is reported per object and per source, with the revision.**
7. **Manifests carry no secrets and no interpolation.**
8. **Infrastructure stays in the config file; the config file may name sources but declares no
   control-plane objects beyond the existing create-only bootstrap.**

### 7.2 Data model

- Every control-plane row that can be declared (repos; bindings or binding sets; later OIDC group
  mappings and retention rules) gets:
  - `owner_kind` (`api` | `source`) and `owner_name` (null for `api`): identity.
  - `source_path`, `source_revision`, `applied_at`: information for the UI and the audit, never
    used as identity (F2).
  - `sync_state` (`Synced` | `Drifted` | `Conflict` | `Orphaned` | `Unresolved` | `Invalid`),
    `sync_reason`, `sync_message`, `observed_revision`.
- A `sources` table: `name`, `kind` (`dir` | `push` | `k8s`), `suspended`,
  `last_applied_revision`, `last_attempted_revision`, `last_error`, `conditions` (JSON),
  `heartbeat`, `leader`. The coordination row follows the `reindex_runs` pattern.
- These are control-plane tables (ADR 0006): backed up and not derived. Restoring a backup
  restores ownership. The next reconcile brings each source's objects back to git, so git becomes
  a second backup of the declared part (inference; this is a useful property to state in the ADR).
- Resources in the API gain `"managedBy": {"kind": "source", "name": "platform"}` and
  `"source": {"path": "repos/npm.yaml", "revision": "3f2c1a9", "appliedAt": "…"}`, plus
  `"status": {...}`. This is an additive contract change to `api.md` and the OpenAPI document.

### 7.3 Enforcement, carve-outs and the error

Blocked for an object owned by a source, through the API, the UI, `lajstrom admin` and the
bootstrap alike:

- `PATCH` and `DELETE /repos/<f>/<n>`
- `DELETE /bindings/<id>` of a managed binding
- creating an `api` object with the identity of a managed one

Always allowed, subject to RBAC as today:

- `POST /repos/…/reindex`
- `DELETE /repos/…/upstream-cache`
- `DELETE …/versions/…`
- deploy tokens of the repo (child objects, `api`-owned)
- `admin key rotate`
- user disable and delete
- token revoke
- reads

The answer:

```json
{"error": {
  "code": "MANAGED",
  "message": "repo npm/all is managed by source platform (repos/npm.yaml at 3f2c1a9): change it there, or release it with `lajstrom source release platform repo npm/all`",
  "managedBy": {"kind": "source", "name": "platform"},
  "source": {"path": "repos/npm.yaml", "revision": "3f2c1a9"}
}}
```

Status `409`. Adding `MANAGED` and the extra members to the envelope is a `contract` change.
Keeping the code distinct from the existing 409 uses ("exists", "has versions") lets clients and
the UI react properly. A refusal changes nothing, so, like other 4xx answers, it needs no audit
row.

Break-glass: an admin suspends the source, then releases the one object (an ownership change,
audited), edits it, and later fixes git and re-adopts. There is **no** "force edit while still
owned" (F1).

### 7.4 Sources

- **Declared in the config file** (infra decides who may own the control plane, the same
  argument ADR 0013 makes for upstreams):

  ```yaml
  sources:
    - name: platform            # stable, user-chosen identity (F2)
      kind: dir                 # serve watches it
      path: /etc/lajstrom/manifests
      prune: true
      interval: 5m              # periodic drift check; revision changes apply at once
    - name: ci
      kind: push                # only `lajstrom apply --source ci` (admin token) writes to it
      prune: true
  ```

  The state (suspended, revision, conditions) lives in the `sources` table. Suspend and resume are
  API and CLI operations (`lajstrom source suspend platform`), because they are operational, like
  Flux's `suspend`.
- A source missing from the config while it still owns objects is reported at startup (an error
  log and condition `SourceMissing` on its objects). Its objects stay owned and read-only until an
  admin runs `lajstrom source release|delete <name>`. Nothing happens implicitly.
- **Identity is always explicit:** `lajstrom apply` needs `--source <name>`. It is never derived
  from the directory or flags (the warning about kubectl allowlist prune).
- Optional: `apply --source api` means a bulk upsert of `api`-owned objects with no prune, for
  seeding and migration. It refuses to touch source-owned objects.

### 7.5 The apply engine

- **Input:** a source name, a revision string (`git rev-parse HEAD` from `apply`; for `dir`, a
  revision file if present, otherwise a hash of the sorted, canonicalised contents), and the full
  set of objects.
- **One function, three callers.** `lajstrom apply` posts to `POST /api/v1/sources/<name>/apply`
  (`?dryRun=true` for the plan). The `dir` watcher calls the same function in-process. The
  operator calls the HTTP endpoint, later with a delta form (`PUT`/`DELETE` of single objects,
  Backstage's "delta" mutation) or full sets per namespace.
- **Steps, in one database transaction:**
  1. Parse and validate the schema. Unknown fields are errors, as in the config and the API.
  2. Validate each object with **the same validators as `POST`/`PATCH`**, as the bootstrap already
     does ("all three validate and audit alike").
  3. Run set-level checks: duplicate identities in the set; references (group members,
     upstreams of the config, registries of the config, principals); ownership conflicts.
  4. Compute the plan (create, update, adopt, prune, refused).
  5. Order it topologically: hosted and proxy repos before the groups that name them; groups
     updated before their old members are pruned. The bootstrap already orders hosted and proxy
     before groups. Inside one transaction, authors never need sync waves (D13).
  6. Apply and write audit rows (`source: "apply:<name>"`, the revision, the principal that
     pushed for `push` sources).
  7. Update the status. On any error nothing changes, and the source reports
     `Ready=False reason=ValidationFailed` with "still at revision X".
- **Side effects outside the database** (registry `ensureRepo` on ECR, upstream checks) happen
  after commit, idempotently, and report into the object's status. They never break atomicity
  (inference about the current code: repo creation looks database-only; signing keys are made on
  first use).
- **Idempotency and drift:** when the revision is unchanged, still compute the desired vs live
  diff each interval. It is cheap, and it catches restores, migrations, direct database edits and
  release-and-edit. Never skip the live diff because a hash matched (Grafana Operator #2365,
  keycloak-config-cli).
- **Replicas:** only the lease holder applies a `dir` source. Others watch and do nothing. The
  watcher reads a whole snapshot (resolve the `..data` symlink once, ignore dot-entries) and never
  acts on single-file events (Grafana #80055). Optionally refuse to apply a revision older than
  the last applied one when the source supplies an ordered generation (Grafana's datasource
  `version` exists for this multi-instance case).

### 7.6 Prune and orphans

- Prune candidates are objects `WHERE owner = source:X AND identity NOT IN current_set`. Nothing
  else is ever a candidate.
- `prune: false` (per source) leaves candidates **owned** with `sync_state = Orphaned`, shown in
  the UI and API. They are resolved explicitly with `lajstrom apply --prune` or `source release`.
- Guards. A refused prune leaves the object `Orphaned` with a reason, and the revision still
  applies the rest unless the source is configured `strict`.
  - **Empty set:** a revision with zero objects for a source that owns some is refused unless
    `--allow-empty` (Argo `allowEmpty`, keycloak-config-cli #1682).
  - **Repo with content:** a repo with versions, signing keys or deploy tokens is pruned only if
    the manifest that removed it left a tombstone (`lajstrom.io/prune: confirm` on a `Deleted`
    stub), or with `apply --prune --confirm repo/<f>/<n>`. Otherwise it is `Orphaned` with reason
    `PruneNeedsConfirmation`. This mirrors the API's 409 for repos with versions and Argo
    `Prune=confirm`.
  - **Referenced object:** a repo that is a member of a group is not pruned until the group
    drops it. Within one transaction that is just ordering. Across owners it is the existing 409
    rule of `proxy-and-group.md` §2.5.
- Per-object opt-out `lajstrom.io/prune: disabled` (Flux) is unnecessary when the guards exist.
  Leave it out of v1.

### 7.7 Adopt, release, transfer, source removal

- **Adopt** (`api` → source): a manifest object whose identity matches an `api` object is a
  `Conflict` unless adoption is requested, either per object with
  `metadata.annotations: {lajstrom.io/adopt: "true"}` or per run with `apply --adopt`. The plan
  shows `adopt` with the field changes it will make. Generate manifests for existing objects with
  `lajstrom export [--kind repo] [--owner api]` (Terraform `-generate-config-out`, Grafana
  export). That covers the JFrog #621 and Nexus #534/#107 complaints, the bootstrap's repos, and
  default repos.
- **Release** (source → `api`): `lajstrom source release <source> [<kind> <id>…]`, admin only.
  The object keeps its values and becomes editable. If the source still declares it, the source
  now reports `Conflict` for it until the manifest drops it or re-adopts it.
- **Transfer** (source A → source B): `lajstrom source transfer <from> <to> <kind> <id>…`, or
  `adopt` in B with `lajstrom.io/adopt-from: A`. Always atomic, with no delete and recreate
  (Flux #338). Without it, moving a file between sources means release followed by adopt.
- **Source removal:** `lajstrom source delete <name> --release | --delete`. Release is the
  default and the choice is mandatory to state (Grafana finalizers, Flux `deletionPolicy`).
  `--delete` obeys the prune guards.

### 7.8 Multiple sources

- Any number of sources. Each object belongs to at most one (ApplySet).
- A second source declaring an owned object gets `Conflict` for that object, naming the owner.
  The rest of its revision applies; `strict` sources fail the whole revision instead. Never last
  writer wins (Flux #1724).
- References across owners are allowed: a source-owned group may name an `api`-owned hosted repo
  and the reverse. Integrity is the existing rule: a repo that is a member of a group cannot be
  deleted (409 naming the groups), whoever owns either. Grafana's "same manager as the folder"
  rule is for containment. Lajstrom has references, not containment, so it does not apply.

### 7.9 Bindings (and later OIDC groups)

- **Identity:** introduce a named object for declaration and ownership:

  ```yaml
  apiVersion: lajstrom.io/v1alpha1
  kind: RoleBinding
  metadata:
    name: platform-writers        # identity of the declared object
  spec:
    role: writer
    repoMasks: ["npm/*", "pypi/*"]
    subjects: ["group:platform", "user:alice"]
  ```

  It expands to `(principal, role, mask)` tuples, each owned by the named object's owner. `api`
  bindings stay as today, without a name. An `api` tuple equal to a declared one is a `Conflict`
  (adopt to resolve), so that "remove alice's write" never silently leaves a second grant behind.
  Alternative: allow duplicates across owners and show "also granted by …". Simpler to build, but
  revocation by API then does not revoke, which is worse for security.
- **Principals by name in manifests,** resolved to uid at apply. A rename through the API makes
  the binding `Unresolved` (it grants by uid as today, and the status says the manifest's name no
  longer resolves) until git is fixed. A deleted or disabled user makes the binding grant nothing,
  with `Unresolved` status. Deleting or disabling a user is **never** refused because of a managed
  binding.
- **Additive by default:** declared bindings coexist with `api` bindings. RBAC is a union
  (ADR 0007).
- **Optional exclusive mode,** a config switch and infra's decision: `rbac.bindings.owner:
  source:platform` refuses every `api` binding create and delete, which makes git the only place
  where access is granted. This is a kind-level lock in the style of Harbor's
  `CONFIG_OVERWRITE_JSON`, explicit and global. The admin bootstrap (`POST /users` with
  `admin: true`, the last-admin rules) needs a defined exception or a declared admin binding.
  Decide which before shipping the switch.
- OIDC: groups come from claims, so `group:<name>` cannot be validated at apply. Accept it and
  report "no member seen yet" as information, not an error. If Lajstrom later stores
  claim-to-group mappings, they are just another declarable kind.

### 7.10 Repos

- Declared spec: `format`, `name`, `registry`, `kind`, `anonymousRead`, `policy`, and
  `upstream` / `group`. The immutable fields stay immutable: a manifest that changes `registry`
  or `kind` is `Invalid` (as `400` in the API), never a delete and recreate.
- **Operational toggles are not spec.** A proxy's `upstream.offline` is something an operator
  flips during an npmjs outage, at 3 a.m., without a PR. Two options:
  - (a) move such toggles to a state sub-resource (`PUT /repos/<f>/<n>/state`), which a manifest
    cannot set and which `manage` can change on any repo;
  - (b) mark them in the schema as "initial only" (Crossplane `initProvider`): set at creation,
    never enforced afterwards.

  Prefer (a). It keeps per-object ownership honest and avoids the Argo `ignoreDifferences`
  complexity. Today the candidate list is `upstream.offline`; review each new field against it.
- Admin-only fields (`group.members`, `upstream.name`) need no special handling: a source applies
  with the authority of the source (an admin-equivalent identity), and the push endpoint requires
  the admin role.

### 7.11 Retention rules (later)

Declaring a retention rule declares the deletion of artifacts. The dry-run of a source that adds
or widens a rule should include an **impact preview** ("would delete 312 versions in 4 repos at
the next run"), and new rules could start in `dryRun: true` mode until confirmed (as Harbor's
retention offers a dry run). The `Orphaned` and prune guards apply to the rules, not to the
artifacts they would delete.

### 7.12 Secrets

- The schema has no secret-bearing fields. A field named `password`, `secret` or `token` is
  rejected, as the config file rejects `password`. Deploy tokens and user passwords are never
  declared.
- No `${ENV}` or `${file:}` interpolation in manifests (Keycloak warning, F9). Templating belongs
  upstream (Kustomize, Helm, CUE). Lajstrom applies plain objects.
- If a future kind needs a secret (webhook HMAC, replication credentials), it refers to a
  **named** secret declared in the config file (`secrets: [{name: hook-hmac, file: …}]`), exactly
  as proxy repos refer to upstreams by name (ADR 0013). A manifest never names a path or a
  variable, so a manifest author cannot read server files or the server's environment.
- A future operator that needs to *output* a generated secret (a deploy token for a CI system)
  writes it to a Kubernetes Secret, as the Grafana Operator does for service accounts. That is a
  later decision.

### 7.13 Drift, status and observability

- **Per object** (`GET /api/v1/repos/<f>/<n>` and the others):
  `status.conditions[type=Synced]` with reason `Applied`, `Drifted`, `Conflict`, `Orphaned`,
  `PruneNeedsConfirmation`, `Unresolved` or `Invalid`, a message, `observedRevision`, and
  `lastTransitionTime`.
- **Per source** (`GET /api/v1/sources`, `/sources/<name>`, `lajstrom source status`):
  - conditions `Ready` (with reasons `Applied`, `ValidationFailed`, `PartiallyApplied`,
    `Suspended`, `SourceUnreadable`), `Reconciling` and `Stalled`;
  - `lastAppliedRevision`, `lastAttemptedRevision`, the object count by state, and the last
    error.
- **Metrics:** `lajstrom_source_ready{source}`, `lajstrom_source_objects{source,state}`,
  `lajstrom_source_last_applied_timestamp_seconds{source}`,
  `lajstrom_source_apply_errors_total{source,reason}`.
- **Audit:** every change by a source is a normal audit row with `details.source =
  "apply:<name>"`, `revision`, `path`. Adopt, release, transfer, suspend and resume are audit
  actions (`source.adopt`, …).
- **Drift** should be rare by construction, because writes are refused. It is still detected and
  corrected on each interval, and reported as an event, so restores and bugs are visible.

### 7.14 Dry-run diff format

Human output (exit codes as in `terraform plan -detailed-exitcode`: 0 no changes, 2 changes, 1
error or refusals):

```text
$ lajstrom apply --source platform -f manifests/ --prune --dry-run
source platform: revision 3f2c1a9 (applied: 9b07e44)
  + Repo        npm/internal        create
  ~ Repo        npm/all             update   group.members: [internal] → [internal, npmjs]
  ~ Repo        raw/assets          adopt    owner: api → source:platform
                                             policy.maxFileSize: null → 104857600
  - RoleBinding legacy-readers      prune    (2 bindings: user:bob reader */*, user:eve reader raw/*)
  ! Repo        pypi/libs           conflict managed by source:ci (pypi.yaml at 77ab0e2); unchanged
  ! Repo        apt/old             refused  prune needs confirmation: 1234 versions, 1 signing key
Plan: 1 create, 1 update, 1 adopt, 1 prune; 2 refused.
```

JSON output (`-o json`) is for CI bots and the UI. Its field paths are the paths of the
validation messages, so one vocabulary covers errors and diffs:

```json
{"source": "platform", "revision": "3f2c1a9", "appliedRevision": "9b07e44",
 "changes": [
   {"action": "update", "kind": "Repo", "id": "npm/all",
    "fields": [{"path": "group.members", "from": ["internal"], "to": ["internal", "npmjs"]}]},
   {"action": "adopt", "kind": "Repo", "id": "raw/assets",
    "owner": {"from": {"kind": "api"}, "to": {"kind": "source", "name": "platform"}},
    "fields": [{"path": "policy.maxFileSize", "from": null, "to": 104857600}]}],
 "refused": [
   {"kind": "Repo", "id": "pypi/libs", "reason": "Conflict", "owner": {"kind": "source", "name": "ci"}},
   {"kind": "Repo", "id": "apt/old", "reason": "PruneNeedsConfirmation", "versions": 1234, "signingKeys": 1}]}
```

Also an offline `lajstrom validate -f manifests/` that checks the schema only, with a published
JSON Schema for editors and CI. It cannot check references to live objects, and says so.

### 7.15 UI (`lajstrom-ui`)

- A badge on every managed object: "Managed by **platform**" with the path, the revision, a link
  to the file (a per-source URL template, for example `https://git.example/infra/blob/{revision}/{path}`)
  and the applied time.
- Edit controls of the declared spec are **disabled from the start**, with the reason shown. Not
  "edit, then fail at save" (Grafana #13823, #114281). Operations (reindex, purge, delete
  version, tokens, the state toggles of §7.10) stay enabled.
- "Copy as manifest" and "Download YAML" on every object, managed or not, for the
  prototype-then-PR workflow (Grafana Operator's "duplicate, edit, export").
- Admins get "Release" on a managed object (with a confirmation that names the source) and see
  the source's status page (conditions, last revision, conflicts, orphans).
- Status chips: `Drifted`, `Conflict`, `Orphaned`, `Unresolved`.

### 7.16 Delivery mechanisms

- **`lajstrom apply`:** a thin client of `POST /sources/<name>/apply`. It reads files and
  directories, sends the set, and prints the plan or the result. `--dry-run`, `--prune`,
  `--adopt`, `--allow-empty`, `--confirm`, `-o json`. It requires the admin role.
- **`serve` watching a directory:** a `dir` source. The Kubernetes recipe is Flux (or Argo)
  writing a ConfigMap mounted into the pod. The ConfigMap limit (1 MiB) is plenty for a control
  plane. Leader-only apply, snapshot reads and symlink-safe (§7.5).
- **Operator (later):** CRDs per kind with the same `spec` as the manifests. The operator is a
  `k8s` source per namespace or cluster. It mirrors CRs into the apply endpoint (delta
  mutations) and copies the object status back into `CR.status`. Kubernetes `metadata.name`
  cannot carry `_` or `/`, so the Lajstrom identity of a repo has to live in `spec` (`format`,
  `name`). Decide this now, so that the plain manifests and the CRDs share one shape (open
  question 1).

### 7.17 The config bootstrap and the ADR 0006 export

- The `repos:` bootstrap is create-only and stays that way. Its objects are `api`-owned (as
  today: "the database wins"). Document that it is a seed, not GitOps (F7).
- At startup the bootstrap runs before the sources. An entry whose repo is source-owned is
  skipped with an info line naming the source, instead of the "differs" warning.
- Migration path: `lajstrom export --owner api` writes manifests, which are reviewed and
  committed with `lajstrom.io/adopt: "true"`. Consider deprecating `repos:` once `dir` sources
  exist, so that the config file holds infrastructure only.
- ADR 0006 promises a control plane "exportable as JSON" and optionally snapshotted into the
  registry. Make that export **this manifest schema**, so that backup, export, migration and
  GitOps share one format.

### 7.18 Leave out of v1, and why

| Leave out | Why |
| --- | --- |
| Per-field ownership, `managerAllowsEdits`, "editable but managed" | The Grafana trap (F1). Kubernetes needs per-field ownership for co-writing controllers; Lajstrom has none. Operational toggles go to a state sub-resource instead. |
| Write-back from the UI to git (commits, PRs) | Grafana Git Sync shows it is a product of its own (git credentials, branch protection, conflicts). "Copy as manifest" covers 80 % of the need. |
| Templating and environment interpolation | Secrets leakage (Keycloak); one more language. Upstream tools do it. |
| Secrets, users, passwords, tokens in manifests | F9; users come from OIDC later. |
| Implicit adoption of same-named objects | F6. |
| "Force edit while owned" | Undoes the model; break-glass is suspend plus release. |
| Sync waves, hooks, health checks | One transaction plus topological order makes them unnecessary for a database-only control plane. |
| Per-object pause and `prune: disabled` annotations | Covered by release and the prune guards. Add later if real cases appear. |
| The operator | Later. The engine and the schema must not preclude it (§7.16). |

### 7.19 A possible "Decision" section for ADR 0015, condensed

1. Declarable kinds: Repo (hosted, proxy, group, with policy), RoleBinding. Later: OIDC group
   mapping, RetentionRule. Not declarable: registries, upstreams, database, keys (config file);
   users, tokens, secrets.
2. Schema: Kubernetes-style YAML (`apiVersion: lajstrom.io/v1alpha1`, `kind`, `metadata`,
   `spec`). Identity in `spec` for repos and `metadata.name` for RoleBindings. Unknown fields are
   errors. No secrets, no interpolation.
3. Ownership: each declarable object has one owner, `api` or `source:<name>`, stored on the
   object with the source path and revision as information. Sources are declared in the config
   file; their state is in the database.
4. Enforcement: one control-plane write path. Changes to the spec of an object owned by a source
   are `409 MANAGED`, naming the source, on the API, `lajstrom admin`, the bootstrap and the UI.
   Operations and revocations are never blocked.
5. Apply: `POST /api/v1/sources/<name>/apply[?dryRun=true]` takes a full set and a revision,
   validates with the API's validators, plans, orders and applies it in one transaction.
   `lajstrom apply`, `serve` (`dir` sources) and a later operator all use it.
6. Prune: only the source's own objects, only with `prune`, never an empty set without
   `--allow-empty`, never a repo with content without confirmation. Otherwise the object is
   `Orphaned`.
7. Ownership verbs: adopt (in the manifest or `--adopt`), release, transfer, source delete
   `--release|--delete`. Admin only, plannable, audited.
8. Multiple sources: an object belongs to at most one. A second claim is a per-object
   `Conflict`.
9. Suspend per source. Status per object and per source (conditions with reasons and
   revisions), in the API, the UI, metrics and audit. Periodic drift check against live state.
10. The bootstrap stays create-only and `api`-owned. The control-plane export of ADR 0006 uses
    this schema.

---

## 8. Open questions for the lead

1. **Repo identity in manifests.** One option is `spec.format` + `spec.name`, with
   `metadata.name` as a free, DNS-safe handle. That is CRD-compatible but has two names. The other
   is `metadata.name: npm.internal`, which means one name, but repo names with `_` cannot become
   CRDs later.
2. **Bindings:** a named `RoleBinding` container (recommended) or bare tuples as natural keys?
   And is exclusive mode (git is the only place access is granted) in v1 or later?
3. **Sources:** declared only in the config (recommended: infra decides) or also creatable
   through the API?
4. **Partial apply:** when one object of a revision conflicts, should the rest apply (default,
   like Backstage and Argo), or should every source be `strict` (all-or-nothing, like a
   transaction)? The transaction makes `strict` cheap. The question is which default surprises
   less.
5. **Default for `dir` sources:** `prune: true` (the Flux habit) or `false` (the Argo default)?
   With the guards in §7.6, `true` is reasonably safe.
6. **Operational toggles:** confirm `upstream.offline` as the first field of a state
   sub-resource, and the rule for classifying future fields.
7. **Error code:** `409 MANAGED` (recommended) or `403` as Grafana does. Both are contract
   changes to `api.md`.

---

## Sources

Grafana
- Provisioning: https://grafana.com/docs/grafana/latest/administration/provisioning/
- Alerting provisioning: https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/ ·
  file: https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/file-provisioning/ ·
  HTTP API: https://grafana.com/docs/grafana/latest/alerting/set-up/provision-alerting-resources/http-api-provisioning/
- Git Sync permissions: https://grafana.com/docs/grafana/latest/as-code/observability-as-code/git-sync/permissions-grafana/
- Source: https://github.com/grafana/grafana/blob/main/pkg/apimachinery/utils/manager.go ·
  https://github.com/grafana/grafana/blob/main/pkg/apimachinery/utils/meta.go ·
  https://github.com/grafana/grafana/blob/main/pkg/storage/unified/apistore/managed.go ·
  https://github.com/grafana/grafana/blob/main/pkg/registry/apis/provisioning/resources/ownership.go ·
  https://github.com/grafana/grafana/blob/main/apps/provisioning/pkg/repository/finalizers.go ·
  https://github.com/grafana/grafana/blob/main/pkg/services/provisioning/dashboards/file_reader.go
- Issues: https://github.com/grafana/grafana/issues/13823 · /75722 · /80055 · /109229 · /111525 · /114281 · /137468
- Operator: https://grafana.github.io/grafana-operator/docs/examples/common_options ·
  https://github.com/grafana/grafana-operator/issues/1597 · /2365 · /2953
- Terraform: https://github.com/grafana/terraform-provider-grafana/blob/main/docs/resources/dashboard.md ·
  https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/folder_permission ·
  https://registry.terraform.io/providers/grafana/grafana/latest/docs/resources/folder_permission_item

Harbor
- Terraform provider: https://github.com/goharbor/terraform-provider-harbor (docs/resources) ·
  issues https://github.com/goharbor/terraform-provider-harbor/issues/39 · /288 · /317 · /572 · /603
- `CONFIG_OVERWRITE_JSON`: https://github.com/goharbor/website/blob/main/docs/install-config/configure-system-settings-cli.md
  (rendered at https://goharbor.io/docs/edge/install-config/configure-system-settings-cli/)
- harbor-operator (archived): https://github.com/goharbor/harbor-operator ·
  https://github.com/goharbor/harbor-operator/blob/main/docs/day2/day2-configurations.md

JFrog Artifactory
- https://docs.jfrog.com/installation/docs/apply-artifactory-yaml-configuration-changes
- https://docs.jfrog.com/installation/docs/repositories-configurations-in-artifactory-yaml
- https://docs.jfrog.com/installation/docs/delete-an-existing-element-in-artifactory-yaml
- https://docs.jfrog.com/installation/docs/artifactory-bootstrap-yaml-file
- https://docs.jfrog.com/installation/docs/artifactory-configuration-descriptors
- Terraform: https://github.com/jfrog/terraform-provider-artifactory · issues /621 · /972 · /1102 · /1346 · /1355

Sonatype Nexus
- Script API: https://help.sonatype.com/repomanager3/integrations/rest-and-integration-api/script-api
- Terraform (community): https://github.com/datadrivers/terraform-provider-nexus · issues /107 · /534 · /617
- CasC plugin (community): https://github.com/AdaptiveConsulting/nexus-casc-plugin

Keycloak
- https://www.keycloak.org/server/importExport
- https://www.keycloak.org/operator/realm-import
- keycloak-config-cli: https://github.com/adorsys/keycloak-config-cli ·
  https://github.com/adorsys/keycloak-config-cli/blob/main/docs/MANAGED.md ·
  https://github.com/adorsys/keycloak-config-cli/blob/main/src/main/java/de/adorsys/keycloak/config/service/RealmImportService.java ·
  issues /728 · /826 · /1682

Argo CD
- https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/
- https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/
- https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/
- https://argo-cd.readthedocs.io/en/stable/user-guide/sync-options/
- https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/
- https://argo-cd.readthedocs.io/en/stable/operator-manual/applicationset/Controlling-Resource-Modification/
- Issues: https://github.com/argoproj/argo-cd/issues/18163 · /20440 · /29739

Flux
- https://fluxcd.io/flux/components/kustomize/kustomizations/
- https://github.com/fluxcd/pkg/blob/main/ssa/manager.go · https://github.com/fluxcd/pkg/blob/main/ssa/manager_delete.go
- Issues: https://github.com/fluxcd/kustomize-controller/issues/338 · /732 · /1724

Kubernetes
- https://kubernetes.io/docs/reference/using-api/server-side-apply/
- https://kubernetes.io/docs/tasks/manage-kubernetes-objects/declarative-config/

Backstage
- https://backstage.io/docs/features/software-catalog/life-of-an-entity
- https://backstage.io/docs/features/software-catalog/well-known-annotations/
- https://backstage.io/docs/features/software-catalog/external-integrations/entity-providers

Crossplane
- https://docs.crossplane.io/latest/managed-resources/managed-resources/
- https://docs.crossplane.io/latest/guides/import-existing-resources/

Terraform, Vault, GitLab
- https://developer.hashicorp.com/terraform/language/import
- https://developer.hashicorp.com/terraform/language/block/removed
- https://github.com/hashicorp/terraform-provider-vault/blob/main/website/docs/r/generic_secret.html.md
- https://github.com/gitlabhq/terraform-provider-gitlab/blob/main/docs/resources/application_settings.md
