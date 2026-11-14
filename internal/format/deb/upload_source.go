package deb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// abortTimeout bounds the abort of a source upload's session after a failure, which runs after
// a client that went away too.
const abortTimeout = 10 * time.Second

// uploadSource publishes one source package to the suite and component of the query
// (docs/spec/http-routing.md §3, apt; #79): POST /apt/<repo>/upload?suite=&component=, which
// needs write, with a multipart/form-data body of one part per file, the .dsc first. The .dsc is
// read whole (at most MaxDsc) and parsed; the pool file name rule is applied to every file it
// names, and to it, from the checksums it gives them, before anything is stored; then a
// publication session of the source and version (deps.Sessions) takes the .dsc and each further
// part as it streams into the registry (ADR 0014), each part bounded by the repo's maxFileSize,
// and each checked against the size and checksums the .dsc gives it once it ended. A file the
// .dsc names and the body lacks must be in the pool already, held by a version of the same
// source with that sha256. Commit publishes the session with the suite and the component; any
// failure before aborts it, so nothing is published but the whole package.
//
// The answer is that of the .deb upload: 201 with Location (the .dsc in the pool),
// Docker-Content-Digest and the JSON body, package the source, architecture source, filename the
// .dsc. The same files again are the idempotent repeat of ingest; other files under the same
// source, version, suite and component its 409.
func (h *handlers) uploadSource(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.startAs(w, r, auth.ActionWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	params := map[string]string{ParamSuite: q.Get(ParamSuite), ParamComponent: q.Get(ParamComponent)}
	suite, component, err := distParams(params)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Sessions == nil {
		h.fail(w, r, errors.New("deb: no publication sessions in format.Deps"))
		return
	}
	limit, err := format.MaxFileSize(repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	parts, err := multipartOf(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dsc, raw, err := readDscPart(parts, repo, limit)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.sourcePoolRule(ctx, repo, dsc, raw, suite, component); err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	pub, err := h.publishSource(ctx, repo, principal, parts, dsc, raw, limit, map[string]string{ParamSuite: suite, ParamComponent: component})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	resp := uploadResponse{Package: pub.Entry.Name, Version: pub.Entry.Version, Architecture: ArchSource,
		Suite: suite, Component: component, Filename: dsc.Filename, Digest: pub.Digest.String(), Tag: pub.Tag}
	hdr := w.Header()
	hdr.Set("Location", "/"+Name+"/"+repo.Name+"/pool/"+component+"/"+PoolPath(dsc.Source, dsc.Filename))
	hdr.Set("Docker-Content-Digest", pub.Digest.String())
	hdr.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// multipartOf returns the reader of the parts of a multipart/form-data body; another body is
// format.ErrInvalid.
func multipartOf(r *http.Request) (*multipart.Reader, error) {
	mt, ps, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || ps["boundary"] == "" {
		return nil, fmt.Errorf("%w: a source package is uploaded as multipart/form-data, a part per file, the .dsc first; "+
			"a .deb is PUT as the body", format.ErrInvalid)
	}
	return multipart.NewReader(r.Body, ps["boundary"]), nil
}

// nextFile returns the next part of the body and the name of its file, without directories;
// io.EOF after the last. A part without a file name is format.ErrInvalid.
func nextFile(parts *multipart.Reader) (*multipart.Part, string, error) {
	p, err := parts.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, "", io.EOF
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: the multipart body: %w", format.ErrInvalid, err)
	}
	name := path.Base(strings.ReplaceAll(p.FileName(), `\`, "/"))
	if p.FileName() == "" || name == "." || name == "/" || name == ".." {
		return nil, "", fmt.Errorf("%w: the part %q of the body is not a file; a source upload has a part per file, each with a filename",
			format.ErrInvalid, p.FormName())
	}
	return p, name, nil
}

// readDscPart reads the first part of the body, which must be a .dsc, called as its Source and
// Version make it, of at most MaxDsc bytes and the repo's maxFileSize, limit, and parses it.
func readDscPart(parts *multipart.Reader, repo index.Repo, limit int64) (*Dsc, []byte, error) {
	p, name, err := nextFile(parts)
	if errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: the body has no file; the first is the .dsc", format.ErrInvalid)
	}
	if err != nil {
		return nil, nil, err
	}
	if !strings.HasSuffix(name, ".dsc") {
		return nil, nil, fmt.Errorf("%w: the first file of a source upload is the .dsc, not %s", format.ErrInvalid, name)
	}
	raw, err := io.ReadAll(io.LimitReader(&partLimit{r: p, n: limit, limit: limit, name: name, repo: repo}, MaxDsc+1))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, nil, err
	case err != nil:
		return nil, nil, fmt.Errorf("%w: %s: %w", format.ErrInvalid, name, err)
	}
	dsc, err := ParseDsc(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	if name != dsc.Filename {
		return nil, nil, fmt.Errorf("%w: the .dsc is called %s; its Source and Version make it %s", format.ErrInvalid, name, dsc.Filename)
	}
	return dsc, raw, nil
}

// sourcePoolRule applies the pool file name rule of poolHolders to the files of a source upload,
// the .dsc and every file it names, from the sha256 the .dsc gives each: a version of the repo
// that holds one of the names with another blob refuses the upload (index.ErrConflict, 409),
// unless it is the version this upload makes, whose tag is ingest's to settle.
func (h *handlers) sourcePoolRule(ctx context.Context, repo index.Repo, dsc *Dsc, raw []byte, suite, component string) error {
	variant := variantOf(suite, component, ArchSource)
	want := map[string]digest.Digest{dsc.Filename: digest.FromBytes(raw)}
	for _, f := range dsc.Files {
		want[f.Name] = digest.NewDigestFromEncoded(digest.SHA256, f.SHA256)
	}
	for name, d := range want {
		matches, err := h.deps.Index.FindFiles(ctx, repo.ID, name)
		if err != nil {
			return fmt.Errorf("deb: the files called %s in apt/%s: %w", name, repo.Name, err)
		}
		for _, m := range matches {
			if m.File.Digest != d && (m.Version.Version != dsc.Version || m.Version.Variant != variant) {
				return fmt.Errorf("%w: pool file name %s is held by version %s in %s/%s with another content; "+
					"a Debian pool holds one content per file name", index.ErrConflict, name, m.Version.Version,
					m.Version.Attrs[profile.AnnotationDebSuite], m.Version.Attrs[profile.AnnotationDebComponent])
			}
		}
	}
	return nil
}

// publishSource streams the .dsc and the further parts of the body into a publication session
// of the source and version, checks each, and commits the session with params; a failure aborts
// it. A session the principal left open (an upload that failed and was not aborted) is aborted
// and opened afresh, so that no file of it is published.
func (h *handlers) publishSource(ctx context.Context, repo index.Repo, principal auth.Principal, parts *multipart.Reader,
	dsc *Dsc, raw []byte, limit int64, params map[string]string) (*format.Published, error) {
	s, err := h.openSource(ctx, repo, principal, dsc)
	if err != nil {
		return nil, err
	}
	pub, err := h.fillSource(ctx, repo, s.ID, parts, dsc, raw, limit, params)
	if err != nil {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
		defer cancel()
		if aerr := h.deps.Sessions.Abort(actx, s.ID); aerr != nil {
			h.deps.Log.WarnContext(ctx, "apt: the session of a failed source upload was not aborted; it expires",
				"repo", repo.Format+"/"+repo.Name, "session", s.ID, "error", aerr.Error())
		}
		return nil, err
	}
	return pub, nil
}

// openSource opens the session of the source and version of dsc for principal, empty.
func (h *handlers) openSource(ctx context.Context, repo index.Repo, principal auth.Principal, dsc *Dsc) (*index.Session, error) {
	for range 2 {
		s, err := h.deps.Sessions.Open(ctx, repo, dsc.Source, dsc.Version, principal)
		if err != nil {
			return nil, err
		}
		files, err := h.deps.Sessions.Files(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return s, nil
		}
		if err := h.deps.Sessions.Abort(ctx, s.ID); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: the session of %s %s is being filled by another upload of the same principal", index.ErrConflict, dsc.Source, dsc.Version)
}

// fillSource adds the .dsc and each further part of the body to the session id, checks every
// part against the .dsc as it ended, checks that the pool holds every file the .dsc names and
// the body lacks, and commits the session with params.
func (h *handlers) fillSource(ctx context.Context, repo index.Repo, id int64, parts *multipart.Reader, dsc *Dsc, raw []byte,
	limit int64, params map[string]string) (*format.Published, error) {
	if _, err := h.deps.Sessions.Add(ctx, id, dsc.Filename, bytes.NewReader(raw), int64(len(raw))); err != nil {
		return nil, err
	}
	named := make(map[string]SourceFile, len(dsc.Files))
	for _, f := range dsc.Files {
		named[f.Name] = f
	}
	for {
		p, name, err := nextFile(parts)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		want, ok := named[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s is not a file %s names, or comes twice", format.ErrInvalid, name, dsc.Filename)
		}
		delete(named, name)
		sf, err := h.deps.Sessions.Add(ctx, id, name, &partLimit{r: p, n: limit, limit: limit, name: name, repo: repo}, -1)
		if err != nil {
			return nil, err
		}
		if _, err := checkSourceFile(format.UploadedFile{Name: name, Size: sf.Size, Checksums: sf.Checksums}, want); err != nil {
			return nil, err
		}
	}
	for _, f := range dsc.Files {
		if _, missing := named[f.Name]; !missing {
			continue
		}
		if err := h.inPool(ctx, repo, dsc, f); err != nil {
			return nil, err
		}
	}
	return h.deps.Sessions.Commit(ctx, id, params)
}

// inPool checks that a version of the source of dsc holds the file f with the sha256 the .dsc
// gives it: its blob is then in the source's OCI repository, where the manifest refers to it.
func (h *handlers) inPool(ctx context.Context, repo index.Repo, dsc *Dsc, f SourceFile) error {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, f.Name)
	if err != nil {
		return fmt.Errorf("deb: the files called %s in apt/%s: %w", f.Name, repo.Name, err)
	}
	d := digest.NewDigestFromEncoded(digest.SHA256, f.SHA256)
	for _, m := range matches {
		if m.Package.Name == dsc.Source && m.File.Digest == d {
			return nil
		}
	}
	return fmt.Errorf("%w: %s names %s, which the body lacks and no version of %s in the pool holds; upload it",
		format.ErrInvalid, dsc.Filename, f.Name, dsc.Source)
}

// partLimit passes at most n bytes of a part through and fails after them with an error that
// wraps an *http.MaxBytesError (413), naming the file and the repo's maxFileSize; a negative n
// is no bound.
type partLimit struct {
	r     io.Reader
	n     int64 // bytes left
	limit int64
	name  string
	repo  index.Repo
}

func (l *partLimit) Read(p []byte) (int, error) {
	if l.n < 0 {
		return l.r.Read(p)
	}
	if l.n == 0 {
		var one [1]byte
		if k, err := l.r.Read(one[:]); k == 0 {
			return 0, err
		}
		return 0, fmt.Errorf("%s is larger than the maxFileSize of %s/%s, %d bytes: %w", l.name, l.repo.Format, l.repo.Name, l.limit,
			&http.MaxBytesError{Limit: l.limit})
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	k, err := l.r.Read(p)
	l.n -= int64(k)
	return k, err
}
