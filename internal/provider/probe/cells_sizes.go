package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// sizeTry is one push of the size measurements: its size and what refused it, if anything.
type sizeTry struct {
	size    int64
	refusal string // "" when the registry took it
}

// measureBlobSizes measures "Largest blob measured" and "Upload: chunk size limit": a blob of
// the bound in one monolithic PUT, and one in a single PATCH and a closing PUT; each refused
// size is halved down to 1 MiB.
func (r *run) measureBlobSizes(ctx context.Context) error {
	const repo = "size"
	bound := r.opts.MaxBlobSize
	put, err := halving(bound, func(size int64) (string, error) { return r.pushBig(ctx, repo, size) })
	if err != nil {
		return err
	}
	ctx = r.in(ctx, "chunkSize")
	patch, err := halving(bound, func(size int64) (string, error) { return r.pushChunk(ctx, repo, size) })
	if err != nil {
		return err
	}
	r.set("largestBlob", largestVerdict(bound, put, patch))
	r.set("chunkSize", chunkVerdict(bound, patch))
	return nil
}

// halving pushes size bytes with push, and halves the size after each refusal while it stays
// at least minBlobTry; it returns the tries, the last one taken unless all were refused.
func halving(size int64, push func(int64) (string, error)) ([]sizeTry, error) {
	var tries []sizeTry
	for {
		refusal, err := push(size)
		if err != nil {
			return nil, err
		}
		tries = append(tries, sizeTry{size, refusal})
		if refusal == "" || size/2 < minBlobTry {
			return tries, nil
		}
		size /= 2
	}
}

// pushBig pushes a blob of size bytes, made as it is sent, with a POST and one PUT; it returns
// the refusal, "" when the registry took it.
func (r *run) pushBig(ctx context.Context, repo string, size int64) (string, error) {
	seed := fmt.Sprintf("put %s %d", r.id, size)
	d, err := patternDigest(seed, size)
	if err != nil {
		return "", err
	}
	loc, err := r.session(ctx, repo)
	if err != nil {
		return "", err
	}
	a, err := r.c.do(ctx, request{
		method: http.MethodPut, target: withDigest(loc, d), size: size,
		stream: func() io.Reader { return newPattern(seed, size) },
		header: http.Header{"Content-Type": {"application/octet-stream"}},
	})
	if err != nil {
		return "the connection: " + err.Error(), nil
	}
	if !a.ok(http.StatusCreated) {
		return a.refusal(), nil
	}
	r.blobs = append(r.blobs, pushed{repo, d})
	return "", nil
}

// pushChunk pushes a blob of size bytes in one PATCH with Content-Range, then a PUT without a
// body; it returns the refusal, "" when the registry took it.
func (r *run) pushChunk(ctx context.Context, repo string, size int64) (string, error) {
	seed := fmt.Sprintf("patch %s %d", r.id, size)
	d, err := patternDigest(seed, size)
	if err != nil {
		return "", err
	}
	loc, err := r.session(ctx, repo)
	if err != nil {
		return "", err
	}
	a, err := r.c.do(ctx, request{
		method: http.MethodPatch, target: loc.String(), size: size,
		stream: func() io.Reader { return newPattern(seed, size) },
		header: http.Header{"Content-Type": {"application/octet-stream"}, "Content-Range": {fmt.Sprintf("0-%d", size-1)}},
	})
	if err != nil {
		return "the connection: " + err.Error(), nil
	}
	if !a.ok(http.StatusAccepted, http.StatusNoContent) {
		_, _ = r.send(ctx, http.MethodDelete, loc.String())
		return a.refusal(), nil
	}
	if next := a.location(); next != nil {
		loc = next
	}
	a, err = r.send(ctx, http.MethodPut, withDigest(loc, d))
	if err != nil {
		return "", err
	}
	if !a.ok(http.StatusCreated) {
		return "the closing `PUT`: " + a.refusal(), nil
	}
	r.blobs = append(r.blobs, pushed{repo, d})
	return "", nil
}

// largestVerdict is the cell "Largest blob measured".
func largestVerdict(bound int64, put, patch []sizeTry) string {
	if taken(put) == bound && taken(patch) == bound {
		return humanSize(bound) + " in one `PUT` and in one `PATCH`; no limit met"
	}
	return triesText(bound, put, "in one `PUT`") + "; " + triesText(bound, patch, "in one `PATCH`")
}

// chunkVerdict is the cell "Upload: chunk size limit".
func chunkVerdict(bound int64, patch []sizeTry) string {
	if taken(patch) == bound {
		return "none met: " + humanSize(bound) + " in one `PATCH`"
	}
	return triesText(bound, patch, "a `PATCH`")
}

// taken is the size of the try the registry took, 0 when it took none.
func taken(tries []sizeTry) int64 {
	for _, t := range tries {
		if t.refusal == "" {
			return t.size
		}
	}
	return 0
}

// triesText describes the tries of one way of pushing.
func triesText(bound int64, tries []sizeTry, how string) string {
	ok := taken(tries)
	first := tries[0]
	switch {
	case ok == bound:
		return humanSize(bound) + " " + how
	case ok > 0:
		return fmt.Sprintf("%s %s; %s is %s", humanSize(ok), how, humanSize(first.size), first.refusal)
	}
	last := tries[len(tries)-1]
	return fmt.Sprintf("%s %s is %s", humanSize(last.size), how, last.refusal)
}

// humanSize writes a size in GiB, MiB or KiB when it is a whole number of them, with the bytes
// above a whole number of MiB, else in bytes.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<20 && n%(1<<20) == 1:
		return fmt.Sprintf("%d MiB + 1 byte", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// manifestLadder are the manifest sizes to try up to bound: 4 MiB and one byte more, then
// doubling from 8 MiB, and the bound.
func manifestLadder(bound int64) []int64 {
	var l []int64
	for _, s := range []int64{4 << 20, 4<<20 + 1} {
		if s <= bound {
			l = append(l, s)
		}
	}
	for s := int64(8 << 20); s < bound; s *= 2 {
		l = append(l, s)
	}
	if len(l) == 0 || l[len(l)-1] != bound {
		l = append(l, bound)
	}
	return l
}

// measureManifestSize measures "Manifest size limit": manifests of the ladder's sizes, padded
// with their description, pushed by digest until one is refused.
func (r *run) measureManifestSize(ctx context.Context) error {
	const repo = "size"
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	var tries []sizeTry
	for _, size := range manifestLadder(r.opts.MaxManifestSize) {
		m := r.paddedManifest(size)
		a, err := r.pushManifest(ctx, repo, m.digest.String(), m)
		t := sizeTry{size: int64(len(m.body))}
		switch {
		case err != nil:
			t.refusal = "the connection: " + err.Error()
		case !a.ok(http.StatusCreated, http.StatusOK):
			t.refusal = a.refusal()
		}
		tries = append(tries, t)
		if t.refusal != "" {
			break
		}
	}
	r.set("manifestSize", manifestVerdict(tries))
	return nil
}

// paddedManifest is a manifest of the probe of size bytes, its description filled up.
func (r *run) paddedManifest(size int64) manifest {
	m := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: emptyArtifactType,
		Config:       emptyDesc,
		Layers:       []ocispec.Descriptor{emptyDesc},
	}
	note := "lajstrom registry probe " + r.id + " "
	m.Annotations = map[string]string{ocispec.AnnotationDescription: note}
	base := int64(len(encode(m.MediaType, m).body))
	if pad := size - base; pad > 0 {
		m.Annotations[ocispec.AnnotationDescription] = note + strings.Repeat("x", int(pad))
	}
	return encode(m.MediaType, m)
}

// manifestVerdict is the cell "Manifest size limit".
func manifestVerdict(tries []sizeTry) string {
	last := tries[len(tries)-1]
	switch {
	case last.refusal == "":
		return "none met: " + humanSize(last.size) + " taken"
	case len(tries) == 1:
		return humanSize(last.size) + " is " + last.refusal
	}
	prev := tries[len(tries)-2]
	if last.size == prev.size+1 {
		return humanSize(prev.size) + "; one byte more is " + last.refusal
	}
	return humanSize(prev.size) + " taken; " + humanSize(last.size) + " is " + last.refusal
}

// measureProfile measures "Lajstrom's manifest profile": a raw manifest of the profile, with a
// file layer and a metadata layer, pushed by tag as ingest pushes, and read back.
func (r *run) measureProfile(ctx context.Context) error {
	const repo = "profile"
	if err := r.ensureBase(ctx, repo); err != nil {
		return err
	}
	file, meta := []byte("lajstrom registry probe "+r.id+"\n"), []byte(`{"probe":"`+r.id+`"}`)
	var layers [2]profile.Layer
	for i, b := range [][]byte{file, meta} {
		d, a, err := r.pushBlob(ctx, repo, b)
		if err != nil {
			return err
		}
		if !a.ok(http.StatusCreated) {
			return fmt.Errorf("push of a layer: %s", a.refusal())
		}
		layers[i] = profile.Layer{Digest: d, Size: int64(len(b))}
	}
	layers[0].MediaType, layers[0].Title = profile.MediaTypeRawFile, "probe.txt"
	m, err := profile.Build(profile.Annotations{
		Format: "raw", Repo: "probe", Name: "probe.txt", Version: "1", Created: time.Now().UTC().Truncate(time.Second),
	}, layers[:1], &layers[1])
	if err != nil {
		return err
	}
	body, d, err := profile.Encode(m)
	if err != nil {
		return err
	}
	a, err := r.pushManifest(ctx, repo, "1", manifest{mediaType: m.MediaType, body: body, digest: d})
	if err != nil {
		return err
	}
	if !a.ok(http.StatusCreated, http.StatusOK) {
		r.found.profileRefused = true
		v := "**refused**: " + a.refusal()
		if _, msg, detail := a.errorOf(); detail != "" || msg != "" {
			v += fmt.Sprintf(" %q", firstNonEmpty(detail, msg))
		}
		r.set("profile", v)
		return nil
	}
	back, err := r.getManifest(ctx, repo, "1")
	if err != nil {
		return err
	}
	switch {
	case back.status != http.StatusOK:
		r.set("profile", "accepted; read back: "+back.refusal())
		return nil
	case back.digest != d:
		r.set("profile", "accepted; **read back other bytes**")
		return nil
	}
	r.set("profile", "accepted, stored as pushed")
	return nil
}

// firstNonEmpty returns the first of ss that is not empty.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
