package ingest

import (
	"fmt"
	"maps"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// The audit actions of a publish (docs/spec/rbac.md §5 and §7).
const (
	actionPush       = "push"
	actionProxyFetch = "proxy.fetch"
)

// setOrigin writes the origin of a proxied artifact on a manifest that Build made: the URL and
// the checksum the fetcher verified, the upstream's publish time as created when it gives one,
// and no lajstrom.uploaded-by, since nobody uploaded it
// (docs/spec/manifest-profile.md §3.1). The map is copied, the plugin's is left alone.
func setOrigin(m *ocispec.Manifest, o *format.Origin) {
	a := maps.Clone(m.Annotations)
	if a == nil {
		a = map[string]string{}
	}
	delete(a, profile.AnnotationUploadedBy)
	a[profile.AnnotationOriginURL] = o.URL
	a[profile.AnnotationOriginChecksum] = o.Checksum
	if !o.Published.IsZero() {
		a[ocispec.AnnotationCreated] = o.Published.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	m.Annotations = a
}

// checkOrigin refuses a manifest that carries a lajstrom.origin.* key and does not come from
// the proxy fetcher (refuseOrigin). Inspect reads the keys as any annotation.
func (pub *publish) checkOrigin() error {
	if pub.origin != nil || pub.readOnly {
		return nil
	}
	return refuseOrigin(pub.manifest.Annotations)
}

// refuseOrigin refuses the annotations a of a manifest a client pushed when a key starts with
// lajstrom.origin.: a client must not pass its bytes off as fetched from an upstream, and only
// the proxy fetcher writes those keys (docs/spec/manifest-profile.md §3.1). It checks a
// version's manifest (checkOrigin) and a referrer of a managed repo (PutReferrer) alike.
func refuseOrigin(a map[string]string) error {
	for k := range a {
		if strings.HasPrefix(k, profile.AnnotationOriginPrefix) {
			return fmt.Errorf("%w: annotation %s: only a proxy repo's fetcher writes the %s keys", ErrInvalid, k,
				profile.AnnotationOriginPrefix)
		}
	}
	return nil
}

// auditDetails are the details of pub's audit row before the request's own: for a proxied
// artifact the upstream, the URL, the checksum and the size of its files (docs/spec/rbac.md §7);
// nil for any other publish.
func (pub *publish) auditDetails() map[string]any {
	if pub.origin == nil {
		return nil
	}
	var size int64
	for _, l := range pub.parsed.Files {
		size += l.Size
	}
	return map[string]any{"upstream": pub.origin.Upstream, "url": pub.origin.URL, "checksum": pub.origin.Checksum, "size": size}
}
