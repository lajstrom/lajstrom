package probe

import (
	"context"
	"fmt"
	"net/http"

	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// deleteSeen is what the delete measurements found.
type deleteSeen struct {
	byDigest  answer // DELETE manifests/<M1>, M1 tagged t1 and t2
	tagAfter  answer // GET t1 after it
	blobAfter answer // HEAD of the empty blob, which only M1 used, after it
	subject   answer // DELETE of a manifest with a referrer; asked when byDigest worked
	refAfter  answer // GET the referrer after it, when the subject went
	indexed   answer // DELETE of a manifest an image index lists; asked when byDigest worked
	byTag     answer // DELETE manifests/u1, M3 pushed by u1 only
	tagged    answer // GET M3 by digest after it
	byDigTag  answer // GET M4 by digest after DELETE u2; M4 pushed by digest, then tagged u2
}

// measureDeletes measures "Delete a manifest by digest" and "Delete by tag".
func (r *run) measureDeletes(ctx context.Context) error {
	const repo = "del"
	var s deleteSeen
	m1 := r.newManifest("delete by digest", "", nil)
	if err := r.mustPush(ctx, repo, "t1", m1); err != nil {
		return err
	}
	if err := r.mustPush(ctx, repo, "t2", m1); err != nil {
		return err
	}
	var err error
	if s.byDigest, err = r.send(ctx, http.MethodDelete, r.path(repo, "manifests/"+m1.digest.String())); err != nil {
		return err
	}
	if s.byDigest.ok(http.StatusAccepted, http.StatusOK) {
		if err := r.afterDigestDelete(ctx, repo, &s); err != nil {
			return err
		}
	}
	r.found.tagsStay = s.byDigest.ok(http.StatusAccepted, http.StatusOK) && s.tagAfter.status == http.StatusOK
	r.found.deleteRefused = !s.byDigest.ok(http.StatusAccepted, http.StatusOK)
	r.set("deleteDigest", digestDeleteVerdict(s))
	if err := r.measureTagDelete(r.in(ctx, "deleteTag"), repo, &s); err != nil {
		return err
	}
	r.set("deleteTag", tagDeleteVerdict(s))
	return nil
}

// measureTagDelete deletes the tag of M3, pushed by that tag only, and when M3 went with it, the
// tag of M4, pushed by digest first.
func (r *run) measureTagDelete(ctx context.Context, repo string, s *deleteSeen) error {
	m3 := r.newManifest("delete by tag", "", nil)
	if err := r.mustPush(ctx, repo, "u1", m3); err != nil {
		return err
	}
	var err error
	if s.byTag, err = r.send(ctx, http.MethodDelete, r.path(repo, "manifests/u1")); err != nil {
		return err
	}
	if !s.byTag.ok(http.StatusAccepted, http.StatusOK) {
		return nil
	}
	if s.tagged, err = r.getManifest(ctx, repo, m3.digest.String()); err != nil || s.tagged.status == http.StatusOK {
		return err
	}
	m4 := r.newManifest("pushed by digest, then tagged", "", nil)
	if err := r.mustPush(ctx, repo, m4.digest.String(), m4); err != nil {
		return err
	}
	if err := r.mustPush(ctx, repo, "u2", m4); err != nil {
		return err
	}
	if _, err := r.send(ctx, http.MethodDelete, r.path(repo, "manifests/u2")); err != nil {
		return err
	}
	s.byDigTag, err = r.getManifest(ctx, repo, m4.digest.String())
	return err
}

// afterDigestDelete looks at what stayed after the digest delete of M1, then deletes a subject
// with a referrer, and a manifest that an image index lists.
func (r *run) afterDigestDelete(ctx context.Context, repo string, s *deleteSeen) error {
	var err error
	if s.tagAfter, err = r.getManifest(ctx, repo, "t1"); err != nil {
		return err
	}
	if s.blobAfter, err = r.c.do(ctx, request{method: http.MethodHead, target: r.path(repo, "blobs/"+emptyDesc.Digest.String())}); err != nil {
		return err
	}
	subject := r.newManifest("a subject deleted", "", nil)
	if err := r.mustPush(ctx, repo, "s1", subject); err != nil {
		return err
	}
	sd := subject.desc()
	ref := r.newManifest("referrer of the deleted", "", &sd)
	if err := r.mustPush(ctx, repo, ref.digest.String(), ref); err != nil {
		return err
	}
	if s.subject, err = r.send(ctx, http.MethodDelete, r.path(repo, "manifests/"+sd.Digest.String())); err != nil {
		return err
	}
	if s.subject.ok(http.StatusAccepted, http.StatusOK) {
		if s.refAfter, err = r.getManifest(ctx, repo, ref.digest.String()); err != nil {
			return err
		}
	}
	m2 := r.newManifest("listed by an index", "", nil)
	if err := r.mustPush(ctx, repo, "x", m2); err != nil {
		return err
	}
	idx := encode(ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{m2.desc()},
	})
	if err := r.mustPush(ctx, repo, "idx", idx); err != nil {
		return err
	}
	s.indexed, err = r.send(ctx, http.MethodDelete, r.path(repo, "manifests/"+m2.digest.String()))
	return err
}

// digestDeleteVerdict is the cell "Delete a manifest by digest".
func digestDeleteVerdict(s deleteSeen) string {
	if !s.byDigest.ok(http.StatusAccepted, http.StatusOK) {
		return "no: " + s.byDigest.refusal()
	}
	v := fmt.Sprintf("yes, %d", s.byDigest.status)
	if s.tagAfter.status == http.StatusOK {
		v += ", **but its tags stay and serve it**"
	} else {
		v += ", with its tags"
	}
	if s.blobAfter.status == http.StatusNotFound {
		v += " **and the blobs only it used**"
	}
	switch {
	case !s.subject.ok(http.StatusAccepted, http.StatusOK):
		v += fmt.Sprintf("; **a manifest with a referrer: %s**", s.subject.refusal())
	case s.refAfter.status == http.StatusOK:
		v += "; its referrers stay"
	default:
		v += "; its referrers go with it"
	}
	if !s.indexed.ok(http.StatusAccepted, http.StatusOK) {
		v += fmt.Sprintf("; **%s** for a manifest an index lists", s.indexed.refusal())
	}
	return v
}

// tagDeleteVerdict is the cell "Delete by tag".
func tagDeleteVerdict(s deleteSeen) string {
	switch {
	case !s.byTag.ok(http.StatusAccepted, http.StatusOK):
		return "no: " + s.byTag.refusal()
	case s.tagged.status == http.StatusOK:
		return fmt.Sprintf("yes, %d: the manifest stays, untagged", s.byTag.status)
	case s.byDigTag.status == http.StatusOK:
		return "the tag; **the manifest too**, when it was pushed by that tag only"
	}
	return "the tag; **the manifest too**"
}

// measureImmutable measures "Immutable tags, where the rule lives": A under v1, then A under
// v1 again, then B under v1, and whether A stays after its tag moved.
func (r *run) measureImmutable(ctx context.Context) error {
	const repo = "imm"
	a := r.newManifest("immutable a", "", nil)
	b := r.newManifest("immutable b", "", nil)
	if err := r.mustPush(ctx, repo, "v1", a); err != nil {
		return err
	}
	again, err := r.pushManifest(ctx, repo, "v1", a)
	if err != nil {
		return err
	}
	repoint, err := r.pushManifest(ctx, repo, "v1", b)
	if err != nil {
		return err
	}
	var kept answer
	if repoint.ok(http.StatusCreated, http.StatusOK) {
		if kept, err = r.getManifest(ctx, repo, a.digest.String()); err != nil {
			return err
		}
	}
	r.found.repointRefused = !repoint.ok(http.StatusCreated, http.StatusOK)
	r.set("immutableTags", immutableVerdict(again, repoint, kept))
	return nil
}

// immutableVerdict is the cell "Immutable tags, where the rule lives": what pushing the same
// manifest again, and another one, under a tag answered, and whether the first manifest stayed.
func immutableVerdict(again, repoint, kept answer) string {
	if repoint.ok(http.StatusCreated, http.StatusOK) {
		v := fmt.Sprintf("none: a tag re-points with %d", repoint.status)
		if kept.status != http.StatusOK {
			v += ", and **the manifest it named is deleted**"
		}
		return v
	}
	v := "a tag cannot re-point: " + repoint.refusal()
	if again.ok(http.StatusCreated, http.StatusOK) {
		return v + "; the same manifest again is taken"
	}
	return v + "; the same manifest again too"
}
