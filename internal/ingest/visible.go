package ingest

import (
	"context"
	"errors"
	"fmt"
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
// consistencyBound when that is longer. Past it the error is a *provider.UnavailableError of
// 30 s, which the frontends and the facade answer 503 with Retry-After: the push stays, and the
// same publish again finds it.
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
		missing := make([]string, 0, len(reads))
		for what := range reads {
			missing = append(missing, what)
		}
		p.log.WarnContext(ctx, "ingest: the registry took a push and does not serve it yet; the publish is not confirmed",
			"name", w.name, "digest", w.manifest.Digest.String(), "tag", w.tag, "missing", strings.Join(missing, ", "),
			"waited", timeout.String())
		return fmt.Errorf("ingest: %s@%s: %w", w.name, w.manifest.Digest, &provider.UnavailableError{RetryAfter: visibilityRetryAfter,
			Reason: fmt.Sprintf("the registry took the push and did not serve it within %s; the same publish again completes it", timeout)})
	}
	if d := time.Since(start); d > time.Second {
		p.log.InfoContext(ctx, "ingest: the registry served a push late", "name", w.name, "digest", w.manifest.Digest.String(),
			"after", d.Round(time.Millisecond).String())
	}
	return nil
}

// reads are the reads of awaitVisible, by what they read: the manifest by digest alone when
// strong, else everything w pushed.
func (w pushed) reads(strong bool) map[string]func(context.Context) (bool, error) {
	reads := map[string]func(context.Context) (bool, error){
		"the manifest": func(ctx context.Context) (bool, error) { return w.rr.Exists(ctx, w.manifest) },
	}
	if strong {
		return reads
	}
	if w.tag != "" {
		reads["the tag "+w.tag] = func(ctx context.Context) (bool, error) {
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
		reads["the blob "+b.Digest.String()] = func(ctx context.Context) (bool, error) { return w.rr.Exists(ctx, b) }
	}
	if w.subject != nil {
		reads["the referrers of "+w.subject.Digest.String()] = func(ctx context.Context) (bool, error) {
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

// published is what pub pushed, for awaitVisible.
func (pub *publish) published() pushed {
	return pushed{name: pub.name, rr: pub.rr, prov: pub.prov, tag: pub.tag,
		manifest: ocispec.Descriptor{MediaType: pub.manifest.MediaType, Digest: pub.digest, Size: int64(len(pub.raw))},
		blobs:    append([]ocispec.Descriptor{pub.manifest.Config}, pub.manifest.Layers...)}
}
