package deb

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
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// defaultUploadName is the name of an upload whose client sent none: what the errors of Build
// call the file. The layer is titled with the pool file name of the control file anyway.
const defaultUploadName = "upload.deb"

// uploadResponse is the body of a successful upload.
type uploadResponse struct {
	Package      string `json:"package"`
	Version      string `json:"version"`
	Architecture string `json:"architecture"`
	Suite        string `json:"suite"`
	Component    string `json:"component"`
	Filename     string `json:"filename"`
	Digest       string `json:"digest"` // the manifest digest
	Tag          string `json:"tag"`
}

// upload publishes the request body, one .deb, to the suite and component of the query
// (docs/spec/http-routing.md §3, apt): PUT /apt/<repo>/upload?suite=&component=[&arch=], which
// needs write. suite and component are required and have the grammar Build checks (400 naming
// the one that is missing or malformed); arch, when given, must be the Architecture of the
// control file. The body, bounded by the repo policy's maxFileSize (format.LimitBody, 413 above
// it), streams into the registry through deps.Ingest.UploadStream, which keeps it only when the
// publish goes through (ADR 0014): its head, up to the end of the control member (readHead, at
// most maxHead bytes), is read and held first, for the package name, which names the OCI
// repository, and for the pool file name rule (poolHolders); then the head and the rest of the
// body pass the deb inspector on their way. Errors name the file as Content-Disposition does,
// upload.deb without one. Nothing of the body is written to a local disk.
//
// The answer is 201 with Location, the file in the pool (pool/<component>/<PoolPath>),
// Docker-Content-Digest, the manifest digest, and a JSON body with the package, version,
// architecture, suite, component, pool file name, manifest digest and tag. The same bytes again
// are the idempotent repeat of ingest, 201 with the digest of the manifest stored first; other
// bytes under the same name, version and architecture in the same suite and component are the
// 409 of ingest, whose tag holds another manifest; in another suite they are the 409 of the
// pool file name rule.
func (h *handlers) upload(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.startAs(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	params := map[string]string{ParamSuite: q.Get(ParamSuite), ParamComponent: q.Get(ParamComponent)}
	if arch := q.Get(ParamArch); arch != "" {
		params[ParamArch] = arch
	}
	suite, component, err := distParams(params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("deb: no publisher in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	name := uploadName(r.Header)
	pkg, head, err := readHead(body)
	if err != nil {
		h.fail(w, r, fmt.Errorf("deb: %q: %w", name, err))
		return
	}
	if err := checkArch(params, pkg); err != nil { // Build's check, before a byte streams
		h.fail(w, r, err)
		return
	}
	held, err := h.poolHolders(ctx, repo, pkg, suite, component)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	stream := io.MultiReader(bytes.NewReader(head), body)
	check := &poolCheck{r: stream, sum: sha256.New(), filename: pkg.Filename, held: held}
	if len(held) > 0 {
		stream = check
	}
	principal := auth.PrincipalFrom(ctx)
	params[ParamUploadedBy] = principal.String()
	pub, err := h.deps.Ingest.UploadStream(ctx, repo, format.StreamUpload{
		Name:   pkg.Name,
		File:   format.UploadedFile{Name: name, Size: r.ContentLength, ContentType: r.Header.Get("Content-Type")},
		Body:   stream,
		Params: params,
	}, principal)
	if check.err != nil && errors.Is(err, check.err) {
		err = check.err // the rule's 409 as it is, not as ingest's error of the body
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := uploadResponse{Package: pub.Entry.Name, Version: pub.Entry.Version, Architecture: pub.Entry.Attrs[profile.AnnotationDebArch],
		Suite: suite, Component: component, Filename: pkg.Filename, Digest: pub.Digest.String(), Tag: pub.Tag}
	hdr := w.Header()
	hdr.Set("Location", "/"+Name+"/"+repo.Name+"/pool/"+component+"/"+pkg.PoolPath)
	hdr.Set("Docker-Content-Digest", pub.Digest.String())
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// poolHolders applies the pool file name rule to an upload of pkg to suite/component: it
// returns the files of the repo's versions that hold pkg's pool file name, whose content the
// upload must have (poolCheck). A Debian pool holds one content per file name, and an apt repo
// has one pool: the same .deb in two suites is two versions over one blob, which the pool route
// serves as one file, but another .deb with the same name, version (without the epoch) and
// architecture has the same pool file name, and apt would fail the hash check of one suite. So
// a version of the repo that holds the file name with another blob refuses the upload, unless
// it is the version this upload makes (the same version and variant): that tag is ingest's to
// settle, the repeat or its own 409.
//
// The rule runs here and not in Build, whose signature has no index; the oras push path has no
// such check, since Validate has none either, and two uploads at once both pass it: the pool
// route answers 500 for a name with two contents, and reindex (#29) reports it.
func (h *handlers) poolHolders(ctx context.Context, repo index.Repo, pkg *Package, suite, component string) ([]index.FileMatch, error) {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, pkg.Filename)
	if err != nil {
		return nil, fmt.Errorf("deb: the files called %s in apt/%s: %w", pkg.Filename, repo.Name, err)
	}
	variant := variantOf(suite, component, pkg.Architecture)
	var held []index.FileMatch
	for _, m := range matches {
		if m.Version.Version != pkg.Version || m.Version.Variant != variant {
			held = append(held, m)
		}
	}
	return held, nil
}

// poolCheck is the body of an upload whose pool file name other versions hold (poolHolders): it
// passes the bytes through and hashes them, and at their end, before ingest commits anything,
// fails instead of io.EOF when a file of held has another digest, with err: index.ErrConflict
// naming its version (409), which ingest returns as the body's error, after it cancelled the
// upload session. The whole file is hashed only when another version holds the name.
type poolCheck struct {
	r        io.Reader
	sum      hash.Hash
	filename string
	held     []index.FileMatch
	err      error // the refusal, once the body ended
}

func (c *poolCheck) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.sum.Write(p[:n])
	if err != io.EOF {
		return n, err
	}
	d := digest.NewDigest(digest.SHA256, c.sum)
	for _, m := range c.held {
		if m.File.Digest != d {
			c.err = fmt.Errorf("%w: pool file name %s is held by version %s in %s/%s with another content; "+
				"a Debian pool holds one content per file name", index.ErrConflict, c.filename, m.Version.Version,
				m.Version.Attrs[profile.AnnotationDebSuite], m.Version.Attrs[profile.AnnotationDebComponent])
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
