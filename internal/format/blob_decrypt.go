package format

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// BlobKey is an opened encrypted layer (docs/spec/manifest-profile.md §11): its options, the data
// key, the nonce and the plaintext digest, and the HMAC of its ciphertext. ServeBlob decrypts a
// blob it is given with.
type BlobKey struct {
	Private blobcrypt.Private
	HMAC    []byte
}

// Decryption returns the key of the file f of the version at the package path path of repo, for
// ServeBlobOptions.Decrypt: nil for a plain layer; for an encrypted one, its row of blob_map
// opened with the repo's key (Deps.Encryption). A row that is missing, or a server without the
// keys, is an error (500): reindex writes the row, with the keys. A KMS that does not answer is
// ErrUnavailable (503).
func Decryption(ctx context.Context, d Deps, repo index.Repo, path string, f index.File) (*BlobKey, error) {
	if _, ok := f.Attrs[profile.AnnotationEncPubOpts]; !ok {
		return nil, nil
	}
	if d.Encryption == nil {
		return nil, fmt.Errorf("format: %s/%s: the file %s is encrypted, and this server has no master key", repo.Format, repo.Name, f.Name)
	}
	row, err := d.Index.GetBlobMap(ctx, repo.ID, path, f.Digest)
	if err != nil {
		return nil, fmt.Errorf("format: %s/%s: the encryption of %s (%s): %w", repo.Format, repo.Name, f.Name, f.Digest, errors.Join(err,
			errors.New("reindex writes it with the repo's keys")))
	}
	return OpenBlobKey(ctx, d.Encryption, repo, row)
}

// OpenBlobKey opens the encrypted layer of row, a row of blob_map of kind blob, with the repo's key.
func OpenBlobKey(ctx context.Context, enc Encryption, repo index.Repo, row *index.BlobMap) (*BlobKey, error) {
	opts, err := enc.Unwrap(ctx, repo, row.KeyFingerprint, row.Wrapped)
	if err != nil {
		return nil, fmt.Errorf("format: the data key of %s: %w", row.Stored, err)
	}
	mac, err := blobcrypt.DecodePublic(row.PubOpts)
	if err != nil {
		return nil, fmt.Errorf("format: the public options of %s: %w", row.Stored, err)
	}
	if opts.Digest != row.Plain {
		return nil, fmt.Errorf("format: the wrapped options of %s name the plaintext %s, its row %s", row.Stored, opts.Digest, row.Plain)
	}
	return &BlobKey{Private: opts, HMAC: mac}, nil
}

// getDecrypted answers GET of an encrypted layer: one OpenBlob, never redirected, with the
// client's Range as get sends it. A whole blob (200) is decrypted as it streams, and its last byte
// held back until the HMAC of the ciphertext and the plaintext digest matched; otherwise the
// response is aborted (docs/spec/guarantees.md, INV-40). A range (206) is decrypted from the
// first byte of the backend's Content-Range, not authenticated, as a range of a plain blob is not
// hashed. The headers are get's: the ciphertext is as long as the plaintext.
func (a *blobAnswer) getDecrypted() error {
	rng := ""
	if a.v.rangeAllowed(a.r) {
		rng = singleByteRange(a.r.Header.Get("Range"))
	}
	b, err := a.prov.OpenBlob(a.r.Context(), a.name, a.d, rng, false)
	if err != nil {
		a.fail(a.w, err)
		return nil
	}
	if b.Body != nil {
		defer func() { _ = b.Body.Close() }()
	}
	if b.Body == nil && b.Status != http.StatusRequestedRangeNotSatisfiable || b.RedirectURL != "" {
		a.fail(a.w, fmt.Errorf("format: serve blob %s@%s: the provider answered %d without a body", a.name, a.d, b.Status))
		return nil
	}
	var plain io.Reader
	switch status := cmp.Or(b.Status, http.StatusOK); status {
	case http.StatusRequestedRangeNotSatisfiable:
	case http.StatusOK:
		if plain, err = blobcrypt.Open(b.Body, a.opts.Decrypt.Private, a.opts.Decrypt.HMAC); err != nil {
			a.fail(a.w, err)
			return nil
		}
	case http.StatusPartialContent:
		first, ok := rangeStart(b.ContentRange)
		s, serr := blobcrypt.At(a.opts.Decrypt.Private, first)
		if !ok || serr != nil {
			a.fail(a.w, fmt.Errorf("format: serve blob %s@%s: a range the backend answered with Content-Range %q", a.name, a.d, b.ContentRange))
			return nil
		}
		plain = blobcrypt.DecryptReader(b.Body, s)
	default:
		a.fail(a.w, fmt.Errorf("format: serve blob %s@%s: the provider answered %d", a.name, a.d, status))
		return nil
	}
	setBlobHeaders(a.w.Header(), b, a.opts, a.v)
	a.w.WriteHeader(cmp.Or(b.Status, http.StatusOK))
	if plain == nil {
		return nil
	}
	return a.copyDecrypted(b, plain)
}

// copyDecrypted streams the plaintext of b to the client, as copy streams a plain blob: a blob
// that does not match is aborted, a short body with a Content-Length returned, one without
// aborted.
func (a *blobAnswer) copyDecrypted(b provider.Blob, plain io.Reader) error {
	n, err := io.Copy(a.w, plain)
	if errors.Is(err, blobcrypt.ErrMismatch) {
		a.abort(n)
	}
	if err == nil && b.Size >= 0 && n < b.Size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil || a.r.Context().Err() != nil {
		return nil
	}
	err = fmt.Errorf("format: blob %s@%s: the stream failed after %d bytes: %w", a.name, a.d, n, err)
	if b.Size < 0 {
		panic(fmt.Errorf("%w; it had no Content-Length, the response is aborted: %w", err, http.ErrAbortHandler))
	}
	return err
}

// rangeStart is the first byte of a Content-Range "bytes <first>-<last>/<size>".
func rangeStart(cr string) (int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(cr), "bytes ")
	first, _, found := strings.Cut(spec, "-")
	if !ok || !found {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	return n, err == nil && n >= 0
}
