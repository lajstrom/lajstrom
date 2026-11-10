package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// referrerTypes are the artifact types of the probe's two referrers.
var referrerTypes = []string{"application/vnd.lajstrom.probe.signature.v1", "application/vnd.lajstrom.probe.sbom.v1"}

// referrersSeen is what the referrers measurement found.
type referrersSeen struct {
	api      answer // GET referrers/<subject>
	listed   bool   // the index lists every referrer pushed
	filtered *bool  // with ?artifactType= only that type is listed; nil when not asked
	written  bool   // the registry wrote the tag schema's sha256-<subject> itself
	subject  string // OCI-Subject of the first referrer's push
	want     digest.Digest
}

// measureReferrers measures "Referrers API" and "`OCI-Subject` on a push with `subject`": a
// subject tagged v1 and two referrers of it pushed by digest, each with an artifact type of
// its own; then the referrers of the subject, with and without the artifactType filter, and
// whether a sha256-<subject> tag appeared. A registry that refuses an unknown artifact type
// (GitLab with its metadata database) gets one referrer of the empty type instead.
func (r *run) measureReferrers(ctx context.Context) error {
	const repo = "ref"
	subject := r.newManifest("subject", "", nil)
	if err := r.mustPush(ctx, repo, "v1", subject); err != nil {
		return err
	}
	sd := subject.desc()
	seen := referrersSeen{want: sd.Digest}
	refs, err := r.pushReferrers(ctx, repo, sd, &seen)
	if err != nil {
		return err
	}
	if seen.api, err = r.get(ctx, r.path(repo, "referrers/"+sd.Digest.String()), nil); err != nil {
		return err
	}
	listed, isIndex := indexDigests(seen.api)
	seen.listed = isIndex && containsAll(listed, refs)
	if isIndex && len(refs) == len(referrerTypes) {
		a, err := r.get(ctx, r.path(repo, "referrers/"+sd.Digest.String()+"?artifactType="+url.QueryEscape(referrerTypes[0])), nil)
		if err != nil {
			return err
		}
		only, _ := indexDigests(a)
		f := slices.Equal(only, []digest.Digest{refs[0].digest})
		seen.filtered = &f
	}
	tags, _, err := r.listTags(ctx, r.path(repo, "tags/list"))
	if err != nil {
		return err
	}
	seen.written = slices.Contains(tags, strings.Replace(sd.Digest.String(), ":", "-", 1))
	r.found.referrersAPI, r.found.referrersKnown = isIndex, true
	r.set("referrers", referrersVerdict(seen))
	r.set("ociSubject", subjectVerdict(seen))
	return nil
}

// pushReferrers pushes the referrers of the subject sd by digest, one per referrerTypes, and
// keeps the OCI-Subject of the first push in seen. A registry that refuses the first artifact
// type gets one referrer of the empty type instead.
func (r *run) pushReferrers(ctx context.Context, repo string, sd ocispec.Descriptor, seen *referrersSeen) ([]manifest, error) {
	var refs []manifest
	for i, t := range referrerTypes {
		m := r.newManifest(fmt.Sprintf("referrer %d", i), t, &sd)
		a, err := r.pushManifest(ctx, repo, m.digest.String(), m)
		if err == nil && !a.ok(http.StatusCreated, http.StatusOK) && i == 0 {
			m = r.newManifest("referrer", ocispec.MediaTypeEmptyJSON, &sd)
			a, err = r.pushManifest(ctx, repo, m.digest.String(), m)
		}
		switch {
		case err != nil:
			return nil, err
		case !a.ok(http.StatusCreated, http.StatusOK) && i == 0:
			return nil, fmt.Errorf("push of a referrer: %s", a.refusal())
		case !a.ok(http.StatusCreated, http.StatusOK):
			return refs, nil
		case i == 0:
			seen.subject = a.header.Get("OCI-Subject")
		}
		refs = append(refs, m)
	}
	return refs, nil
}

// indexDigests returns the digests an image index answer lists; isIndex is false when the
// answer is no 200 with an index.
func indexDigests(a answer) (ds []digest.Digest, isIndex bool) {
	var idx struct {
		Manifests *[]ocispec.Descriptor `json:"manifests"`
	}
	if a.status != http.StatusOK || json.Unmarshal(a.body, &idx) != nil || idx.Manifests == nil {
		return nil, false
	}
	for _, d := range *idx.Manifests {
		ds = append(ds, d.Digest)
	}
	return ds, true
}

// containsAll reports whether every manifest of ms is in ds.
func containsAll(ds []digest.Digest, ms []manifest) bool {
	for _, m := range ms {
		if !slices.Contains(ds, m.digest) {
			return false
		}
	}
	return true
}

// referrersVerdict is the cell "Referrers API".
func referrersVerdict(s referrersSeen) string {
	if _, isIndex := indexDigests(s.api); !isIndex {
		return "no: " + s.api.refusal() + ", the tag schema"
	}
	if !s.listed {
		return "**yes, but the referrers pushed are not listed**"
	}
	v := "yes"
	switch {
	case s.filtered == nil:
	case *s.filtered:
		v += ", with the `artifactType` filter"
	default:
		v += ", **without** the `artifactType` filter"
	}
	if s.written {
		return v + "; and it writes the tag schema itself"
	}
	return v + "; no tag schema"
}

// subjectVerdict is the cell "`OCI-Subject` on a push with `subject`".
func subjectVerdict(s referrersSeen) string {
	switch s.subject {
	case "":
		return "no"
	case s.want.String():
		return "yes"
	}
	return "**another digest**: " + s.subject
}
