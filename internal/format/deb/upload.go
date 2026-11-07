package deb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// it), is spooled for the request under the file name of Content-Disposition, upload.deb
// without one; checkPoolName applies the pool file name rule; then deps.Ingest publishes it
// with the deb plugin's Build, which reads the control file and the checksums in one pass.
//
// The answer is 201 with Location, the file in the pool (pool/<component>/<PoolPath>),
// Docker-Content-Digest, the manifest digest, and a JSON body with the package, version,
// architecture, suite, component, pool file name, manifest digest and tag. The same bytes again
// are the idempotent repeat of ingest, 201 with the digest of the manifest stored first; other
// bytes under the same name, version and architecture in the same suite and component are the
// 409 of ingest, whose tag holds another manifest; in another suite they are the 409 of the
// pool file name rule. The spool is removed when the handler returns.
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
	sp, err := format.Spool(body, uploadName(r.Header), r.Header.Get("Content-Type"), r.ContentLength)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() {
		if err := sp.Close(); err != nil {
			h.deps.Log.WarnContext(ctx, "apt: upload spool not removed", "error", err.Error())
		}
	}()
	file := sp.File()
	pkg, err := h.checkPoolName(ctx, repo, file, suite, component)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	params[ParamUploadedBy] = principal.String()
	pub, err := h.deps.Ingest.Upload(ctx, repo, format.Upload{Files: []format.UploadedFile{file}, Params: params}, principal)
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

// checkPoolName applies the pool file name rule to the upload f for suite/component and returns
// what the control file of f says. A Debian pool holds one content per file name, and an apt
// repo has one pool: the same .deb in two suites is two versions over one blob, which the pool
// route serves as one file, but another .deb with the same name, version (without the epoch)
// and architecture has the same pool file name, and apt would fail the hash check of one
// suite. So a version of the repo that holds the file name with another blob is
// index.ErrConflict naming it (409), unless it is the version this upload makes (the same
// version and variant): that tag is ingest's to settle, the repeat or its own 409.
//
// The control member is read with ParseControl from a fresh reader of the spool, the first KiB
// of the file, for the file name; a body that is not a .deb is format.ErrInvalid here (400).
// The whole file is hashed only when another version holds the name. The rule runs here and
// not in Build, whose signature has no index; the oras push path has no such check, since
// Validate has none either, and two uploads at once both pass it: the pool route answers 500
// for a name with two contents, and reindex (#29) reports it.
func (h *handlers) checkPoolName(ctx context.Context, repo index.Repo, f format.UploadedFile, suite, component string) (*Package, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("deb: open %q: %w", f.Name, err)
	}
	pkg, err := ParseControl(rc, MaxControl)
	_ = rc.Close()
	if err != nil {
		return nil, fmt.Errorf("deb: %q: %w", f.Name, err)
	}
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, pkg.Filename)
	if err != nil {
		return nil, fmt.Errorf("deb: the files called %s in apt/%s: %w", pkg.Filename, repo.Name, err)
	}
	variant := variantOf(suite, component, pkg.Architecture)
	var d digest.Digest
	for _, m := range matches {
		if m.Version.Version == pkg.Version && m.Version.Variant == variant {
			continue
		}
		if d == "" {
			if d, err = digestOf(f); err != nil {
				return nil, err
			}
		}
		if m.File.Digest != d {
			return nil, fmt.Errorf("%w: pool file name %s is held by version %s in %s/%s with another content; "+
				"a Debian pool holds one content per file name", index.ErrConflict, pkg.Filename, m.Version.Version,
				m.Version.Attrs[profile.AnnotationDebSuite], m.Version.Attrs[profile.AnnotationDebComponent])
		}
	}
	return pkg, nil
}

// digestOf returns the sha256 digest of f, the digest of its layer.
func digestOf(f format.UploadedFile) (digest.Digest, error) {
	rc, err := f.Open()
	if err != nil {
		return "", fmt.Errorf("deb: open %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()
	d, err := digest.SHA256.FromReader(rc)
	if err != nil {
		return "", fmt.Errorf("deb: read %q: %w", f.Name, err)
	}
	return d, nil
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
