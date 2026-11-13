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
// 2xx of a write (or a delete) until the read saw it, and whether that was the first ask.
type lagSeen struct {
	writes  int // the writes, or the deletes, measured
	worst   map[string]time.Duration
	late    map[string]bool // not seen at the first ask, at least once
	unseen  map[string]bool // not seen within lagLimit, at least once
	refused string          // of the deletes: the refusal of the first, which ends them
}

func newLagSeen() lagSeen {
	return lagSeen{worst: map[string]time.Duration{}, late: map[string]bool{}, unseen: map[string]bool{}}
}

// lagWrite is a manifest the consistency measurement pushed: its tag and digest.
type lagWrite struct {
	tag    string
	digest digest.Digest
}

// measureConsistency measures "Read-after-write lag": lagWrites times, a new blob, read with
// HEAD until it answers 200 or a redirect (distribution on S3), then a manifest with it as its
// layer under a new tag, read until HEAD by digest answers 200, HEAD by tag the manifest's
// digest, the tag list names the tag and, when the registry has a catalog Lajstrom uses, the
// catalog the repository. Before each manifest it reads the tag list and the catalog once, as a
// client that lists before it pushes does, so that a registry that caches its lists serves the
// stale one after the push (Quay, #803). Then each manifest is deleted (measureDeleteLag).
func (r *run) measureConsistency(ctx context.Context) error {
	const repo = "lag"
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	s := newLagSeen()
	var pushed []lagWrite
	for i := range lagWrites {
		w, err := r.lagWrite(ctx, repo, i, &s)
		if err != nil {
			return err
		}
		s.writes++
		pushed = append(pushed, w)
	}
	del, err := r.measureDeleteLag(ctx, repo, pushed)
	if err != nil {
		return err
	}
	for _, seen := range []lagSeen{s, del} {
		r.found.lagged = r.found.lagged || len(seen.late) > 0
		r.found.lagUnbounded = r.found.lagUnbounded || len(seen.unseen) > 0
		for _, w := range seen.worst {
			r.found.lag = max(r.found.lag, w)
		}
	}
	r.set("consistency", consistencyVerdict(s, del))
	return nil
}

// lagWrite is the write i of measureConsistency, its reads recorded into s: the blob, then the
// manifest after the reads of the lists.
func (r *run) lagWrite(ctx context.Context, repo string, i int, s *lagSeen) (lagWrite, error) {
	data := []byte(fmt.Sprintf("lajstrom registry probe %s: lag %d", r.id, i))
	d, a, err := r.pushBlob(ctx, repo, data)
	if err != nil {
		return lagWrite{}, err
	}
	if !a.ok(http.StatusCreated) {
		return lagWrite{}, fmt.Errorf("push of a blob to %s: %s", r.name(repo), a.refusal())
	}
	if err := r.watch(ctx, s, map[string]func(context.Context) (bool, error){
		readBlob: func(ctx context.Context) (bool, error) {
			a, err := r.send(ctx, http.MethodHead, r.path(repo, "blobs/"+d.String()))
			return a.status == http.StatusOK || a.location() != nil && a.status/100 == 3, err // a blob on S3 redirects
		},
	}); err != nil {
		return lagWrite{}, err
	}
	m := r.layerManifest(fmt.Sprintf("lag %d", i), ocispec.Descriptor{MediaType: "application/octet-stream", Digest: d, Size: int64(len(data))})
	tag := fmt.Sprintf("lag-%d", i)
	if err := r.primeLists(ctx, repo); err != nil {
		return lagWrite{}, err
	}
	if a, err = r.pushManifest(ctx, repo, tag, m); err != nil {
		return lagWrite{}, err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		return lagWrite{}, fmt.Errorf("push of a manifest to %s:%s: %s", r.name(repo), tag, a.refusal())
	}
	if err := r.watch(ctx, s, r.manifestReads(repo, tag, m.digest)); err != nil {
		return lagWrite{}, err
	}
	return lagWrite{tag: tag, digest: m.digest}, nil
}

// primeLists reads the tag list of repo and, when the registry has a catalog Lajstrom uses, the
// catalog once, as a client does before a push; what they answer does not matter.
func (r *run) primeLists(ctx context.Context, repo string) error {
	if _, _, err := r.listTags(ctx, r.path(repo, "tags/list")); err != nil {
		return err
	}
	if r.found.catalog && r.report.Capabilities.Catalog {
		if _, _, err := r.catalogBelow(ctx, r.scratch); err != nil {
			return err
		}
	}
	return nil
}

// measureDeleteLag deletes each manifest pushed by its digest, one after the other, and reads
// it until HEAD by digest and by tag answer 404 and the tag list leaves the tag out. The first
// delete the registry refuses ends them, and so does the first a read did not see within
// lagLimit: a registry that keeps a tag after the delete of its manifest would hold the probe
// lagLimit per delete.
func (r *run) measureDeleteLag(ctx context.Context, repo string, pushed []lagWrite) (lagSeen, error) {
	s := newLagSeen()
	for _, w := range pushed {
		a, err := r.send(ctx, http.MethodDelete, r.path(repo, "manifests/"+w.digest.String()))
		if err != nil {
			return s, err
		}
		if !a.ok(http.StatusAccepted, http.StatusOK) {
			s.refused = a.refusal()
			return s, nil
		}
		if err := r.watch(ctx, &s, r.goneReads(repo, w.tag, w.digest)); err != nil {
			return s, err
		}
		s.writes++
		if len(s.unseen) > 0 {
			return s, nil
		}
	}
	return s, nil
}

// goneReads are the reads of a manifest deleted from repo, that had tag and digest d: each sees
// the delete when the manifest is not there any more.
func (r *run) goneReads(repo, tag string, d digest.Digest) map[string]func(context.Context) (bool, error) {
	return map[string]func(context.Context) (bool, error){
		readManifest: func(ctx context.Context) (bool, error) {
			a, err := r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "manifests/"+d.String()), header: manifestAccept})
			return a.status == http.StatusNotFound, err
		},
		readTag: func(ctx context.Context) (bool, error) {
			a, err := r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "manifests/"+tag), header: manifestAccept})
			return a.status == http.StatusNotFound, err
		},
		readTagList: func(ctx context.Context) (bool, error) {
			tags, _, err := r.listTags(ctx, r.path(repo, "tags/list"))
			return !slices.Contains(tags, tag), err
		},
	}
}

// layerManifest is an image manifest of the empty config and one layer, the note as its
// description: a write of the consistency measurement, an image of the routing words.
func (r *run) layerManifest(note string, layer ocispec.Descriptor) manifest {
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: emptyArtifactType,
		Config:       emptyDesc,
		Layers:       []ocispec.Descriptor{layer},
		Annotations:  map[string]string{ocispec.AnnotationDescription: "lajstrom registry probe " + r.id + ": " + note},
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

// consistencyVerdict is the cell "Read-after-write lag": the writes (lagVerdict), then the
// deletes when there were any.
func consistencyVerdict(writes, deletes lagSeen) string {
	v := lagVerdict(writes)
	switch {
	case deletes.refused != "":
		return v + "; deletes not measured: the registry refused a delete by digest (" + deletes.refused + ")"
	case deletes.writes == 0:
		return v
	case len(deletes.late) == 0:
		return fmt.Sprintf("%s; each read saw each of %s at once (%s)", v, deletesOf(deletes.writes), strings.Join(askedReads(deletes), ", "))
	}
	return fmt.Sprintf("%s; after a delete: %s; the worst of %s", v, strings.Join(lagParts(deletes), ", "), deletesOf(deletes.writes))
}

// deletesOf is n deletes, in words.
func deletesOf(n int) string {
	if n == 1 {
		return "1 delete"
	}
	return fmt.Sprintf("%d deletes", n)
}

// lagVerdict is the part of the cell "Read-after-write lag" about the writes.
func lagVerdict(s lagSeen) string {
	if len(s.late) == 0 {
		return fmt.Sprintf("none: each read saw each of %d writes at once (%s)", s.writes, strings.Join(askedReads(s), ", "))
	}
	return fmt.Sprintf("%s; the worst of %d writes", strings.Join(lagParts(s), ", "), s.writes)
}

// askedReads are the reads of s, in the order of the cell.
func askedReads(s lagSeen) []string {
	var asked []string
	for _, k := range readOrder {
		if _, ok := s.worst[k]; ok {
			asked = append(asked, k)
		}
	}
	return asked
}

// lagParts is each read of s with its worst time, in the order of the cell.
func lagParts(s lagSeen) []string {
	asked := askedReads(s)
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
	return parts
}

// roundLag rounds a measured lag up to a tenth of a second, as the cell says it.
func roundLag(d time.Duration) time.Duration {
	return (d + 100*time.Millisecond - 1).Truncate(100 * time.Millisecond)
}

// consistencyOverrides are the overrides of "Overrides needed" that the consistency
// measurement asks for: a registry seen to lag, after a write or a delete, is eventual, with a
// bound at least the worst lag, in whole seconds.
func consistencyOverrides(caps Capabilities, f facts) []string {
	if !f.lagged {
		return nil
	}
	if f.lagUnbounded {
		return []string{fmt.Sprintf("`consistency: %s`; a read did not see a write or a delete within %s", config.ConsistencyEventual, lagLimit)}
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
