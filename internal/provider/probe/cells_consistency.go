package probe

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/config"
)

// The consistency measurement: lagWrites writes, each read back until every read sees it, at
// first after lagFirst and then twice as long each time, at most lagStep, for at most lagLimit:
// past the 120 s that Quay keeps a tag list (docs/registries/README.md, Consistency).
const (
	lagWrites = 3
	lagFirst  = 25 * time.Millisecond
	lagStep   = 2 * time.Second
)

// lagLimit is a variable so that the tests need not wait two minutes.
var lagLimit = 2 * time.Minute

// The reads of the consistency measurement, in the order of the cell.
const (
	readManifest = "manifest by digest"
	readTag      = "manifest by tag"
	readBlob     = "blob"
	readTagList  = "tag list"
	readCatalog  = "catalog"
)

var readOrder = []string{readManifest, readTag, readBlob, readTagList, readCatalog}

// lagSeen is what the consistency measurement found: for each read, the longest time from the
// 201 of a write until the read saw it, and whether that was the first ask.
type lagSeen struct {
	writes int
	worst  map[string]time.Duration
	late   map[string]bool // not seen at the first ask, at least once
	unseen map[string]bool // not seen within lagLimit, at least once
}

// measureConsistency measures "Read-after-write lag": lagWrites times, a new blob, read with
// HEAD until it answers 200 or a redirect (distribution on S3), then a manifest with it as its
// layer under a new tag, read until HEAD by digest answers 200, HEAD by tag the manifest's
// digest, the tag list names the tag and, when the registry has a catalog Lajstrom uses, the
// catalog the repository.
func (r *run) measureConsistency(ctx context.Context) error {
	const repo = "lag"
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	s := lagSeen{worst: map[string]time.Duration{}, late: map[string]bool{}, unseen: map[string]bool{}}
	for i := range lagWrites {
		data := []byte(fmt.Sprintf("lajstrom registry probe %s: lag %d", r.id, i))
		d, a, err := r.pushBlob(ctx, repo, data)
		if err != nil {
			return err
		}
		if !a.ok(http.StatusCreated) {
			return fmt.Errorf("push of a blob to %s: %s", r.name(repo), a.refusal())
		}
		if err := r.watch(ctx, &s, map[string]func(context.Context) (bool, error){
			readBlob: func(ctx context.Context) (bool, error) {
				a, err := r.send(ctx, http.MethodHead, r.path(repo, "blobs/"+d.String()))
				return a.status == http.StatusOK || a.location() != nil && a.status/100 == 3, err // a blob on S3 redirects
			},
		}); err != nil {
			return err
		}
		m := r.layerManifest(fmt.Sprintf("lag %d", i), ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(data))})
		tag := fmt.Sprintf("lag-%d", i)
		if a, err = r.pushManifest(ctx, repo, tag, m); err != nil {
			return err
		}
		if !a.ok(http.StatusCreated, http.StatusOK) {
			return fmt.Errorf("push of a manifest to %s:%s: %s", r.name(repo), tag, a.refusal())
		}
		if err := r.watch(ctx, &s, r.manifestReads(repo, tag, m.digest)); err != nil {
			return err
		}
		s.writes++
	}
	r.found.lagged, r.found.lagUnbounded = len(s.late) > 0, len(s.unseen) > 0
	for _, w := range s.worst {
		r.found.lag = max(r.found.lag, w)
	}
	r.set("consistency", lagVerdict(s))
	return nil
}

// layerManifest is an image manifest of the empty config and one layer, the note as its
// description: a write of the consistency measurement, an image of the routing words.
func (r *run) layerManifest(note string, layer ocispec.Descriptor) manifest {
	m := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      emptyDesc,
		Layers:      []ocispec.Descriptor{layer},
		Annotations: map[string]string{ocispec.AnnotationDescription: "lajstrom registry probe " + r.id + ": " + note},
	}
	return encode(m.MediaType, m)
}

// manifestReads are the reads of a manifest pushed to repo under tag with digest d.
func (r *run) manifestReads(repo, tag string, d digest.Digest) map[string]func(context.Context) (bool, error) {
	reads := map[string]func(context.Context) (bool, error){
		readManifest: func(ctx context.Context) (bool, error) {
			a, err := r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "manifests/"+d.String()), header: manifestAccept})
			return a.status == http.StatusOK, err
		},
		readTag: func(ctx context.Context) (bool, error) {
			a, err := r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "manifests/"+tag), header: manifestAccept})
			return a.status == http.StatusOK && a.header.Get("Docker-Content-Digest") == d.String(), err
		},
		readTagList: func(ctx context.Context) (bool, error) {
			tags, _, err := r.listTags(ctx, r.path(repo, "tags/list"))
			return slices.Contains(tags, tag), err
		},
	}
	if r.found.catalog && r.report.Capabilities.Catalog {
		reads[readCatalog] = func(ctx context.Context) (bool, error) {
			names, _, err := r.catalogBelow(ctx, r.scratch)
			return slices.Contains(names, r.name(repo)), err
		}
	}
	return reads
}

// watch asks each of reads until it sees the write the registry acknowledged just before, and
// records into s how long each took. A read that has not seen it after lagLimit is unseen.
func (r *run) watch(ctx context.Context, s *lagSeen, reads map[string]func(context.Context) (bool, error)) error {
	at, wait := time.Now(), lagFirst
	pending := map[string]bool{}
	for k := range reads {
		pending[k] = true
	}
	for first := true; ; first = false {
		for _, k := range readOrder {
			if !pending[k] {
				continue
			}
			ok, err := reads[k](ctx)
			if err != nil {
				return err
			}
			if ok {
				delete(pending, k)
				if !first {
					s.late[k] = true
					s.worst[k] = max(s.worst[k], time.Since(at))
				} else if _, seen := s.worst[k]; !seen {
					s.worst[k] = 0
				}
			}
		}
		if len(pending) == 0 {
			return nil
		}
		if time.Since(at) >= lagLimit {
			for k := range pending {
				s.unseen[k], s.late[k], s.worst[k] = true, true, max(s.worst[k], lagLimit)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, lagStep)
	}
}

// lagVerdict is the cell "Read-after-write lag".
func lagVerdict(s lagSeen) string {
	var asked []string
	for _, k := range readOrder {
		if _, ok := s.worst[k]; ok {
			asked = append(asked, k)
		}
	}
	if len(s.late) == 0 {
		return fmt.Sprintf("none: each read saw each of %d writes at once (%s)", s.writes, strings.Join(asked, ", "))
	}
	parts := make([]string, 0, len(asked))
	for _, k := range asked {
		switch {
		case s.unseen[k]:
			parts = append(parts, fmt.Sprintf("%s **not within %s**", k, lagLimit))
		case s.late[k]:
			parts = append(parts, fmt.Sprintf("%s **after %s**", k, roundLag(s.worst[k])))
		default:
			parts = append(parts, k+" at once")
		}
	}
	return fmt.Sprintf("%s; the worst of %d writes", strings.Join(parts, ", "), s.writes)
}

// roundLag rounds a measured lag up to a tenth of a second, as the cell says it.
func roundLag(d time.Duration) time.Duration {
	return (d + 100*time.Millisecond - 1).Truncate(100 * time.Millisecond)
}

// consistencyOverrides are the overrides of "Overrides needed" that the consistency
// measurement asks for: a registry seen to lag is eventual, with a bound at least the worst
// lag, in whole seconds.
func consistencyOverrides(caps Capabilities, f facts) []string {
	if !f.lagged {
		return nil
	}
	if f.lagUnbounded {
		return []string{fmt.Sprintf("`consistency: %s`; a read did not see a write within %s", config.ConsistencyEventual, lagLimit)}
	}
	bound := f.lag.Truncate(time.Second) + time.Second
	known, _ := time.ParseDuration(caps.ConsistencyBound)
	switch {
	case caps.Consistency != config.ConsistencyEventual:
		return []string{fmt.Sprintf("`consistency: %s`, `consistencyBound: %s`", config.ConsistencyEventual, bound)}
	case known < f.lag:
		return []string{fmt.Sprintf("`consistencyBound: %s`", bound)}
	}
	return nil
}
