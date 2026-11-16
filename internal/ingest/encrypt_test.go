package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/blobcrypt"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// secret is the plaintext that must never reach the registry.
const secret = "a known string that the registry must never hold: 7f3a9c"

// encrypted turns policy.encryption at-rest on for the env's repo and gives its pipeline the keys
// of encrypted repos over the env's store; it returns them.
func (e *env) encrypted(t *testing.T, o ingest.Options) *keys.Encryption {
	t.Helper()
	m, err := keys.ParseMaster(strings.Repeat("0123456789abcdef", 4))
	must(t, err)
	enc, err := keys.NewEncryption(keys.EncryptionOptions{Index: e.st, Master: m, Log: slog.New(slog.DiscardHandler)})
	must(t, err)
	e.repo.Policy = json.RawMessage(`{"encryption":{"mode":"at-rest"}}`)
	must(t, e.st.UpdateRepo(t.Context(), &e.repo))
	o.Encryption = enc
	e.pipe = ingest.New(o)
	return enc
}

// storedAt returns the manifest the tag of the package path holds in the fake, as its bytes.
func (e *env) storedAt(t *testing.T, repoPath, tag string) ([]byte, ocispec.Manifest) {
	t.Helper()
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, repoPath)
	must(t, err)
	desc, err := rr.Resolve(ctx, tag)
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	raw, err := io.ReadAll(rc)
	must(t, err)
	var m ocispec.Manifest
	must(t, json.Unmarshal(raw, &m))
	return raw, m
}

// checkEncrypted checks the stored manifest of repoPath:tag: every layer encrypted, its blob in the
// registry the ciphertext without plain, which the repo's key opens back to it, the plaintext
// blob absent; the files rows name the ciphertexts, and blob_map maps both digests.
func (e *env) checkEncrypted(t *testing.T, enc *keys.Encryption, repoPath, tag, plain string) {
	t.Helper()
	ctx := t.Context()
	raw, m := e.storedAt(t, repoPath, tag)
	rr, err := e.prov.Repository(ctx, repoPath)
	must(t, err)
	pkgPath := strings.TrimPrefix(repoPath, "raw/"+e.repo.Name+"/")
	found := false
	for _, l := range m.Layers {
		if !blobcrypt.Encrypted(l.MediaType) || l.Annotations[blobcrypt.AnnotationJWE] == "" || l.Annotations[blobcrypt.AnnotationPubOpts] == "" {
			t.Fatalf("a layer that is not encrypted: %+v", l)
		}
		rc, err := rr.Fetch(ctx, l)
		must(t, err)
		ct, err := io.ReadAll(rc)
		_ = rc.Close()
		must(t, err)
		if bytes.Contains(ct, []byte(plain)) {
			t.Fatalf("the registry holds the plaintext in %s", l.Digest)
		}
		row, err := e.st.GetBlobMap(ctx, e.repo.ID, pkgPath, l.Digest)
		must(t, err)
		opts, err := enc.Unwrap(ctx, e.repo, row.KeyFingerprint, row.Wrapped)
		must(t, err)
		mac, err := blobcrypt.DecodePublic(row.PubOpts)
		must(t, err)
		r, err := blobcrypt.Open(bytes.NewReader(ct), opts, mac)
		must(t, err)
		got, err := io.ReadAll(r)
		must(t, err)
		if string(got) == plain {
			found = true
			if ok, _ := rr.Exists(ctx, ocispec.Descriptor{Digest: row.Plain, Size: int64(len(plain))}); ok {
				t.Error("the registry holds the plaintext blob")
			}
		}
	}
	if !found {
		t.Fatalf("no layer decrypts to the file")
	}
	all, err := e.st.ListBlobMap(ctx, e.repo.ID, pkgPath)
	must(t, err)
	manifests := 0
	for _, r := range all {
		if r.Kind == index.BlobMapManifest && r.Stored == digest.FromBytes(raw) {
			manifests++
			view, err := profile.DecryptManifest(raw, func(d digest.Digest) (digest.Digest, bool) {
				row, err := e.st.GetBlobMap(ctx, e.repo.ID, pkgPath, d)
				if err != nil {
					return "", false
				}
				return row.Plain, true
			}, "")
			must(t, err)
			if digest.FromBytes(view) != r.Plain {
				t.Errorf("the manifest's row maps %s, its undone form is %s", r.Plain, digest.FromBytes(view))
			}
		}
	}
	if manifests != 1 {
		t.Errorf("%d rows of the stored manifest in blob_map", manifests)
	}
}

// TestUploadEncrypted: a frontend's upload to an encrypted repo stores ciphertext only, the files
// rows name it, and the same file again is the idempotent repeat although it would be another
// ciphertext.
func TestUploadEncrypted(t *testing.T) {
	e := newEnv(t)
	enc := e.encrypted(t, e.options(e.prov))
	up := upload("dist/app.txt", "1.0.0", file{"dist/app.txt", secret})
	pub, err := e.pipe.Upload(t.Context(), e.repo, up, alice)
	must(t, err)
	e.checkEncrypted(t, enc, pub.RepoPath, pub.Tag, secret)
	r := e.rows(t)
	if len(r.Files) != 1 || len(r.Files[0]) != 1 {
		t.Fatalf("files %+v", r.Files)
	}
	f := r.Files[0][0]
	if f.Digest == digest.FromString(secret) || f.Attrs[blobcrypt.AnnotationPubOpts] == "" || blobcrypt.Encrypted(f.MediaType) {
		t.Errorf("the file row %+v: want the ciphertext's digest, the role's media type, the annotations", f)
	}
	again, err := e.pipe.Upload(t.Context(), e.repo, upload("dist/app.txt", "1.0.0", file{"dist/app.txt", secret}), alice)
	must(t, err)
	if !again.Existed || again.Digest != pub.Digest {
		t.Errorf("the same file again = %+v, want the idempotent repeat of %s", again, pub.Digest)
	}
}

// TestUploadStreamEncrypted: a streamed upload encrypts the file on its way into the session, and
// its metadata layer, and Validate and the rows see the plaintext's checksums.
func TestUploadStreamEncrypted(t *testing.T) {
	e, sp := newStreamEnv(t)
	o := e.options(e.prov)
	o.Plugins = func(name string) (format.Plugin, bool) { return sp, name == "raw" }
	enc := e.encrypted(t, o)
	pub, err := e.pipe.UploadStream(t.Context(), e.repo, streamUpload("1.0.0", secret), alice)
	must(t, err)
	e.checkEncrypted(t, enc, pub.RepoPath, pub.Tag, secret)
	if got := sp.got[0].Checksums["sha256"]; got != digest.FromString(secret).Encoded() {
		t.Errorf("Build got the sha256 %s, want the plaintext's", got)
	}
}

// TestIngestEncrypted: a manifest of /v2 whose layers were uploaded encrypted (the facade wrote
// their rows) is stored changed, under its own digest, and the result says the client's; a layer
// without a row is refused.
func TestIngestEncrypted(t *testing.T) {
	e := newEnv(t)
	enc := e.encrypted(t, e.options(e.prov))
	ctx := t.Context()
	built, err := e.plugin.Build(ctx, e.repo, upload("dist/app.txt", "1.0.0", file{"dist/app.txt", secret}))
	must(t, err)
	raw, _, err := profile.Encode(built.Manifest)
	must(t, err)
	pkgPath := strings.TrimPrefix(built.RepoPath, "raw/scratch/")
	rr, err := e.prov.Repository(ctx, built.RepoPath)
	must(t, err)
	// The facade's upload: each layer encrypted into the registry and its row written.
	for _, b := range built.Blobs {
		rc, err := b.Open(ctx)
		must(t, err)
		plain, err := io.ReadAll(rc)
		_ = rc.Close()
		must(t, err)
		if b.Descriptor.Digest == profile.EmptyConfig.Digest {
			must(t, rr.Push(ctx, b.Descriptor, bytes.NewReader(plain)))
			continue
		}
		opts := blobcrypt.NewPrivate()
		opts.Digest = b.Descriptor.Digest
		ct, mac, err := blobcrypt.Seal(opts, plain)
		must(t, err)
		jwe, fp, err := enc.Wrap(ctx, e.repo, opts)
		must(t, err)
		stored := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(ct), Size: int64(len(ct))}
		must(t, rr.Push(ctx, stored, bytes.NewReader(ct)))
		must(t, e.st.PutBlobMap(ctx, []index.BlobMap{{RepoID: e.repo.ID, Path: pkgPath, Kind: index.BlobMapBlob, Plain: b.Descriptor.Digest,
			Stored: stored.Digest, Size: stored.Size, KeyFingerprint: fp, Wrapped: jwe, PubOpts: blobcrypt.EncodePublic(mac)}}))
	}
	res, err := e.pipe.Ingest(ctx, e.repo, pkgPath, built.Tag, built.Manifest, raw, alice)
	must(t, err)
	if res.View != digest.FromBytes(raw) || res.Digest == res.View {
		t.Errorf("Ingest = %s, view %s; want the stored digest and the client's %s", res.Digest, res.View, digest.FromBytes(raw))
	}
	e.checkEncrypted(t, enc, built.RepoPath, built.Tag, secret)

	other, err := e.plugin.Build(ctx, e.repo, upload("dist/other.txt", "1.0.0", file{"dist/other.txt", "not uploaded encrypted"}))
	must(t, err)
	oraw, _, err := profile.Encode(other.Manifest)
	must(t, err)
	_, err = e.pipe.Ingest(ctx, e.repo, strings.TrimPrefix(other.RepoPath, "raw/scratch/"), other.Tag, other.Manifest, oraw, alice)
	if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("a layer not uploaded encrypted: %v, want ErrInvalid", err)
	}
}

// TestEncryptedNeedsTheKeys: a publish to an encrypted repo on a server without the master key
// fails, and stores nothing.
func TestEncryptedNeedsTheKeys(t *testing.T) {
	e := newEnv(t)
	e.encrypted(t, e.options(e.prov))
	e.pipe = ingest.New(e.options(e.prov)) // no Encryption
	_, err := e.pipe.Upload(context.WithoutCancel(t.Context()), e.repo, upload("dist/app.txt", "1.0.0", file{"dist/app.txt", secret}), alice)
	if err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("Upload without the keys: %v", err)
	}
	if len(e.prov.Pushes()) != 0 {
		t.Errorf("pushed %d blobs", len(e.prov.Pushes()))
	}
}

// TestSessionEncrypted: the files of a publication session go to the registry encrypted as they
// arrive, with their rows in blob_map, and the commit stores the manifest of their ciphertexts.
func TestSessionEncrypted(t *testing.T) {
	e, c := sessionEnv(t)
	o := e.options(e.prov)
	o.Now, o.SessionTimeout = c.Now, time.Minute
	enc := e.encrypted(t, o)
	ctx := t.Context()
	s, err := e.pipe.Open(ctx, e.repo, "lib", "1.0", alice)
	must(t, err)
	f := add(t, e, s.ID, "lib.jar", secret)
	if f.Digest != digest.FromString(secret) {
		t.Errorf("the session file's digest %s, want the plaintext's", f.Digest)
	}
	rows, err := e.st.FindBlobMap(ctx, e.repo.ID, "lib", f.Digest, index.BlobMapBlob)
	must(t, err)
	if len(rows) != 1 {
		t.Fatalf("blob_map rows of the session file: %+v", rows)
	}
	pub, err := e.pipe.Commit(ctx, s.ID, nil)
	must(t, err)
	e.checkEncrypted(t, enc, pub.RepoPath, pub.Tag, secret)
}
