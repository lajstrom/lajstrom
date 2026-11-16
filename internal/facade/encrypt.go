package facade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// Encrypted repos (docs/spec/http-routing.md §2.1, manifest-profile.md §11). The facade encrypts
// every byte a client uploads into a repo whose policy.encryption.mode is at-rest, and answers
// each client in the form it pushed: a blob by its plaintext digest decrypted, a manifest as the
// client sent it under its digest. blob_map maps the digests a client knows to those of the
// registry; a repo whose policy has encryption, in either mode, reads through it, since it may
// hold encrypted layers.

// encState is the encryption of an upload session, in its row (uploadState.Enc): the session's
// data key sealed with the master key, its nonce, the sha256 state of the ciphertext so far, the
// HMAC of the ciphertext while one request only carried bytes, and how many requests did.
type encState struct {
	Key    []byte `json:"key"`
	Nonce  []byte `json:"nonce"`
	Stored []byte `json:"stored,omitempty"`
	HMAC   []byte `json:"hmac,omitempty"`
	Bodies int    `json:"bodies"`
}

// errNoMasterKey is the answer to a write to an encrypted repo on a server without the keys.
var errNoMasterKey = errors.New("facade: the repo encrypts at rest, and this server has no master key (keys.masterKeyEnv)")

// encrypts reports whether the uploads of t's repo are encrypted; a server without the keys
// cannot, and that is an error.
func (h *handler) encrypts(t *target) (bool, error) {
	pol, err := format.EncryptionOf(t.repo)
	switch {
	case err != nil:
		return false, err
	case !pol.AtRest():
		return false, nil
	case h.o.Encryption == nil:
		return false, errNoMasterKey
	}
	return true, nil
}

// mapped reports whether the reads of t's repo go through blob_map: its policy has encryption, in
// either mode. An oci repo and a group never encrypt.
func mapped(t *target) bool {
	if len(t.repo.Policy) == 0 || t.repo.Format == ociFormat || t.repo.Kind == index.KindGroup {
		return false
	}
	var p struct {
		Encryption json.RawMessage `json:"encryption"`
	}
	return json.Unmarshal(t.repo.Policy, &p) == nil && len(p.Encryption) > 0 && string(p.Encryption) != "null"
}

// newEncState returns the encryption of the new upload session id: a new data key, sealed, and
// nonce.
func (h *handler) newEncState(id string) *encState {
	opts := blobcrypt.NewPrivate()
	return &encState{Key: h.o.Encryption.Seal(uploadKind+id, opts.Key), Nonce: opts.Nonce()}
}

// private returns the options of the session id: its data key opened with the master key.
func (h *handler) private(id string, e *encState) (blobcrypt.Private, error) {
	key, err := h.o.Encryption.Open(uploadKind+id, e.Key)
	if err != nil {
		return blobcrypt.Private{}, fmt.Errorf("facade: upload session %s: %w", id, err)
	}
	p := blobcrypt.Private{Key: key, CipherOptions: map[string][]byte{"nonce": e.Nonce}}
	return p, p.Check()
}

// storedHash returns the sha256 of the ciphertext that passed in the session, from its state.
func (e *encState) storedHash() (hash.Hash, error) {
	h := sha256.New()
	if e.Stored != nil {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(e.Stored); err != nil {
			return nil, fmt.Errorf("facade: upload session: ciphertext sha256 state: %w", err)
		}
	}
	return h, nil
}

// chunk is the encryption of one request's body: the reader of its ciphertext, from the session's
// offset, and the hashes it feeds.
type chunk struct {
	r      io.Reader
	stored hash.Hash
	mac    hash.Hash // nil when another request carried bytes before: the read back computes it
}

// encryptChunk returns the chunk of plain, the plaintext body of a request on the session id at
// offset, in st's encryption.
func (h *handler) encryptChunk(id string, e *encState, offset int64, plain io.Reader) (*chunk, error) {
	opts, err := h.private(id, e)
	if err != nil {
		return nil, err
	}
	s, err := blobcrypt.At(opts, offset)
	if err != nil {
		return nil, err
	}
	stored, err := e.storedHash()
	if err != nil {
		return nil, err
	}
	c := &chunk{stored: stored}
	to := []io.Writer{stored}
	if e.Bodies == 0 && offset == 0 {
		c.mac = blobcrypt.NewMAC(opts)
		to = append(to, c.mac)
	}
	c.r = blobcrypt.EncryptReader(plain, s, to...)
	return c, nil
}

// passed records in e a chunk that the backend took, with bytes: the ciphertext's sha256 state,
// and the HMAC while it is the session's only body.
func (c *chunk) passed(e *encState) error {
	state, err := c.stored.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return fmt.Errorf("facade: upload session: ciphertext sha256 state: %w", err)
	}
	e.Stored, e.Bodies = state, e.Bodies+1
	e.HMAC = nil
	if c.mac != nil && e.Bodies == 1 {
		e.HMAC = c.mac.Sum(nil)
	}
	return nil
}

// finishEncrypted closes the encrypted session st of t, the backend's session u, with the
// client's digest d: the last body (nil: none) is appended through the cipher, its plaintext
// checked against d with its last byte held back, then the backend's session is closed with the
// ciphertext's digest. A session whose bytes came in more than one request reads its blob back
// once for the HMAC. The data key, with d as the plaintext digest, is wrapped for the repo's key,
// and the blob's row of blob_map written. hs is the hash of the plaintext before body.
func (h *handler) finishEncrypted(ctx context.Context, t *target, id string, rr provider.Repository, u provider.Upload, st *uploadState,
	d digest.Digest, hs hash.Hash, body *bodyReader, size int64) error {
	e := st.Enc
	if body != nil {
		vr := &verifier{r: body, h: hs, want: d}
		c, err := h.encryptChunk(id, e, u.Offset, vr)
		if err != nil {
			return err
		}
		next, err := rr.AppendBlobChunk(ctx, u, c.r, size)
		switch {
		case errors.Is(vr.end, errNotDigest):
			return errNotDigest
		case err != nil:
			return err
		}
		if err := c.passed(e); err != nil {
			return err
		}
		u = next
	} else if digest.NewDigest(d.Algorithm(), hs) != d {
		return errNotDigest
	}
	stored, err := e.storedHash()
	if err != nil {
		return err
	}
	cd := digest.NewDigest(digest.SHA256, stored)
	if _, err := rr.FinishBlobUpload(ctx, u, cd, nil, 0); err != nil {
		return err
	}
	opts, err := h.private(id, e)
	if err != nil {
		return err
	}
	mac, err := h.blobMAC(ctx, rr, opts, e, cd, u.Offset)
	if err != nil {
		return err
	}
	opts.Digest = d
	jwe, fp, err := h.o.Encryption.Wrap(ctx, t.repo, opts)
	if err != nil {
		return fmt.Errorf("facade: wrap the data key of %s: %w", d, err)
	}
	err = h.o.Index.PutBlobMap(ctx, []index.BlobMap{{RepoID: t.repo.ID, Path: t.path, Kind: index.BlobMapBlob, Plain: d, Stored: cd,
		Size: u.Offset, KeyFingerprint: fp, Wrapped: jwe, PubOpts: blobcrypt.EncodePublic(mac)}})
	if err != nil {
		return fmt.Errorf("%w: %w", errRowAfterClose, err)
	}
	return nil
}

// blobMAC returns the HMAC of the session's ciphertext, cd of size bytes: the one the row kept
// when one request carried all its bytes, else computed by reading the committed blob back.
func (h *handler) blobMAC(ctx context.Context, rr provider.Repository, opts blobcrypt.Private, e *encState, cd digest.Digest, size int64) ([]byte, error) {
	switch {
	case e.Bodies == 1 && e.HMAC != nil:
		return e.HMAC, nil
	case e.Bodies == 0:
		return blobcrypt.NewMAC(opts).Sum(nil), nil
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: octetStream, Digest: cd, Size: size})
	if err != nil {
		return nil, fmt.Errorf("facade: read back %s for its HMAC: %w", cd, err)
	}
	defer func() { _ = rc.Close() }()
	m, sum := blobcrypt.NewMAC(opts), sha256.New()
	if _, err := io.Copy(io.MultiWriter(m, sum), rc); err != nil {
		return nil, fmt.Errorf("facade: read back %s for its HMAC: %w", cd, err)
	}
	if digest.NewDigest(digest.SHA256, sum) != cd {
		return nil, fmt.Errorf("facade: read back %s for its HMAC: the registry served other bytes", cd)
	}
	return m.Sum(nil), nil
}

// blobRow returns the row of blob_map of the plaintext digest d at t's path, nil when there is
// none.
func (h *handler) blobRow(ctx context.Context, t *target, d digest.Digest) (*index.BlobMap, error) {
	rows, err := h.o.Index.FindBlobMap(ctx, t.repo.ID, t.path, d, index.BlobMapBlob)
	if err != nil {
		return nil, fmt.Errorf("facade: blob_map of %s at %s: %w", d, t.name, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// serveEncrypted answers HEAD and GET of the plaintext digest d at t when blob_map maps it: the
// ciphertext of row, decrypted, never redirected, with d as Docker-Content-Digest and ETag.
func (h *handler) serveEncrypted(w http.ResponseWriter, r *http.Request, t *target, d digest.Digest, row *index.BlobMap, p provider.Provider) error {
	if h.o.Encryption == nil {
		return errNoMasterKey
	}
	opts := format.ServeBlobOptions{ContentType: octetStream, ETag: `"` + d.String() + `"`, ManifestDigest: d,
		Fail: func(w http.ResponseWriter, err error) { h.fail(w, r, blobError(err)) }}
	if r.Method != http.MethodHead {
		key, err := format.OpenBlobKey(r.Context(), h.o.Encryption, t.repo, row)
		if err != nil {
			return err
		}
		opts.Decrypt = key
	}
	if err := format.ServeBlob(w, r, p, t.oci, row.Stored, opts); err != nil {
		h.logProxy(r.Context(), t, d, err)
	}
	return nil
}

// checkEncryptedLayers is checkLayers for a manifest pushed to an encrypted repo: every layer
// must have been uploaded there encrypted, its row of blob_map at the path and its ciphertext in
// the backend, else MANIFEST_BLOB_UNKNOWN naming the client's digest.
func (h *handler) checkEncryptedLayers(ctx context.Context, t *target, layers []ocispec.Descriptor) error {
	stored := make([]ocispec.Descriptor, 0, len(layers))
	for _, l := range layers {
		row, err := h.blobRow(ctx, t, l.Digest)
		if err != nil {
			return err
		}
		if row == nil {
			return &apiError{status: http.StatusBadRequest, code: codeManifestBlobUnknown,
				msg: fmt.Sprintf("manifest blob unknown to registry: blob %s was not uploaded to %s encrypted", l.Digest, t.name)}
		}
		stored = append(stored, ocispec.Descriptor{MediaType: octetStream, Digest: row.Stored, Size: row.Size})
	}
	return h.checkLayers(ctx, t, stored)
}

// manifestRow returns the row of blob_map of the manifest whose digest as the client had it is d,
// at t's path; nil when there is none.
func (h *handler) manifestRow(ctx context.Context, t *target, d digest.Digest) (*index.BlobMap, error) {
	rows, err := h.o.Index.FindBlobMap(ctx, t.repo.ID, t.path, d, index.BlobMapManifest)
	if err != nil {
		return nil, fmt.Errorf("facade: blob_map of the manifest %s at %s: %w", d, t.name, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// clientForm returns the manifest raw, stored at t, as the client had it (DecryptManifest): its
// encrypted layers' plaintext digests from blob_map, its subject's digest as the client had it
// when blob_map maps it. A manifest without encrypted layers and a mapped subject comes back as
// it is.
func (h *handler) clientForm(ctx context.Context, t *target, raw []byte) ([]byte, error) {
	layers, subject, err := profile.EncryptedLayers(raw)
	if err != nil {
		return raw, nil //nolint:nilerr // not a manifest the change touches: answered as it is
	}
	plain := make(map[digest.Digest]digest.Digest, len(layers))
	for _, l := range layers {
		row, err := h.o.Index.GetBlobMap(ctx, t.repo.ID, t.path, l.Stored)
		switch {
		case errors.Is(err, index.ErrNotFound):
			return nil, fmt.Errorf("facade: the encrypted layer %s of a manifest at %s has no row in blob_map: reindex writes it", l.Stored, t.name)
		case err != nil:
			return nil, err
		}
		plain[l.Stored] = row.Plain
	}
	var subjectPlain digest.Digest
	if subject != "" {
		row, err := h.o.Index.GetBlobMap(ctx, t.repo.ID, t.path, subject)
		if err == nil && row.Kind == index.BlobMapManifest {
			subjectPlain = row.Plain
		}
	}
	if len(layers) == 0 && subjectPlain == "" {
		return raw, nil
	}
	return profile.DecryptManifest(raw, func(d digest.Digest) (digest.Digest, bool) { p, ok := plain[d]; return p, ok }, subjectPlain)
}

// getMappedManifest answers HEAD and GET of a manifest of a repo that reads through blob_map: a
// tag or a digest as the client had it is the stored manifest answered in the client's form, the
// stored digest the stored manifest as it is. The manifest is read whole, at most
// MaxManifestSize, and checked against its digest before anything goes out.
func (h *handler) getMappedManifest(w http.ResponseWriter, r *http.Request, t *target, ref string) error {
	ctx := r.Context()
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	asStored := false
	if d, err := digest.Parse(ref); err == nil {
		row, err := h.manifestRow(ctx, t, d)
		if err != nil {
			return err
		}
		if row != nil {
			ref = row.Stored.String()
		} else {
			asStored = true
		}
	}
	desc, err := rr.Resolve(ctx, ref)
	if err != nil {
		return manifestUnknown(err)
	}
	rc, err := rr.Fetch(ctx, desc)
	if err != nil {
		return manifestUnknown(err)
	}
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(io.LimitReader(rc, h.o.MaxManifestSize+1))
	if err != nil {
		return manifestUnknown(err)
	}
	if desc.Digest.Validate() == nil && desc.Digest.Algorithm().FromBytes(raw) != desc.Digest {
		return fmt.Errorf("facade: manifest %s@%s: the bytes from registry %s do not have the digest", t.name, desc.Digest, t.repo.Registry)
	}
	answer := raw
	if !asStored {
		if answer, err = h.clientForm(ctx, t, raw); err != nil {
			return err
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", desc.MediaType)
	hdr.Set("Docker-Content-Digest", digest.FromBytes(answer).String())
	hdr.Set("Content-Length", fmt.Sprint(len(answer)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(answer)
	}
	return nil
}

// sealReferrer changes the referrer body of an encrypted repo, as the client sent it, into the
// stored one (manifest-profile.md §11): its layers' ciphertexts from blob_map, its subject the
// stored digest of the subject when blob_map maps it, and pushes its config in the clear when the
// backend has only its ciphertext (ocicrypt never encrypts a config). It returns the stored bytes
// and the subject as the backend has it.
func (h *handler) sealReferrer(ctx context.Context, t *target, body []byte, m ocispec.Manifest, subject digest.Digest) ([]byte, digest.Digest, error) {
	layers := make(map[digest.Digest]profile.EncryptedLayer, len(m.Layers))
	for _, l := range m.Layers {
		row, err := h.blobRow(ctx, t, l.Digest)
		if err != nil {
			return nil, "", err
		}
		if row == nil {
			return nil, "", &apiError{status: http.StatusBadRequest, code: codeManifestBlobUnknown,
				msg: fmt.Sprintf("manifest blob unknown to registry: blob %s was not uploaded to %s encrypted", l.Digest, t.name)}
		}
		layers[l.Digest] = profile.EncryptedLayer{Stored: row.Stored, JWE: row.Wrapped, PubOpts: row.PubOpts}
	}
	if err := h.plainConfig(ctx, t, m.Config); err != nil {
		return nil, "", err
	}
	storedSubject := subject
	if subject != "" {
		row, err := h.manifestRow(ctx, t, subject)
		if err != nil {
			return nil, "", err
		}
		if row != nil {
			storedSubject = row.Stored
		}
	}
	stored, err := profile.EncryptManifest(body, layers, map[bool]digest.Digest{true: storedSubject}[storedSubject != subject])
	if err != nil {
		return nil, "", &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: validationText(err), err: err}
	}
	return stored, storedSubject, nil
}

// plainConfig pushes the config desc in the clear when the backend does not have it and blob_map
// has its ciphertext: the client uploaded it through the facade, which encrypted it.
func (h *handler) plainConfig(ctx context.Context, t *target, desc ocispec.Descriptor) error {
	rr, err := h.backend(ctx, t)
	if err != nil {
		return err
	}
	if ok, err := rr.Exists(ctx, desc); err != nil || ok {
		return err
	}
	row, err := h.blobRow(ctx, t, desc.Digest)
	if err != nil || row == nil {
		return err // the backend's own answer names a config it does not have
	}
	if row.Size > h.o.MaxManifestSize {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: fmt.Sprintf("the config %s is %d bytes, larger than a manifest may be", desc.Digest, row.Size)}
	}
	key, err := format.OpenBlobKey(ctx, h.o.Encryption, t.repo, row)
	if err != nil {
		return err
	}
	rc, err := rr.Fetch(ctx, ocispec.Descriptor{MediaType: octetStream, Digest: row.Stored, Size: row.Size})
	if err != nil {
		return fmt.Errorf("facade: the ciphertext of the config %s: %w", desc.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	pr, err := blobcrypt.Open(rc, key.Private, key.HMAC)
	if err != nil {
		return err
	}
	plain, err := io.ReadAll(pr)
	if err != nil {
		return fmt.Errorf("facade: decrypt the config %s: %w", desc.Digest, err)
	}
	return rr.Push(ctx, desc, bytes.NewReader(plain))
}

// clientReferrers returns the referrers refs of a stored manifest at t as the client knows them:
// Lajstrom's keys referrers left out, each other's digest and size those of its manifest in the
// client's form.
func (h *handler) clientReferrers(ctx context.Context, t *target, rr provider.Repository, refs []ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	out := make([]ocispec.Descriptor, 0, len(refs))
	for _, d := range refs {
		if d.ArtifactType == profile.ArtifactTypeKeys {
			continue
		}
		row, err := h.o.Index.GetBlobMap(ctx, t.repo.ID, t.path, d.Digest)
		if errors.Is(err, index.ErrNotFound) {
			out = append(out, d)
			continue
		}
		if err != nil {
			return nil, err
		}
		rc, err := rr.Fetch(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("facade: the referrer %s: %w", d.Digest, err)
		}
		raw, err := io.ReadAll(io.LimitReader(rc, h.o.MaxManifestSize+1))
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("facade: the referrer %s: %w", d.Digest, err)
		}
		view, err := h.clientForm(ctx, t, raw)
		if err != nil {
			return nil, err
		}
		if digest.FromBytes(view) != row.Plain {
			return nil, fmt.Errorf("facade: the referrer %s in the client's form is not %s", d.Digest, row.Plain)
		}
		d.Digest, d.Size = row.Plain, int64(len(view))
		out = append(out, d)
	}
	return out, nil
}

// errRowAfterClose is the error of an upload whose row of blob_map could not be written after the
// backend took the blob: the session ends, and the client uploads again (500).
var errRowAfterClose = errors.New("facade: the encrypted blob was stored, and its row of blob_map was not written: upload it again")

// monolithicEncrypted is monolithic for an encrypted repo: the body, the whole blob, appended to
// the backend's session u through the cipher and closed with the ciphertext's digest
// (finishEncrypted). encErr is the error of encrypts.
func (h *handler) monolithicEncrypted(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, u provider.Upload, d digest.Digest,
	body *bodyReader, size int64, encErr error) error {
	ctx := r.Context()
	if encErr != nil {
		h.cancelBackend(ctx, rr, u)
		return encErr
	}
	id, err := newUploadID()
	if err != nil {
		h.cancelBackend(ctx, rr, u)
		return err
	}
	st := uploadState{Name: t.name, Location: u.Location, ID: u.ID, Offset: u.Offset, Enc: h.newEncState(id)}
	if err := h.finishEncrypted(ctx, t, id, rr, u, &st, d, d.Algorithm().Hash(), body, size); err != nil {
		h.cancelBackend(ctx, rr, u)
		return finishError(err, body)
	}
	answerBlob(w, t, d)
	return nil
}

// mountEncrypted is mount for an encrypted repo: a source in the same repo whose row of blob_map
// maps the plaintext digest d mounts its ciphertext, and the row is written at t's path; any other
// source, or none, gets a session, encrypted.
func (h *handler) mountEncrypted(w http.ResponseWriter, r *http.Request, t *target, rr provider.Repository, d digest.Digest, from, src string, ok bool) error {
	ctx := r.Context()
	if ok {
		if st, err := h.resolve(ctx, from); err == nil && st.repo.ID == t.repo.ID {
			row, err := h.blobRow(ctx, st, d)
			if err != nil {
				return err
			}
			if row != nil {
				return h.openSession(w, r, t, rr, d, func(ctx context.Context) (bool, provider.Upload, error) {
					mounted, u, err := rr.MountBlob(ctx, row.Stored, src)
					if err == nil && mounted {
						at := *row
						at.ID, at.Path, at.IndexedAt = 0, t.path, h.o.Now()
						if err := h.o.Index.PutBlobMap(ctx, []index.BlobMap{at}); err != nil {
							return false, u, fmt.Errorf("facade: blob_map of the mount of %s: %w", d, err)
						}
					}
					return mounted, u, err
				})
			}
		}
	}
	return h.openSession(w, r, t, rr, "", startSession(rr))
}

// checkPushedLayers checks the layers of a manifest pushed to a managed repo before ingest: none
// that the client encrypted itself, and each in the backend (checkLayers), encrypted on an
// encrypted repo (checkEncryptedLayers).
func (h *handler) checkPushedLayers(ctx context.Context, t *target, parsed *profile.Parsed) error {
	infos := make([]profile.LayerInfo, 0, len(parsed.Files)+1)
	descs := make([]ocispec.Descriptor, 0, len(parsed.Files)+1)
	all := parsed.Files
	if parsed.Metadata != nil {
		all = append(all[:len(all):len(all)], *parsed.Metadata)
	}
	for _, l := range all {
		ann := l.Annotations
		infos = append(infos, profile.LayerInfo{MediaType: l.MediaType, Digest: l.Digest, Annotations: ann})
		descs = append(descs, ocispec.Descriptor{MediaType: l.MediaType, Digest: l.Digest, Size: l.Size})
	}
	if err := profile.CheckPlainLayers(infos); err != nil {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid, msg: validationText(err), err: err}
	}
	encrypted, err := h.encrypts(t)
	switch {
	case err != nil:
		return err
	case encrypted:
		return h.checkEncryptedLayers(ctx, t, descs)
	}
	return h.checkLayers(ctx, t, descs)
}

// putEncryptedReferrer is putReferrer for an encrypted repo: the referrer is stored changed
// (sealReferrer), under the referrers lock of the stored subject, its row of blob_map written, and
// the answer names the client's digest and subject. A referrer under cosign's legacy tag, which
// names the subject by the client's digest, is refused: the referrers API is the way.
func (h *handler) putEncryptedReferrer(w http.ResponseWriter, r *http.Request, t *target, tag string, desc ocispec.Descriptor, body []byte,
	m ocispec.Manifest, subject digest.Digest) error {
	ctx := r.Context()
	if tag != "" {
		return &apiError{status: http.StatusBadRequest, code: codeManifestInvalid,
			msg: "an encrypted repo takes referrers through the referrers API, by digest, not under a tag: cosign --registry-referrers-mode=oci-1-1"}
	}
	stored, storedSubject, err := h.sealReferrer(ctx, t, body, m, subject)
	if err != nil {
		return err
	}
	sd := ocispec.Descriptor{MediaType: desc.MediaType, Digest: digest.FromBytes(stored), Size: int64(len(stored)), ArtifactType: desc.ArtifactType}
	if err := h.ingestReferrer(r, t, "", sd, stored, storedSubject); err != nil {
		return err
	}
	err = h.o.Index.PutBlobMap(ctx, []index.BlobMap{{RepoID: t.repo.ID, Path: t.path, Kind: index.BlobMapManifest, Plain: desc.Digest,
		Stored: sd.Digest, Size: sd.Size}})
	if err != nil {
		return fmt.Errorf("facade: blob_map of the referrer %s: %w", desc.Digest, err)
	}
	created(w, t, desc.Digest, m.Subject)
	return nil
}

// storedRef returns the reference of a manifest at t as the backend has it: a digest that
// blob_map maps is the client's form of its stored manifest; anything else is the backend's.
func (h *handler) storedRef(ctx context.Context, t *target, ref string) (string, error) {
	if !mapped(t) {
		return ref, nil
	}
	d, err := digest.Parse(ref)
	if err != nil {
		return ref, nil //nolint:nilerr // a tag
	}
	row, err := h.manifestRow(ctx, t, d)
	if err != nil || row == nil {
		return ref, err
	}
	return row.Stored.String(), nil
}
