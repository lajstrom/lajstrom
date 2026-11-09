package pypi

import (
	"crypto/md5" //nolint:gosec // md5_digest of the legacy upload API, compared, not trusted
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"golang.org/x/crypto/blake2b"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// maxFields bounds the form fields of an upload besides the file: the core metadata twine
// repeats, a long description included.
const maxFields = 8 << 20

// upload is the legacy upload API that twine and uv use (docs/spec/http-routing.md §3, pypi):
// POST /pypi/{repo}/ with multipart/form-data, :action=file_upload, protocol_version=1, the file
// in content, and the metadata fields. The file is spooled to disk as it streams in, with its
// sha256, md5 and blake2b-256, and those the client sent must match, as must the name, version
// and filetype it sent with what the file name says, all before anything is published; Build
// makes the metadata layer from the file's own METADATA or PKG-INFO, which must say what the file
// name says. 200 on success and for the same bytes again; 400 with the reason;
// 409 "File already exists" for other bytes under the file name's version and variant, which
// twine --skip-existing skips; 413 over policy.maxFileSize. It needs write on the repo.
func (h *handlers) upload(w http.ResponseWriter, r *http.Request) {
	repo, ok := format.RepoFrom(r.Context())
	if !ok {
		h.fail(w, r, errors.New("pypi: the route has no repo"))
		return
	}
	ctx := r.Context()
	if err := format.Authorize(ctx, h.deps, auth.ActionWrite, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if h.deps.Ingest == nil {
		h.fail(w, r, errors.New("pypi: no publisher in format.Deps"))
		return
	}
	body, err := format.LimitBody(w, r, repo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	r.Body = io.NopCloser(body)
	form, err := readForm(r)
	if form != nil && form.file != nil {
		defer func() {
			if err := form.file.Close(); err != nil {
				h.deps.Log.WarnContext(ctx, "pypi: upload spool not removed", "error", err.Error())
			}
		}()
	}
	if err == nil {
		err = form.check()
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	file := form.file.File()
	pub, err := h.deps.Ingest.Upload(ctx, repo, format.Upload{Files: []format.UploadedFile{file},
		Params: map[string]string{ParamUploadedBy: principal.String()}}, principal)
	var conflict *index.TagConflictError
	switch {
	case errors.As(err, &conflict) || errors.Is(err, index.ErrConflict):
		http.Error(w, "409 File already exists: "+file.Name+" is in the repository with other bytes ("+err.Error()+")", http.StatusConflict)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Docker-Content-Digest", pub.Digest.String())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK "+file.Name+" "+pub.Entry.Name+" "+pub.Entry.Version+"\n")
}

// uploadForm is what readForm read of an upload.
type uploadForm struct {
	fields              map[string]string
	file                *format.Spooled
	sha256, md5, blake2 []byte
}

// readForm reads the multipart form of an upload as it streams: the fields in memory, at most
// maxFields bytes together, the content part spooled to disk and digested on the way. Errors
// wrap format.ErrInvalid, except a body over the size limit (413) and a failure to spool.
func readForm(r *http.Request) (*uploadForm, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("%w: the upload is not multipart/form-data: %w", format.ErrInvalid, err)
	}
	form := &uploadForm{fields: map[string]string{}}
	budget := int64(maxFields)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}
		if err != nil {
			return form, bodyError(err)
		}
		name := part.FormName()
		if name == "content" {
			if form.file != nil {
				return form, fmt.Errorf("%w: the upload has two files; one file is one upload", format.ErrInvalid)
			}
			if err := form.spool(part); err != nil {
				return form, err
			}
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, budget+1))
		if err != nil {
			return form, bodyError(err)
		}
		if budget -= int64(len(b)); budget < 0 {
			return form, fmt.Errorf("%w: the form fields are more than %d bytes", format.ErrInvalid, maxFields)
		}
		if _, repeated := form.fields[name]; !repeated { // classifiers and the like repeat; the first is enough here
			form.fields[name] = string(b)
		}
	}
}

// spool copies the content part to a temporary file with its digests.
func (f *uploadForm) spool(part *multipart.Part) error {
	filename := part.FileName()
	if filename == "" || strings.ContainsAny(filename, "/\\") {
		return fmt.Errorf("%w: the content part has no plain file name: %q", format.ErrInvalid, filename)
	}
	s, m, b := sha256.New(), md5.New(), mustBlake2() //nolint:gosec // see the import
	sp, err := format.Spool(io.TeeReader(part, io.MultiWriter(s, m, b)), filename, part.Header.Get("Content-Type"), -1)
	if err != nil {
		return bodyError(err)
	}
	f.file, f.sha256, f.md5, f.blake2 = sp, s.Sum(nil), m.Sum(nil), b.Sum(nil)
	return nil
}

// mustBlake2 returns BLAKE2b-256 without a key, which cannot fail.
func mustBlake2() hash.Hash {
	h, _ := blake2b.New256(nil)
	return h
}

// bodyError keeps a body over the size limit a 413 (format.LimitBody's error) and makes every
// other error of the multipart stream a 400.
func bodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return err
	}
	return fmt.Errorf("%w: the upload: %w", format.ErrInvalid, err)
}

// check checks the form before the file is published: the action, the protocol version, the
// file, and the digests the client sent against the file's.
func (f *uploadForm) check() error {
	if a := f.fields[":action"]; a != "file_upload" {
		return fmt.Errorf("%w: :action is %q; this API takes file_upload", format.ErrInvalid, a)
	}
	if v, ok := f.fields["protocol_version"]; ok && v != "1" {
		return fmt.Errorf("%w: protocol_version is %q, not 1", format.ErrInvalid, v)
	}
	if f.file == nil {
		return fmt.Errorf("%w: the upload has no content", format.ErrInvalid)
	}
	d, err := describe(f.file.File().Name, "")
	if err != nil {
		return err
	}
	for _, c := range []struct{ field, got, want string }{
		{"name", Normalize(f.fields["name"]), Normalize(d.name)},
		{"version", f.fields["version"], d.version},
		{"filetype", f.fields["filetype"], d.packagetype},
	} {
		if f.fields[c.field] != "" && c.got != c.want {
			return fmt.Errorf("%w: the field %s is %q, the file %s says %q", format.ErrInvalid, c.field, f.fields[c.field], f.file.File().Name, c.want)
		}
	}
	for _, d := range []struct {
		field string
		got   []byte
	}{{"sha256_digest", f.sha256}, {"md5_digest", f.md5}, {"blake2_256_digest", f.blake2}} {
		sent := strings.TrimSpace(f.fields[d.field])
		if sent == "" {
			continue
		}
		if !sameDigest(sent, d.got) {
			return fmt.Errorf("%w: %s %s is not the digest of the file sent, %x", format.ErrInvalid, d.field, sent, d.got)
		}
	}
	return nil
}

// sameDigest compares a digest the client sent, in hex or, as old clients send md5_digest, in
// URL-safe base64 without padding, with sum.
func sameDigest(sent string, sum []byte) bool {
	if b, err := hex.DecodeString(sent); err == nil {
		return string(b) == string(sum)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sent, "="))
	return err == nil && string(b) == string(sum)
}
