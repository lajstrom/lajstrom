package npm

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// publication is a publish body that checkPublish took: the version, its document, the readme of
// the body and the dist-tags that name the version.
type publication struct {
	version  string
	document json.RawMessage
	readme   string
	tags     []string
	tarball  *attachment
}

// publish is npm publish, pnpm publish and yarn npm publish (docs/spec/http-routing.md, npm):
// PUT /npm/<repo>/<name> with npm's body, read by readBody as it streams, checked by
// checkPublish, and the version published through deps.Ingest as one manifest of the profile,
// built from the tarball and the version document. 201 {"ok": true}, also for the same tarball
// again; 409 for another tarball under a published version; 400 naming the rule; 413 over the
// repo's maxFileSize or the bound of the rest of the body. It needs write on the repo. The
// dist-tags of the body, latest when it has none, are journal adds. A body without _attachments,
// or any body at PUT /<name>/-rev/<rev> (rev), is the packument update of npm deprecate and
// unpublish (update).
func (h *handlers) publish(w http.ResponseWriter, r *http.Request, repo index.Repo, name, rev string) {
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionWrite, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("npm: no publisher in format.Deps"))
		return
	}
	limit, err := format.MaxFileSize(repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body, err := readBody(r.Body, limit, repo.Format+"/"+repo.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() {
		if err := body.Close(); err != nil {
			h.deps.Log.WarnContext(ctx, "npm: publish spool not removed", "error", err.Error())
		}
	}()
	if body.attachments == nil || rev != "" {
		h.update(w, r, repo, name, rev, body.fields)
		return
	}
	pub, err := checkPublish(name, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	res, err := h.deps.Ingest.Upload(ctx, repo, format.Upload{Files: []format.UploadedFile{pub.tarball.file.File()},
		Params: map[string]string{ParamUploadedBy: principal.String(), ParamDocument: string(pub.document), ParamReadme: pub.readme}}, principal)
	var conflict *index.TagConflictError
	switch {
	case errors.As(err, &conflict) || errors.Is(err, index.ErrConflict):
		h.fail(w, r, fmt.Errorf("%w: cannot publish over the published version %s@%s, which has another tarball", index.ErrConflict, name, pub.version))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	// The dist-tags go to the journal after the version is indexed, those that name it already
	// excepted, so that a repeat after a failed append completes it (docs/spec/http-routing.md,
	// npm, Publish).
	tags := pub.tags
	if len(tags) == 0 {
		tags = []string{profile.LatestTag}
	}
	if err := h.setTags(ctx, repo, name, pub.version, tags); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Log.InfoContext(ctx, "npm: published", "repo", repo.Format+"/"+repo.Name, "package", name, "version", pub.version,
		"digest", res.Digest.String(), "existed", res.Existed, "distTags", pub.tags)
	w.Header().Set("Docker-Content-Digest", res.Digest.String())
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// checkPublish checks a publish body for the package name of the URL (docs/spec/http-routing.md,
// npm): the body's name is the URL's, a name npm takes for a new package; one version, SemVer
// without build metadata, whose document has the name and the version; one attachment, of the
// length it says, with the dist.integrity and dist.shasum of the document; dist-tags well formed
// that name the version. What the tarball says is Build's to check. Errors wrap
// format.ErrInvalid, or an *http.MaxBytesError for a document and readme above the metadata
// layer's bound (413).
func checkPublish(name string, b *publishBody) (*publication, error) {
	invalid := func(f string, args ...any) error {
		return fmt.Errorf("%w: "+f, append([]any{format.ErrInvalid}, args...)...)
	}
	var bodyName string
	if json.Unmarshal(b.fields["name"], &bodyName) != nil || bodyName != name {
		return nil, invalid("the body is of the package %q, the URL of %q", bodyName, name)
	}
	if err := CheckNewName(name); err != nil {
		return nil, err
	}
	var versions map[string]json.RawMessage
	if json.Unmarshal(b.fields["versions"], &versions) != nil || len(versions) != 1 {
		return nil, invalid("versions must be an object of one version, it has %d", len(versions))
	}
	if len(b.attachments) != 1 {
		return nil, invalid("the body must have one attachment, it has %d", len(b.attachments))
	}
	p := &publication{tarball: b.attachments[0]}
	for v, doc := range versions {
		p.version, p.document = v, doc
	}
	if err := CheckVersion(p.version); err != nil {
		return nil, err
	}
	doc, err := ParsePackageJSON(p.document)
	if err != nil {
		return nil, fmt.Errorf("the version document: %w", err)
	}
	if doc.Name != name || doc.Version != p.version {
		return nil, invalid("the version document is of %s@%s, the body of %s@%s", doc.Name, doc.Version, name, p.version)
	}
	if err := p.tarball.check(doc); err != nil {
		return nil, err
	}
	if p.tags, err = tagsOf(b.fields["dist-tags"], p.version); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(b.fields["readme"], &p.readme)
	if n := len(p.document) + len(p.readme); n > maxMetadata-64<<10 {
		return nil, fmt.Errorf("the version document and the readme are %d bytes, more than the metadata of a version takes: %w", n,
			&http.MaxBytesError{Limit: maxMetadata})
	}
	return p, nil
}

// check compares the attachment with what the document says of it: its length, the
// dist.integrity (each sha512, sha256 or sha1 of it; other algorithms are ignored) and the
// dist.shasum, each when it is there.
func (a *attachment) check(doc *PackageJSON) error {
	size := a.file.File().Size
	var length int64
	if a.length != nil && (json.Unmarshal(a.length, &length) != nil || length != size) {
		return fmt.Errorf("%w: the attachment %q says a length of %s, its data has %d bytes", format.ErrInvalid, a.name, a.length, size)
	}
	var dist struct{ Integrity, Shasum string }
	_ = json.Unmarshal(doc.Fields["dist"], &dist)
	for _, entry := range strings.Fields(dist.Integrity) {
		alg, b64, _ := strings.Cut(entry, "-")
		b64, _, _ = strings.Cut(b64, "?")
		sum := map[string][]byte{"sha512": a.sha512, "sha256": a.sha256, "sha1": a.sha1}[alg]
		if got, err := base64.StdEncoding.DecodeString(b64); sum != nil && (err != nil || !bytes.Equal(got, sum)) {
			return fmt.Errorf("%w: dist.integrity %s is not the tarball's, sha512-%s", format.ErrInvalid, entry,
				base64.StdEncoding.EncodeToString(a.sha512))
		}
	}
	if dist.Shasum != "" && !strings.EqualFold(dist.Shasum, hex.EncodeToString(a.sha1)) {
		return fmt.Errorf("%w: dist.shasum %s is not the tarball's, %x", format.ErrInvalid, dist.Shasum, a.sha1)
	}
	return nil
}

// tagsOf returns the dist-tags of a body, which must all be well formed and name version; none
// for a body without dist-tags.
func tagsOf(raw json.RawMessage, version string) ([]string, error) {
	var tags map[string]string
	if raw != nil && json.Unmarshal(raw, &tags) != nil {
		return nil, fmt.Errorf("%w: dist-tags must be an object of tags and versions: %s", format.ErrInvalid, raw)
	}
	var out []string
	for tag, v := range tags {
		switch {
		case !profile.IsDistTag(tag):
			return nil, fmt.Errorf("%w: the dist-tag %q is not a letter and up to 127 letters, digits, '.', '_' or '-'", format.ErrInvalid, tag)
		case v != version:
			return nil, fmt.Errorf("%w: the dist-tag %s names %s, which this publish is not; it publishes %s", format.ErrInvalid, tag, v, version)
		}
		out = append(out, tag)
	}
	slices.Sort(out)
	return out, nil
}
