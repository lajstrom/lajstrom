package npm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
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
// PUT /npm/<repo>/<name> with npm's body, read by readBody as it streams, and the version
// published through deps.Ingest as one manifest of the profile, built from the tarball and the
// version document. The tarball streams into the registry as its data is decoded (ADR 0014,
// streamTarball), and the upload ends only once the rest of the body is read and checked by
// checkPublish: a body refused stores nothing, whatever the order of its members (yarn sends
// _attachments before versions). 201 {"ok": true}, also for the same tarball again; 409 for
// another tarball under a published version; 400 naming the rule; 413 over the repo's
// maxFileSize or the bound of the rest of the body. It needs write on the repo. The dist-tags of
// the body, latest when it has none, are journal adds. A body without _attachments, or any body
// at PUT /<name>/-rev/<rev> (rev), whose data is dropped, is the packument update of npm
// deprecate and unpublish (update).
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
	principal := auth.PrincipalFrom(ctx)
	var tarball *streamedTarball
	defer func() {
		if tarball != nil {
			tarball.abort()
		}
	}()
	body, err := readBody(r.Body, limit, repo.Format+"/"+repo.Name, func(file string, data io.Reader) error {
		switch {
		case rev != "":
			return nil
		case tarball != nil: // readBody refuses a second attachment before its data
			return fmt.Errorf("%w: the publish body has a second tarball", format.ErrInvalid)
		}
		tarball = h.streamTarball(ctx, repo, name, file, principal)
		return tarball.send(data)
	})
	if tarball == nil { // no data: an update, or a publish that checkPublish refuses
		if err == nil && (body.attachments == nil || rev != "") {
			h.update(w, r, repo, name, rev, body.fields)
			return
		}
		if err == nil {
			_, err = checkPublish(name, body)
		}
		h.fail(w, r, err)
		return
	}
	var pub *publication
	if err == nil {
		pub, err = checkPublish(name, body)
	}
	if err == nil {
		tarball.params[ParamDocument], tarball.params[ParamReadme] = string(pub.document), pub.readme
	}
	res, err := tarball.end(err)
	h.published(w, r, repo, name, pub, res, err)
}

// published answers a publish whose upload returned res and err, and appends its dist-tags to the
// journal.
func (h *handlers) published(w http.ResponseWriter, r *http.Request, repo index.Repo, name string, pub *publication,
	res *format.Published, err error) {
	ctx := r.Context()
	var conflict *index.TagConflictError
	switch {
	case pub != nil && (errors.As(err, &conflict) || errors.Is(err, index.ErrConflict)):
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

// streamedTarball is the tarball of a publish on its way into the registry: UploadStream runs on
// a goroutine of its own and reads the tarball from a pipe, which send fills with the data of
// the attachment as readBody decodes it. The pipe ends with end, once the rest of the body is read
// and checked, so that the upload is refused, and stores nothing, when the body is; until then
// end may add the version document of the body to params, which Build reads after the end. A
// panic of the upload, which no handler's recovery would see on its goroutine, is raised again
// by end on the handler's.
type streamedTarball struct {
	pw       *io.PipeWriter
	params   map[string]string
	done     chan struct{}
	ended    bool // end was called
	res      *format.Published
	err      error
	panicked error // what the upload panicked with, with its stack, once done is closed
}

// errUploadEnded is the error of a write of the tarball after its upload returned, and
// errPublishEnded that of the upload's read when publish ended without ending it (a panic).
var (
	errUploadEnded  = errors.New("npm: the upload of the tarball ended")
	errPublishEnded = errors.New("npm: the publish ended before its tarball")
)

// streamTarball starts the upload of the tarball file of the package name.
func (h *handlers) streamTarball(ctx context.Context, repo index.Repo, name, file string, principal auth.Principal) *streamedTarball {
	pr, pw := io.Pipe()
	t := &streamedTarball{pw: pw, params: map[string]string{ParamUploadedBy: principal.String()}, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		defer func() {
			if v := recover(); v != nil {
				t.panicked = fmt.Errorf("npm: the upload of the tarball panicked: %v\n%s", v, debug.Stack())
			}
			_ = pr.CloseWithError(errUploadEnded) // a PipeReader's CloseWithError is always nil
		}()
		t.res, t.err = h.deps.Ingest.UploadStream(ctx, repo, format.StreamUpload{Name: name,
			File: format.UploadedFile{Name: file, Size: -1, ContentType: "application/octet-stream"}, Body: pr, Params: t.params}, principal)
	}()
	return t
}

// send copies the decoded data of the attachment into the upload.
func (t *streamedTarball) send(data io.Reader) error {
	_, err := io.Copy(t.pw, data)
	return err
}

// end ends the tarball, with err when the body failed or was refused, waits for the upload and
// returns what it returned; but err itself when the body failed or was refused on its own, not
// because the upload ended first (errUploadEnded), so that the client reads the reason as
// readBody or checkPublish gave it.
func (t *streamedTarball) end(err error) (*format.Published, error) {
	t.ended = true
	if err != nil {
		_ = t.pw.CloseWithError(err) // a PipeWriter's CloseWithError is always nil
	} else {
		_ = t.pw.Close()
	}
	<-t.done
	if p := t.panicked; p != nil {
		t.panicked = nil
		panic(p)
	}
	if err != nil && (t.err == nil || !errors.Is(err, errUploadEnded)) {
		return nil, err
	}
	return t.res, t.err
}

// abort ends the upload when end was not called, after a panic of publish: the goroutine of the
// upload would wait on the pipe for ever otherwise, its session open.
func (t *streamedTarball) abort() {
	if !t.ended {
		_, _ = t.end(errPublishEnded)
	}
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
	if !a.data {
		return fmt.Errorf("%w: the attachment %q has no data", format.ErrInvalid, a.name)
	}
	size := a.size
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
