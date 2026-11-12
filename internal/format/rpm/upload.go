package rpm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// defaultUploadName is the name of an upload whose client sent none: what the errors of Build
// call the file. The layer is titled with the file name of the header anyway.
const defaultUploadName = "upload.rpm"

// uploadResponse is the body of a successful upload.
type uploadResponse struct {
	Name     string `json:"name"`
	Epoch    int64  `json:"epoch"`
	Version  string `json:"version"`
	Release  string `json:"release"`
	Arch     string `json:"arch"`
	Filename string `json:"filename"`
	Digest   string `json:"digest"` // the manifest digest
	Tag      string `json:"tag"`
}

// upload publishes the request body, one .rpm (docs/spec/http-routing.md §3, rpm): PUT
// /rpm/<repo>/upload, which needs write. The body, bounded by the repo policy's maxFileSize
// (format.LimitBody, 413 above it), streams into the registry through deps.Ingest.UploadStream,
// which keeps it only when the publish goes through (ADR 0014): its head, up to the end of the
// header (uploadHead, at most MaxHeader bytes), is read and held first, for the NAME, which
// names the OCI repository, and for the file name rule (holders); then the head and the rest of
// the body pass the rpm inspector on their way. Errors name the file as Content-Disposition
// does, upload.rpm without one. Nothing of the body is written to a local disk.
//
// The answer is 201 with Location, the file below Packages/, Docker-Content-Digest, the
// manifest digest, and a JSON body with the name, epoch, version, release, arch, file name,
// manifest digest and tag. The same bytes again are the idempotent repeat of ingest, 201 with
// the digest of the manifest stored first; other bytes under the same name, version and arch
// are the 409 of ingest, whose tag holds another manifest; under the file name of a version
// with another epoch they are the 409 of the file name rule.
func (h *handlers) upload(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.start(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("rpm: no publisher in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	name := uploadName(r.Header)
	pkg, head, err := uploadHead(body)
	if err != nil {
		h.fail(w, r, fmt.Errorf("rpm: %q: %w", name, err))
		return
	}
	held, err := h.holders(ctx, repo, pkg)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stream := io.MultiReader(bytes.NewReader(head), body)
	check := &nameCheck{r: stream, sum: sha256.New(), filename: pkg.Filename, held: held}
	if len(held) > 0 {
		stream = check
	}
	principal := auth.PrincipalFrom(ctx)
	pub, err := h.deps.Ingest.UploadStream(ctx, repo, format.StreamUpload{
		Name:   pkg.Name,
		File:   format.UploadedFile{Name: name, Size: r.ContentLength, ContentType: r.Header.Get("Content-Type")},
		Body:   stream,
		Params: map[string]string{ParamUploadedBy: principal.String()},
	}, principal)
	if check.err != nil && errors.Is(err, check.err) {
		err = check.err // the rule's 409 as it is, not as ingest's error of the body
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := uploadResponse{Name: pkg.Name, Epoch: pkg.Epoch, Version: pkg.Version, Release: pkg.Release, Arch: pkg.Arch,
		Filename: pkg.Filename, Digest: pub.Digest.String(), Tag: pub.Tag}
	hdr := w.Header()
	hdr.Set("Location", "/"+Name+"/"+repo.Name+"/Packages/"+pkg.Filename)
	hdr.Set("Docker-Content-Digest", pub.Digest.String())
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// uploadHead reads the head of an rpm from body with ParseHead, which reads nothing after the
// header, and returns the package and the bytes read: the start of the body, to stream again
// before the rest. Errors as ParseHead.
func uploadHead(body io.Reader) (*Package, []byte, error) {
	var head bytes.Buffer
	pkg, err := ParseHead(io.TeeReader(io.LimitReader(body, MaxHeader), &head), MaxHeader)
	return pkg, head.Bytes(), err
}

// holders applies the file name rule to an upload of pkg: it returns the files of the repo's
// versions that hold pkg's file name, whose content the upload must have (nameCheck). The file
// name has no epoch, and the repo serves one content per name, so a version that holds the name
// with another blob, one of another epoch, refuses the upload, unless it is the version this
// upload makes (the same version and arch): that tag is ingest's to settle, the repeat or its
// own 409. The rule runs here and not in Build, whose signature has no index; an oras push has
// no such check, and two uploads at once both pass it: the Packages route answers 500 for a
// name with two contents.
func (h *handlers) holders(ctx context.Context, repo index.Repo, pkg *Package) ([]index.FileMatch, error) {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, pkg.Filename)
	if err != nil {
		return nil, fmt.Errorf("rpm: the files called %s in rpm/%s: %w", pkg.Filename, repo.Name, err)
	}
	var held []index.FileMatch
	for _, m := range matches {
		if m.Version.Version != pkg.EVR() || m.Version.Variant != pkg.Arch {
			held = append(held, m)
		}
	}
	return held, nil
}

// nameCheck is the body of an upload whose file name other versions hold (holders): it passes
// the bytes through and hashes them, and at their end, before ingest commits anything, fails
// instead of io.EOF when a file of held has another digest, with err: index.ErrConflict naming
// its version (409), which ingest returns as the body's error, after it cancelled the upload
// session.
type nameCheck struct {
	r        io.Reader
	sum      hash.Hash
	filename string
	held     []index.FileMatch
	err      error // the refusal, once the body ended
}

func (c *nameCheck) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sum.Write(p[:n])
	if err != io.EOF {
		return n, err
	}
	d := digest.NewDigest(digest.SHA256, c.sum)
	for _, m := range c.held {
		if m.File.Digest != d {
			c.err = fmt.Errorf("%w: the file name %s is held by version %s of %s with another content; "+
				"a repo serves one content per file name, which has no epoch", index.ErrConflict, c.filename, m.Version.Version, m.Package.Name)
			return n, c.err
		}
	}
	return n, io.EOF
}

// uploadName returns the file name of an upload: the filename parameter of Content-Disposition
// without directories, or defaultUploadName when the client sent none.
func uploadName(h http.Header) string {
	_, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		return defaultUploadName
	}
	switch name := path.Base(strings.ReplaceAll(params["filename"], `\`, "/")); name {
	case ".", "/", "..":
		return defaultUploadName
	default:
		return name
	}
}
