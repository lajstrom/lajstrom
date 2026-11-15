// Package harbor is the Harbor Provider: the generic provider's data path for /v2, below one
// Harbor project, and Harbor's REST API (/api/v2.0) for what /v2 does not say or do.
//
// It registers the registries[].type "harbor" with provider.Register; cmd/lajstrom imports it
// for that. The project is the first component of registries[].prefix, which the type
// requires; the rest of the prefix is a path inside the project. registries[].auth, a robot
// account's name and secret, answers /v2 as for type generic (Harbor's token service takes
// it) and goes to the REST API as Basic credentials, to the registry's host only.
//
// EnsureRepository checks that the project exists (HEAD /api/v2.0/projects?project_name=,
// which any account may ask), once per knownFor: Harbor creates a repository on its first
// manifest inside a project, never a project. With registries[].harbor.createProject it
// creates a missing project, private, with Harbor's default quota (POST /api/v2.0/projects, a
// system robot's or an administrator's); without, a missing project is a *RejectedError
// whose hint names the key. The first answer of Capabilities, the probe at start, also checks
// the project, whether the account may list its repositories (Capabilities.Catalog), and its
// immutability rules (Capabilities.ImmutableTags). ListRepositories reads the project's
// repositories from the REST API, every page; DeleteRepository deletes one there, with its
// artifacts. A manifest's delete stays the data path's, DELETE of /v2 by digest. Harbor's
// garbage collector, which frees the space, is the Harbor administrator's to run.
//
// Errors of /v2 are the generic provider's: a project's quota is provider.ErrLimit, a tag an
// immutability rule holds provider.ErrConflict (#357, docs/registries/harbor.md). Errors of
// the REST API wrap the sentinels of package provider where one applies (translate), and a
// 403 names the permission the account lacks.
//
// Tested against an httptest fake of the REST API and of /v2.
package harbor
