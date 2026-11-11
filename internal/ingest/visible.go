package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

const (
	// defaultVisibilityTimeout is Options.VisibilityTimeout when it is zero: it covers the lags
	// reported of ECR and GHCR (docs/registries/README.md, Consistency).
	defaultVisibilityTimeout = 30 * time.Second
	// visibilityRetryAfter is the Retry-After of a publish the registry did not serve in time.
	visibilityRetryAfter = 30 * time.Second
)

// pushed is what one push wrote to the repository rr, read back by awaitVisible: the manifest
// by its digest, under its tag (unless ""), and the blobs it names. referrers names a
// referrer: it must be among its subject's referrers too.
type pushed struct {
	name     string
	rr       provider.Repository
	prov     provider.Provider
	manifest ocispec.Descriptor
	tag      string
	blobs    []ocispec.Descriptor
	subject  *ocispec.Descriptor
}

// awaitVisible is the visibility barrier: it reads back what w pushed until the registry
// serves all of it, so that nothing confirms what a reader cannot get yet. On a strongly
// consistent registry it reads the manifest by digest, once, a HEAD; on any other, the tag,
// every blob and, for a referrer, the subject's referrers too, and reads again what it did not
// see, with backoff, for the visibility timeout (twice it for a referrer), or the registry's
// consistencyBound when that is longer. Past it the error is a *NotVisibleError, which names
// what the registry did not serve and wraps a *provider.UnavailableError of 30 s: the
// frontends and the facade answer 503 with Retry-After, the push stays, and the same publish
// again finds it.
func (p *Pipeline) awaitVisible(ctx context.Context, w pushed) error {
	caps, err := w.prov.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("ingest: capabilities of the registry of %s: %w", w.name, err)
	}
	reads := w.reads(caps.Consistency == provider.ConsistencyStrong)
	timeout := max(p.visibility, caps.ConsistencyBound)
	if w.subject != nil {
		timeout *= 2
	}
	start := time.Now()
	ok, err := provider.ReadBack{Timeout: timeout}.Await(ctx, func(ctx context.Context) (bool, error) {
		for what, read := range reads {
			seen, err := read(ctx)
			if err != nil {
				return false, fmt.Errorf("ingest: read back %s of %s@%s: %w", what, w.name, w.manifest.Digest, err)
			}
			if seen {
				delete(reads, what)
			}
		}
		return len(reads) == 0, nil
	})
	if err != nil {
		return err
	}
	if !ok {
		nv := notVisible(w, reads, timeout)
		p.log.WarnContext(ctx, "ingest: the registry took a push and does not serve it yet; the publish is not confirmed",
			"name", w.name, "digest", w.manifest.Digest.String(), "tag", w.tag, "missing", strings.Join(nv.Missing(), ", "),
			"waited", timeout.String())
		return nv
	}
	if d := time.Since(start); d > time.Second {
		p.log.InfoContext(ctx, "ingest: the registry served a push late", "name", w.name, "digest", w.manifest.Digest.String(),
			"after", d.Round(time.Millisecond).String())
	}
	return nil
}

// NotVisibleError is the error of a push that the registry took and did not serve within the
// visibility barrier's bound (awaitVisible): what it did not serve yet, by the digests and the
// tag that were pushed, so that an answer can name them and a client tell a registry that lags
// from one that lost the push. Nothing of the registry's own is in it: Name is the OCI name the
// provider got, the repo's base and the package path, without the instance prefix of the
// registry. It wraps a *provider.UnavailableError of 30 s, which the frontends and the facade
// answer 503 with Retry-After; the same publish again completes it.
type NotVisibleError struct {
	Name     string          // the OCI repository: the repo's base and the package path
	Digest   digest.Digest   // the manifest pushed
	Manifest bool            // the manifest is not served by its digest
	Tag      string          // the tag that does not name the manifest yet; "" when it does, or the push has none
	Blobs    []digest.Digest // the blobs not served, sorted
	Subject  digest.Digest   // of a referrer: the subject whose referrers do not list it yet; "" when they do, or none
	Waited   time.Duration   // how long the barrier read

	unavailable *provider.UnavailableError
}

func (e *NotVisibleError) Error() string {
	return fmt.Sprintf("ingest: %s@%s: %s", e.Name, e.Digest, e.unavailable)
}

// Unwrap returns the *provider.UnavailableError, so that errors.Is matches
// provider.ErrUnavailable and provider.RetryAfter finds its 30 s.
func (e *NotVisibleError) Unwrap() error { return e.unavailable }

// Missing names what the registry does not serve yet, in this order: "the manifest", "the tag
// <tag>", "the blob <digest>" for each, "the referrers of <digest>".
func (e *NotVisibleError) Missing() []string {
	var missing []string
	if e.Manifest {
		missing = append(missing, readManifest.String())
	}
	if e.Tag != "" {
		missing = append(missing, readback{kindTag, e.Tag}.String())
	}
	for _, b := range e.Blobs {
		missing = append(missing, readback{kindBlob, b.String()}.String())
	}
	if e.Subject != "" {
		missing = append(missing, readback{kindReferrers, e.Subject.String()}.String())
	}
	return missing
}

// notVisible is the NotVisibleError of w, whose reads left were not seen within waited.
func notVisible(w pushed, left map[readback]func(context.Context) (bool, error), waited time.Duration) *NotVisibleError {
	e := &NotVisibleError{Name: w.name, Digest: w.manifest.Digest, Waited: waited}
	for r := range left {
		switch r.kind {
		case kindManifest:
			e.Manifest = true
		case kindTag:
			e.Tag = r.ref
		case kindBlob:
			e.Blobs = append(e.Blobs, digest.Digest(r.ref))
		case kindReferrers:
			e.Subject = digest.Digest(r.ref)
		}
	}
	slices.Sort(e.Blobs)
	e.unavailable = &provider.UnavailableError{RetryAfter: visibilityRetryAfter,
		Reason: fmt.Sprintf("the registry took the push and did not serve %s within %s; the same publish again completes it",
			strings.Join(e.Missing(), ", "), waited)}
	return e
}

// readback is one read of awaitVisible: of the manifest by its digest, of the tag, of a blob,
// or of the subject's referrers, with the tag or the digest it reads.
type readback struct {
	kind string // kindManifest, kindTag, kindBlob, kindReferrers
	ref  string // the tag, the blob's digest, the subject's digest; "" for the manifest
}

// The kinds of a readback.
const (
	kindManifest  = "manifest"
	kindTag       = "tag"
	kindBlob      = "blob"
	kindReferrers = "referrers"
)

// readManifest is the read of the manifest by its digest.
var readManifest = readback{kind: kindManifest}

// String is what the read reads, for a message: "the manifest", "the tag 1.0", "the blob
// sha256:…", "the referrers of sha256:…".
func (r readback) String() string {
	switch r.kind {
	case kindManifest:
		return "the manifest"
	case kindReferrers:
		return "the referrers of " + r.ref
	}
	return "the " + r.kind + " " + r.ref
}

// reads are the reads of awaitVisible, by what they read: the manifest by digest alone when
// strong, else everything w pushed.
func (w pushed) reads(strong bool) map[readback]func(context.Context) (bool, error) {
	reads := map[readback]func(context.Context) (bool, error){
		readManifest: func(ctx context.Context) (bool, error) { return w.rr.Exists(ctx, w.manifest) },
	}
	if strong {
		return reads
	}
	if w.tag != "" {
		reads[readback{kindTag, w.tag}] = func(ctx context.Context) (bool, error) {
			desc, err := w.rr.Resolve(ctx, w.tag)
			if errors.Is(err, provider.ErrNotFound) {
				return false, nil
			}
			return err == nil && desc.Digest == w.manifest.Digest, err
		}
	}
	seen := map[digest.Digest]bool{}
	for _, b := range w.blobs {
		if seen[b.Digest] {
			continue
		}
		seen[b.Digest] = true
		reads[readback{kindBlob, b.Digest.String()}] = func(ctx context.Context) (bool, error) { return w.rr.Exists(ctx, b) }
	}
	if w.subject != nil {
		reads[readback{kindReferrers, w.subject.Digest.String()}] = func(ctx context.Context) (bool, error) {
			found := false
			err := w.rr.Referrers(ctx, *w.subject, "", func(page []ocispec.Descriptor) error {
				for _, r := range page {
					found = found || r.Digest == w.manifest.Digest
				}
				return nil
			})
			if errors.Is(err, provider.ErrNotFound) {
				return false, nil
			}
			return found, err
		}
	}
	return reads
}

// readBlob reads the blob desc of rr again, which the backend did not serve, until it does or
// *deadline: timeout, or the registry's consistencyBound when longer, from the first blob of a
// manifest read again, so that the blobs of one manifest share one wait. A client, or Upload,
// pushed the blob a moment before, and an eventually consistent registry can answer 404 for it
// for a while (docs/registries/README.md, Consistency). A strongly consistent registry is not
// asked again.
func readBlob(ctx context.Context, prov provider.Provider, rr provider.Repository, desc ocispec.Descriptor, timeout time.Duration, deadline *time.Time) (bool, error) {
	caps, err := prov.Capabilities(ctx)
	if err != nil || caps.Consistency == provider.ConsistencyStrong {
		return false, err
	}
	if deadline.IsZero() {
		*deadline = time.Now().Add(max(timeout, caps.ConsistencyBound))
	}
	return provider.ReadBack{Timeout: time.Until(*deadline)}.Await(ctx, func(ctx context.Context) (bool, error) {
		return rr.Exists(ctx, desc)
	})
}

// published is what pub pushed, for awaitVisible.
func (pub *publish) published() pushed {
	return pushed{name: pub.name, rr: pub.rr, prov: pub.prov, tag: pub.tag,
		manifest: ocispec.Descriptor{MediaType: pub.manifest.MediaType, Digest: pub.digest, Size: int64(len(pub.raw))},
		blobs:    append([]ocispec.Descriptor{pub.manifest.Config}, pub.manifest.Layers...)}
}
