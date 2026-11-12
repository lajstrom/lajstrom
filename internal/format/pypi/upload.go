package pypi

import (
	"context"
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
	"slices"
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
// in content, and the metadata fields. The file streams into the registry as it comes, with its
// sha256, md5 and blake2b-256 (ADR 0014), and the rest of the form is read when it ends: the
// digests the client sent must be the file's, and the name, version and filetype it sent what
// the file name says, or nothing is stored. Build makes the metadata layer from the file's own
// METADATA or PKG-INFO, which must say what the file name says. 200 on success and for the same
// bytes again; 400 with the reason; 409 "File already exists" for other bytes under the file
// name's version and variant, which twine --skip-existing skips; 413 over policy.maxFileSize.
// A wheel with a build tag that the index has under the earlier form of its variant goes to that
// tag (ParamVariant). It needs write on the repo.
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
	form, err := newForm(r)
	var part *multipart.Part
	if err == nil {
		part, err = form.next()
	}
	var d dist
	switch {
	case err != nil:
	case part == nil:
		err = form.check()
	default:
		d, err = describe(form.filename, "")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	principal := auth.PrincipalFrom(ctx)
	params := map[string]string{ParamUploadedBy: principal.String()}
	if d.earlier != "" {
		earlier, err := h.atEarlierVariant(ctx, repo, d, form.filename)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if earlier {
			params[ParamVariant] = d.earlier
		}
	}
	pub, err := h.deps.Ingest.UploadStream(ctx, repo, format.StreamUpload{Name: d.name,
		File:   format.UploadedFile{Name: form.filename, Size: -1, ContentType: part.Header.Get("Content-Type")},
		Body:   &content{part: part, form: form},
		Params: params}, principal)
	var conflict *index.TagConflictError
	switch {
	case errors.As(err, &conflict) || errors.Is(err, index.ErrConflict):
		http.Error(w, h.conflictMessage(ctx, repo, form.filename, conflict, err), http.StatusConflict)
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Docker-Content-Digest", pub.Digest.String())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "OK "+form.filename+" "+pub.Entry.Name+" "+pub.Entry.Version+"\n")
}

// atEarlierVariant reports whether the index has filename, a wheel with a build tag, in the
// version of d whose variant is the earlier form, the tag set alone, as uploads wrote it before
// #726.
func (h *handlers) atEarlierVariant(ctx context.Context, repo index.Repo, d dist, filename string) (bool, error) {
	matches, err := h.deps.Index.FindFiles(ctx, repo.ID, filename)
	if err != nil {
		return false, fmt.Errorf("pypi: find %s: %w", filename, err)
	}
	return slices.ContainsFunc(matches, func(m index.FileMatch) bool {
		return m.Package.Name == Normalize(d.name) && m.Version.Version == d.version && m.Version.Variant == d.earlier
	}), nil
}

// conflictMessage is the body of the 409 of an upload of filename: "File already exists", which
// twine --skip-existing looks for, naming the file the tag holds, which the index has under the
// digest of the conflict; that is filename, but for a wheel with a build tag written before #726,
// whose tag is the one of the same file name without it.
func (h *handlers) conflictMessage(ctx context.Context, repo index.Repo, filename string, conflict *index.TagConflictError, err error) string {
	held := filename
	if conflict != nil {
		if v, verr := h.deps.Index.GetVersionByDigest(ctx, repo.ID, conflict.Existing); verr == nil {
			if files, ferr := h.deps.Index.ListFiles(ctx, v.ID); ferr == nil && len(files) > 0 {
				held = files[0].Name
			}
		}
	}
	if held == filename {
		return "409 File already exists: " + filename + " is in the repository with other bytes (" + err.Error() + ")"
	}
	return "409 File already exists: " + held + " is in the repository under the version and tags of " + filename +
		", which cannot be stored beside it (" + err.Error() + ")"
}

// uploadForm is the multipart form of an upload as it streams: the fields in memory, at most
// maxFields bytes together, and the digests of the content part, which comes once.
type uploadForm struct {
	mr       *multipart.Reader
	fields   map[string]string
	budget   int64
	filename string // the file name of the content part, once it came
	digests  *digests
}

// newForm returns the form of the upload r. Errors wrap format.ErrInvalid.
func newForm(r *http.Request) (*uploadForm, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("%w: the upload is not multipart/form-data: %w", format.ErrInvalid, err)
	}
	return &uploadForm{mr: mr, fields: map[string]string{}, budget: maxFields, digests: newDigests()}, nil
}

// next reads the fields up to the content part and returns it, nil at the end of the form; a
// second content part is ErrInvalid, as is one without a plain file name. Errors wrap
// format.ErrInvalid, except a body over the size limit (413).
func (f *uploadForm) next() (*multipart.Part, error) {
	for {
		part, err := f.mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		if err != nil {
			return nil, bodyError(err)
		}
		name := part.FormName()
		if name == "content" {
			filename := part.FileName()
			switch {
			case f.filename != "":
				return nil, fmt.Errorf("%w: the upload has two files; one file is one upload", format.ErrInvalid)
			case filename == "" || strings.ContainsAny(filename, "/\\"):
				return nil, fmt.Errorf("%w: the content part has no plain file name: %q", format.ErrInvalid, filename)
			}
			f.filename = filename
			return part, nil
		}
		b, err := io.ReadAll(io.LimitReader(part, f.budget+1))
		if err != nil {
			return nil, bodyError(err)
		}
		if f.budget -= int64(len(b)); f.budget < 0 {
			return nil, fmt.Errorf("%w: the form fields are more than %d bytes", format.ErrInvalid, maxFields)
		}
		if _, repeated := f.fields[name]; !repeated { // classifiers and the like repeat; the first is enough here
			f.fields[name] = string(b)
		}
	}
}

// finish reads the rest of the form once the content part has been read, and checks it.
func (f *uploadForm) finish() error {
	if _, err := f.next(); err != nil {
		return err
	}
	return f.check()
}

// content is the content part of a form as the body of a streamed upload: its bytes, digested
// on the way; when they end, the rest of the form is read and checked (finish), and a form that
// fails is the error of the read that ends the body, so that nothing of the upload is stored.
type content struct {
	part *multipart.Part
	form *uploadForm
}

func (c *content) Read(p []byte) (int, error) {
	n, err := c.part.Read(p)
	_, _ = c.form.digests.Write(p[:n])
	switch {
	case errors.Is(err, io.EOF):
		if ferr := c.form.finish(); ferr != nil {
			return n, ferr
		}
	case err != nil:
		return n, bodyError(err)
	}
	return n, err
}

// digests are the digests of a file that a client may send: sha256, md5 and blake2b-256.
type digests struct {
	sha256, md5, blake2 hash.Hash
	w                   io.Writer
}

func newDigests() *digests {
	d := &digests{sha256: sha256.New(), md5: md5.New(), blake2: mustBlake2()} //nolint:gosec // see the import
	d.w = io.MultiWriter(d.sha256, d.md5, d.blake2)
	return d
}

func (d *digests) Write(p []byte) (int, error) { return d.w.Write(p) }

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
	if f.filename == "" {
		return fmt.Errorf("%w: the upload has no content", format.ErrInvalid)
	}
	d, err := describe(f.filename, "")
	if err != nil {
		return err
	}
	for _, c := range []struct{ field, got, want string }{
		{"name", Normalize(f.fields["name"]), Normalize(d.name)},
		{"version", f.fields["version"], d.version},
		{"filetype", f.fields["filetype"], d.packagetype},
	} {
		if f.fields[c.field] != "" && c.got != c.want {
			return fmt.Errorf("%w: the field %s is %q, the file %s says %q", format.ErrInvalid, c.field, f.fields[c.field], f.filename, c.want)
		}
	}
	for _, d := range []struct {
		field string
		got   []byte
	}{{"sha256_digest", f.digests.sha256.Sum(nil)}, {"md5_digest", f.digests.md5.Sum(nil)}, {"blake2_256_digest", f.digests.blake2.Sum(nil)}} {
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
