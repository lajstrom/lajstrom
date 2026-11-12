package profile

import (
	"strings"

	"github.com/opencontainers/go-digest"
)

// keepPrefix starts every keep-tag; no tag of the referrers schema (sha256-<hex>) or of a
// version starts with it.
const keepPrefix = "_keep."

// maxTag is the longest tag of the distribution spec.
const maxTag = 128

// KeepTag is the keep-tag of the manifest or index d that an oci proxy repo holds
// (docs/spec/proxy-and-group.md §11.6): _keep.<algorithm>.<encoded>, cut to 128 characters.
// It is written once and never moved, so that a garbage collection of untagged manifests
// leaves d when the upstream's tag that named it moves on.
func KeepTag(d digest.Digest) string {
	t := keepPrefix + d.Algorithm().String() + "." + d.Encoded()
	if len(t) > maxTag {
		t = t[:maxTag]
	}
	return t
}

// IsKeepTag reports whether tag has the form of a keep-tag: tags/list of an oci proxy leaves
// it out, and an upstream tag of this form is not served.
func IsKeepTag(tag string) bool { return strings.HasPrefix(tag, keepPrefix) }
