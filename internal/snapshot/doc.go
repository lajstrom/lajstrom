// Package snapshot writes the control plane of the index into the registry and reads it back
// (#76; docs/spec/manifest-profile.md §9, docs/spec/index-schema.md, Snapshots of the control
// plane): one manifest per snapshot in the repository lajstrom/control-plane below the instance
// prefix of a registry, under a tag of its time and digest, its one layer the document of the
// rows, gzip'd, and sealed with a key derived from the master key unless the operator asks
// otherwise. It writes and reads through the provider, as everything that talks to the registry
// does; the index's index.Snapshotter reads and writes the rows. lajstrom admin snapshot is its
// command.
package snapshot
