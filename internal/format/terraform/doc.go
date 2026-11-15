// Package terraform is the terraform format plugin (plugin name and artifact type "terraform"):
// Terraform's module registry protocol, modules.v1, which Terraform and OpenTofu read. See
// docs/spec/http-routing.md §3 (terraform), docs/spec/manifest-profile.md and ADR 0023.
//
// The model: a repo is a Terraform namespace, so the module address art.example.com/infra/vpc/aws
// is the module vpc/aws of the repo terraform/infra. A module is a package, lajstrom.name
// <name>/<system> as written, whose package path is PackagePath(EscapeComponent(name),
// EscapeComponent(system)); a version is a SemVer 2.0.0 version without build metadata, with no
// variant, so the tag is Escape(version). The one file layer is the module archive, a
// gzip-compressed tar titled <name>-<system>-<version>.tar.gz; there is no metadata layer.
//
// Build makes the manifest of a publish and Validate checks one pushed with oras; both read the
// archive whole (ReadArchive): a gzip stream of a tar whose entries stay inside it. A publish
// streams into the registry (ADR 0014), the archive read by the inspector on its way.
//
// The plugin registers itself with format.Register when the package is imported.
package terraform
