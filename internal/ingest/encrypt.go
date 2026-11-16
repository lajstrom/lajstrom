package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// Encrypted repos (docs/spec/manifest-profile.md §11, ADR 0009). A publish to a repo whose
// policy.encryption.mode is at-rest has a sealer: every layer goes to the registry encrypted, the
// steps that read a layer before the claim (the checks, the plugin's Validate, a read-back) read
// it decrypted by its plaintext digest, and seal changes the manifest in place after them, so that
// the claim, the push and the rows are those of the stored manifest. The rows of blob_map go into
// the transaction that indexes it. A layer uploaded encrypted beforehand, through the facade or a
// publication session, is found in blob_map by its plaintext digest at the package path.

// sealer is the encryption of one publish.
type sealer struct {
	enc  format.Encryption
	st   index.Reader
	repo index.Repo
	path string // the package path, the path of blob_map's rows
	// blobs are the encrypted layers of the publish by their plaintext digest.
	blobs map[digest.Digest]*sealedBlob
	// ready are the ciphertexts made in memory (sealHeld), which commit pushes as they are.
	ready map[digest.Digest]bool
	// stored is the stored manifest of an Inspect, which seal keeps as it is.
	stored []byte
	// view is the digest of the manifest as the client had it, and rows what confirm writes to
	// blob_map; seal sets both.
	view digest.Digest
	rows []index.BlobMap
	// plain are the file layers of the manifest as the client had it, which seal replaced.
	plain []profile.Layer
}

// sealedBlob is an encrypted layer: its two digests and size, its wrapping and public options,
// and its options, once known.
type sealedBlob struct {
	plain, stored digest.Digest
	size          int64
	wrapped       string // the value of org.opencontainers.image.enc.keys.jwe for fingerprint
	pubOpts       string
	fingerprint   string
	opts          *blobcrypt.Private // encrypted here, or unwrapped
	mac           []byte
}

// errNoMasterKey is the error of a publish to an encrypted repo on a server without the master key.
var errNoMasterKey = errors.New("ingest: the repo encrypts at rest, and this server has no master key (keys.masterKeyEnv)")

// sealerFor returns the sealer of a publish to the package path of repo, nil when the repo does
// not encrypt.
func (p *Pipeline) sealerFor(repo index.Repo, pkgPath string) (*sealer, error) {
	pol, err := format.EncryptionOf(repo)
	if err != nil || !pol.AtRest() {
		return nil, err
	}
	if p.enc == nil {
		return nil, errNoMasterKey
	}
	return &sealer{enc: p.enc, st: p.index, repo: repo, path: pkgPath, blobs: map[digest.Digest]*sealedBlob{},
		ready: map[digest.Digest]bool{}}, nil
}

// lookup returns the encrypted layer of the plaintext digest d: one of the publish, else the
// newest row of blob_map at the path. A layer it does not find is ErrInvalid: it was not uploaded
// into the repository encrypted.
func (s *sealer) lookup(ctx context.Context, d digest.Digest) (*sealedBlob, error) {
	if b, ok := s.blobs[d]; ok {
		return b, nil
	}
	rows, err := s.st.FindBlobMap(ctx, s.repo.ID, s.path, d, index.BlobMapBlob)
	if err != nil {
		return nil, fmt.Errorf("ingest: blob_map of %s at %s: %w", d, s.path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: layer %s was not uploaded to %s/%s encrypted", ErrInvalid, d, s.repo.Format+"/"+s.repo.Name, s.path)
	}
	r := rows[0]
	b := &sealedBlob{plain: d, stored: r.Stored, size: r.Size, wrapped: r.Wrapped, pubOpts: r.PubOpts, fingerprint: r.KeyFingerprint}
	s.blobs[d] = b
	return b, nil
}

// known reports whether the publish has the encrypted layer of the plaintext digest d.
func (s *sealer) known(d digest.Digest) bool {
	if s == nil {
		return false
	}
	_, ok := s.blobs[d]
	return ok
}

// open returns the options of b, unwrapped with the repo's key when the publish did not make them.
func (s *sealer) open(ctx context.Context, b *sealedBlob) (blobcrypt.Private, []byte, error) {
	if b.opts == nil {
		opts, err := s.enc.Unwrap(ctx, s.repo, b.fingerprint, b.wrapped)
		if err != nil {
			return blobcrypt.Private{}, nil, fmt.Errorf("ingest: the data key of %s: %w", b.plain, err)
		}
		mac, err := blobcrypt.DecodePublic(b.pubOpts)
		if err != nil {
			return blobcrypt.Private{}, nil, fmt.Errorf("ingest: the public options of %s: %w", b.plain, err)
		}
		if opts.Digest != b.plain {
			return blobcrypt.Private{}, nil, fmt.Errorf("%w: the wrapped options of %s name the plaintext %s", ErrInvalid, b.stored, opts.Digest)
		}
		b.opts, b.mac = &opts, mac
	}
	return *b.opts, b.mac, nil
}

// storedDesc is the descriptor of b's ciphertext in the registry.
func (b *sealedBlob) storedDesc() ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: sessionBlobType, Digest: b.stored, Size: b.size}
}

// newBlob returns the options of a new encrypted blob of the plaintext digest d.
func newBlob(d digest.Digest) *sealedBlob {
	opts := blobcrypt.NewPrivate()
	opts.Digest = d
	return &sealedBlob{plain: d, opts: &opts}
}

// cipherWriter encrypts what is written to it into w, a blob writer of the registry, and hashes
// the ciphertext: its sha256, the stored digest, and its HMAC. It does not change the bytes it is
// given, which an io.MultiWriter hands its other writers too.
type cipherWriter struct {
	w    io.Writer
	s    interface{ XORKeyStream(dst, src []byte) }
	sum  hash.Hash
	mac  hash.Hash
	buf  []byte
	size int64
}

func newCipherWriter(w io.Writer, b *sealedBlob) (*cipherWriter, error) {
	s, err := blobcrypt.At(*b.opts, 0)
	if err != nil {
		return nil, err
	}
	return &cipherWriter{w: w, s: s, sum: sha256.New(), mac: blobcrypt.NewMAC(*b.opts)}, nil
}

func (c *cipherWriter) Write(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		chunk := p[n:min(len(p), n+32<<10)]
		if cap(c.buf) < len(chunk) {
			c.buf = make([]byte, 32<<10)
		}
		out := c.buf[:len(chunk)]
		c.s.XORKeyStream(out, chunk)
		_, _ = c.sum.Write(out) // hashes never fail
		_, _ = c.mac.Write(out)
		w, err := c.w.Write(out)
		n += w
		c.size += int64(w)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// finish records in b what the writer made: the stored digest, the size and the HMAC.
func (c *cipherWriter) finish(b *sealedBlob) {
	b.stored, b.size, b.mac = digest.NewDigest(digest.SHA256, c.sum), c.size, c.mac.Sum(nil)
	b.pubOpts = blobcrypt.EncodePublic(b.mac)
}

// wrap wraps the options of b, which the publish made, for the repo's key and recipients.
func (s *sealer) wrap(ctx context.Context, b *sealedBlob) error {
	jwe, fp, err := s.enc.Wrap(ctx, s.repo, *b.opts)
	if err != nil {
		return fmt.Errorf("ingest: wrap the data key of %s: %w", b.plain, err)
	}
	b.wrapped, b.fingerprint = jwe, fp
	return nil
}

// pushSealed encrypts the blob b into a new blob of t's repository: its plaintext checked against
// its descriptor, its ciphertext committed with its own digest. Once per plaintext digest.
func (t *target) pushSealed(ctx context.Context, b format.BlobSource) error {
	d := b.Descriptor
	if t.sealer.known(d.Digest) {
		return nil
	}
	sb := newBlob(d.Digest)
	if err := t.sealer.wrap(ctx, sb); err != nil {
		return err
	}
	rc, err := b.Open(ctx)
	if err != nil {
		return fmt.Errorf("ingest: open blob %s: %w", d.Digest, err)
	}
	defer func() { _ = rc.Close() }()
	w, err := t.rr.OpenBlobWriter(ctx)
	if err != nil {
		return fmt.Errorf("ingest: open a blob upload in %s: %w", t.name, err)
	}
	cw, err := newCipherWriter(w, sb)
	if err != nil {
		return err
	}
	v := newVerifier(rc, d)
	if _, err := io.Copy(cw, v); err != nil {
		_ = w.Cancel(context.WithoutCancel(ctx))
		if bad := v.failure(); bad != nil {
			return fmt.Errorf("%w: blob %s: the bytes are not the descriptor's: %w", ErrInvalid, d.Digest, bad)
		}
		return fmt.Errorf("ingest: push blob %s to %s: %w", d.Digest, t.name, err)
	}
	cw.finish(sb)
	if _, err := w.Commit(ctx, sb.stored); err != nil {
		return fmt.Errorf("ingest: commit the ciphertext of %s to %s: %w", d.Digest, t.name, err)
	}
	t.sealer.blobs[d.Digest] = sb
	return nil
}

// openSealed returns the plaintext of the encrypted layer of the plaintext digest d, read whole
// from the registry and checked at its end: its HMAC and its digest.
func (t *target) openSealed(ctx context.Context, d digest.Digest) (io.ReadCloser, error) {
	b, err := t.sealer.lookup(ctx, d)
	if err != nil {
		return nil, err
	}
	opts, mac, err := t.sealer.open(ctx, b)
	if err != nil {
		return nil, err
	}
	rc, err := t.rr.Fetch(ctx, b.storedDesc())
	if err != nil {
		return nil, fmt.Errorf("ingest: fetch the ciphertext of %s from %s: %w", d, t.name, err)
	}
	r, err := blobcrypt.Open(rc, opts, mac)
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{r, rc}, nil
}

// seal changes pub's manifest, validated, into the stored one once its layers are encrypted
// (manifest-profile.md §11): the bytes, the digest and the files that the claim, the push and the
// rows take from then on, and the rows of blob_map. The manifest as the client had it keeps its
// digest in sealer.view. Inspect keeps the stored manifest it read. Nothing without a sealer.
func (p *Pipeline) seal(ctx context.Context, pub *publish) error {
	s := pub.sealer
	if s == nil {
		return nil
	}
	if err := pub.sealHeld(ctx, pub.manifest.Layers); err != nil {
		return err
	}
	layers := make(map[digest.Digest]profile.EncryptedLayer, len(pub.manifest.Layers))
	byPlain := make(map[digest.Digest]*sealedBlob, len(pub.manifest.Layers))
	for _, l := range pub.manifest.Layers {
		b, err := s.lookup(ctx, l.Digest)
		if err != nil {
			return err
		}
		if b.size != l.Size {
			return fmt.Errorf("%w: layer %s is %d bytes, its ciphertext %d", ErrInvalid, l.Digest, l.Size, b.size)
		}
		layers[l.Digest] = profile.EncryptedLayer{Stored: b.stored, JWE: b.wrapped, PubOpts: b.pubOpts}
		byPlain[l.Digest] = b
	}
	stored := s.stored
	if stored == nil {
		var err error
		if stored, err = profile.EncryptManifest(pub.raw, layers, ""); err != nil {
			return invalid(fmt.Errorf("ingest: %w", err))
		}
	}
	s.view = pub.digest
	pub.raw, pub.digest = stored, digest.FromBytes(stored)
	files := make([]profile.Layer, len(pub.parsed.Files))
	for i, f := range pub.parsed.Files {
		files[i] = sealedLayer(f, layers[f.Digest])
	}
	s.plain, pub.parsed.Files = pub.parsed.Files, files
	s.rows = s.rows[:0]
	for _, b := range byPlain {
		s.rows = append(s.rows, index.BlobMap{RepoID: s.repo.ID, Path: s.path, Kind: index.BlobMapBlob, Plain: b.plain, Stored: b.stored,
			Size: b.size, KeyFingerprint: b.fingerprint, Wrapped: b.wrapped, PubOpts: b.pubOpts})
	}
	s.rows = append(s.rows, index.BlobMap{RepoID: s.repo.ID, Path: s.path, Kind: index.BlobMapManifest, Plain: s.view,
		Stored: pub.digest, Size: int64(len(stored))})
	return nil
}

// sealedLayer is the file layer f as the stored manifest has it: the ciphertext's digest and the
// two annotations beside its own. Its media type stays the role's, which the frontends read.
func sealedLayer(f profile.Layer, e profile.EncryptedLayer) profile.Layer {
	f.Digest = e.Stored
	ann := make(map[string]string, len(f.Annotations)+2)
	maps.Copy(ann, f.Annotations)
	ann[profile.AnnotationEncJWE], ann[profile.AnnotationEncPubOpts] = e.JWE, e.PubOpts
	f.Annotations = ann
	return f
}

// plainDigest is the plaintext digest of the file f of a version at the package path of the
// repo: an encrypted file's from blob_map, a plain one's its own. An encrypted file whose row
// reindex has not written yet keeps its stored digest, and is then no repeat.
func plainDigest(ctx context.Context, r index.Reader, repoID int64, pkgPath string, f index.File) (digest.Digest, error) {
	if _, ok := f.Attrs[profile.AnnotationEncPubOpts]; !ok {
		return f.Digest, nil
	}
	row, err := r.GetBlobMap(ctx, repoID, pkgPath, f.Digest)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return f.Digest, nil
	case err != nil:
		return "", fmt.Errorf("ingest: blob_map of %s: %w", f.Digest, err)
	}
	return row.Plain, nil
}

// plainFiles are pub's file layers as the client had them: before seal changed them.
func (pub *publish) plainFiles() []profile.Layer {
	if pub.sealer != nil && pub.sealer.plain != nil {
		return pub.sealer.plain
	}
	return pub.parsed.Files
}

// sealedFetched is the fetcher's reader of the encrypted layer of desc, a plaintext descriptor:
// a fetchedBlob of its ciphertext that decrypts.
func (pub *publish) sealedFetched(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	b, err := pub.sealer.lookup(ctx, desc.Digest)
	if err != nil {
		return nil, err
	}
	opts, mac, err := pub.sealer.open(ctx, b)
	if err != nil {
		return nil, err
	}
	return &fetchedBlob{ctx: ctx, t: pub.target, desc: b.storedDesc(), blocks: map[int64][]byte{}, key: &blobKey{opts: opts, mac: mac}}, nil
}

// pushSealedBlobs is pushBlobs for an encrypted repo: every blob of the Build is encrypted into the
// registry but the empty config, which is pushed as it is; a layer of the manifest that is not
// among them must have been uploaded encrypted (a publication session's file, in blob_map).
func (t *target) pushSealedBlobs(ctx context.Context, built *format.Built) error {
	have := map[digest.Digest]bool{}
	for _, b := range built.Blobs {
		have[b.Descriptor.Digest] = true
		if err := t.pushBlob(ctx, b); err != nil {
			return err
		}
	}
	for _, l := range built.Manifest.Layers {
		if have[l.Digest] {
			continue
		}
		b, err := t.sealer.lookup(ctx, l.Digest)
		if err != nil {
			return err
		}
		ok, err := t.rr.Exists(ctx, b.storedDesc())
		if err != nil {
			return fmt.Errorf("ingest: the ciphertext of %s in %s: %w", l.Digest, t.name, err)
		}
		if !ok {
			return fmt.Errorf("%w: the ciphertext of layer %s is not in %s", ErrInvalid, l.Digest, t.name)
		}
	}
	return nil
}

// encryptStreamed puts the cipher between the streamed file of t and its upload session, in an
// encrypted repo: the session gets the ciphertext, the hashes and the inspector the plaintext.
func (t *target) encryptStreamed() error {
	if t.sealer == nil {
		return nil
	}
	s := t.streamed
	s.blob = newBlob("")
	cw, err := newCipherWriter(s.w, s.blob)
	if err != nil {
		return err
	}
	s.cipher = cw
	return nil
}

// sealStreamed records the encryption of the streamed file once its body ended: the plaintext
// digest in its options, the ciphertext's digest and HMAC, the wrapping. The session is committed
// later, with the ciphertext's digest (commitFile).
func (t *target) sealStreamed(ctx context.Context) error {
	s := t.streamed
	if s.blob == nil {
		return nil
	}
	s.blob.plain, s.blob.opts.Digest = s.desc.Digest, s.desc.Digest
	s.cipher.finish(s.blob)
	if err := t.sealer.wrap(ctx, s.blob); err != nil {
		return err
	}
	t.sealer.blobs[s.desc.Digest] = s.blob
	return nil
}

// recordSealed writes the row of blob_map of the streamed file of a publication session, which
// its commit finds by the plaintext digest: the session's file is in the registry from now on, and
// no manifest names it yet.
func (p *Pipeline) recordSealed(ctx context.Context, t *target) error {
	b := t.streamed.blob
	if b == nil {
		return nil
	}
	err := p.index.PutBlobMap(ctx, []index.BlobMap{{RepoID: t.repo.ID, Path: t.pkgPath, Kind: index.BlobMapBlob, Plain: b.plain,
		Stored: b.stored, Size: b.size, KeyFingerprint: b.fingerprint, Wrapped: b.wrapped, PubOpts: b.pubOpts}})
	if err != nil {
		return fmt.Errorf("ingest: blob_map of %s: %w", b.plain, err)
	}
	return nil
}

// readBackOf returns the descriptor that a read-back of t's streamed file fetches, and for an
// encrypted file the key that decrypts it: its ciphertext's.
func (t *target) readBackOf(ctx context.Context) (ocispec.Descriptor, *blobKey, error) {
	s := t.streamed
	if s.blob == nil {
		return s.desc, nil, nil
	}
	opts, mac, err := t.sealer.open(ctx, s.blob)
	if err != nil {
		return ocispec.Descriptor{}, nil, err
	}
	return s.blob.storedDesc(), &blobKey{opts: opts, mac: mac}, nil
}

// sealHeld encrypts the held blobs of a streamed publish that are layers, its metadata layer, in
// memory, before seal: the stored manifest names their ciphertext, which commit pushes after the
// file. The empty config is pushed as it is.
func (t *target) sealHeld(ctx context.Context, layers []ocispec.Descriptor) error {
	s := t.streamed
	if s == nil || t.sealer == nil {
		return nil
	}
	isLayer := map[digest.Digest]bool{}
	for _, l := range layers {
		isLayer[l.Digest] = true
	}
	for i, h := range s.held {
		d := h.Descriptor
		if !isLayer[d.Digest] || t.sealer.known(d.Digest) {
			continue
		}
		rc, err := h.Open(ctx)
		if err != nil {
			return fmt.Errorf("ingest: open blob %s: %w", d.Digest, err)
		}
		plain, err := io.ReadAll(newVerifier(rc, d))
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("%w: blob %s: %w", ErrInvalid, d.Digest, err)
		}
		b := newBlob(d.Digest)
		if err := t.sealer.wrap(ctx, b); err != nil {
			return err
		}
		ct, mac, err := blobcrypt.Seal(*b.opts, plain)
		if err != nil {
			return err
		}
		b.stored, b.size, b.mac, b.pubOpts = digest.FromBytes(ct), int64(len(ct)), mac, blobcrypt.EncodePublic(mac)
		t.sealer.blobs[d.Digest] = b
		t.sealer.ready[b.stored] = true
		s.held[i] = format.BlobSource{Descriptor: b.storedDesc(), Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(ct)), nil
		}}
	}
	return nil
}
