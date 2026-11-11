package ingest

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // the .md5 of apt's and maven's indexes; not a security use
	"crypto/sha1" //nolint:gosec // the .sha1 of apt's and maven's indexes; not a security use
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// UploadStream is the frontend path of a file that streams into the registry while it is read
// (ADR 0014, docs/adr/0014-uploads-stream-into-the-registry.md): nothing of it is stored unless
// the publish goes through. It implements format.Publisher. The steps:
//
//  1. the plugin of repo's format must be a format.Streamer (otherwise ErrUnsupported), up.Name
//     a name it allows (otherwise ErrInvalid); its Inspect says which checksums Build needs
//     besides sha256 and which inspector the bytes pass through;
//  2. the OCI repository of the package is ensured and a provider.BlobWriter opened in it; Body
//     is read once, to its end, into the writer, the hashes and the inspector, which is closed
//     after it. An error of the body is returned as it is, so the 413 of format.LimitBody stays
//     a 413; one of the inspector is Build's (ErrInvalid); a body that ends at another size
//     than File.Size is ErrInvalid;
//  3. Build gets the file without Open, with its size, checksums and inspector; its manifest
//     must be at the package path of up.Name and have a file layer of the streamed digest and
//     size;
//  4. then the steps of Upload: the idempotent repeat, the checks of Ingest, the claims, the tag
//     in the backend. The other blobs, Build's (the metadata layer) and the empty config, are
//     held in memory meanwhile, and the plugin's Validate reads them there. Only then is the
//     session closed with the sha256 that ingest computed (BlobWriter.Commit; a blob that the
//     repository holds already is not sent again), the held blobs pushed, and the manifest;
//  5. any failure before the commit, and the idempotent repeat, cancel the session, with ctx's
//     values but not its cancellation: nothing of the upload is stored. A cancel that fails is
//     logged, and the registry expires the session.
func (p *Pipeline) UploadStream(ctx context.Context, repo index.Repo, up format.StreamUpload, principal auth.Principal) (_ *format.Published, err error) {
	existed := false
	defer func() { count(repo.Format, existed, err) }()
	plugin, prov, err := p.lookup(repo)
	if err != nil {
		return nil, err
	}
	streamer, ok := plugin.(format.Streamer)
	if !ok {
		return nil, fmt.Errorf("ingest: repo %s/%s: the plugin takes no streamed upload: %w", repo.Format, repo.Name, format.ErrUnsupported)
	}
	pkgPath, err := plugin.EscapeName(up.Name)
	if err != nil {
		return nil, invalid(fmt.Errorf("ingest: %s %q: %w", profile.AnnotationName, up.Name, err))
	}
	if pkgPath, err = p.storedPath(ctx, repo, pkgPath); err != nil {
		return nil, err
	}
	in, err := streamer.Inspect(repo, up)
	if err != nil {
		return nil, buildError(repo, err)
	}
	t, err := newTarget(ctx, repo, plugin, prov, pkgPath)
	if err != nil {
		return nil, err
	}
	w, err := t.rr.OpenBlobWriter(ctx)
	if err != nil {
		return nil, fmt.Errorf("ingest: open a blob upload in %s: %w", t.name, err)
	}
	t.streamed = &streamed{w: w, held: []format.BlobSource{emptyConfig}}
	defer p.cancelStreamed(ctx, t)
	file, err := t.streamed.read(repo, up, in)
	if err != nil {
		return nil, fmt.Errorf("ingest: %s into %s: %w", up.File.Name, t.name, err)
	}
	built, err := plugin.Build(ctx, repo, format.Upload{Files: []format.UploadedFile{file}, Params: up.Params, Origin: up.Origin})
	if err != nil {
		return nil, buildError(repo, err)
	}
	builtPath, pub, err := p.prepare(ctx, repo, built, up.Origin, principal)
	switch {
	case err != nil:
		return nil, err
	case builtPath != pkgPath:
		return nil, fmt.Errorf("ingest: the %s plugin built %q, but %s streamed into %s", repo.Format, built.RepoPath, up.File.Name, t.name)
	case !t.streamed.layerOf(built.Manifest):
		return nil, fmt.Errorf("ingest: the %s plugin built a manifest without the layer of %s, %s of %d bytes",
			repo.Format, up.File.Name, t.streamed.desc.Digest, t.streamed.desc.Size)
	case pub != nil:
		existed = true
		return pub, nil
	}
	pub, err = p.store(ctx, t, built, up.Origin, principal)
	existed = pub != nil && pub.Existed
	return pub, err
}

// streamed is the file of UploadStream: its upload session holds the bytes until commit closes
// it, after every check, or cancelStreamed drops it.
type streamed struct {
	w    provider.BlobWriter
	desc ocispec.Descriptor // the sha256 digest and size of the bytes, once the body has ended
	done bool               // committed, or found in the repository already
	// held are the other blobs of the upload, the empty config and Build's, which commit pushes
	// after the file; until then the checks read them here (publish.open).
	held []format.BlobSource
}

// emptyConfig is the config of every manifest of the profile, as a blob to push.
var emptyConfig = format.BlobSource{Descriptor: profile.EmptyConfig, Open: func(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(ocispec.DescriptorEmptyJSON.Data)), nil
}}

// heldBlob returns the held blob of digest d; false without a streamed file or such a blob.
func (s *streamed) heldBlob(d digest.Digest) (format.BlobSource, bool) {
	if s == nil {
		return format.BlobSource{}, false
	}
	for _, b := range s.held {
		if b.Descriptor.Digest == d {
			return b, true
		}
	}
	return format.BlobSource{}, false
}

// read copies the body of up once into the writer, the hashes and the inspector of in, and
// returns the file for Build.
func (s *streamed) read(repo index.Repo, up format.StreamUpload, in format.Inspection) (format.UploadedFile, error) {
	hashes := map[string]hash.Hash{"sha256": sha256.New()}
	for _, name := range in.Checksums {
		h := newHash(name)
		if h == nil {
			return format.UploadedFile{}, fmt.Errorf("the %s plugin asks for the checksum %q, which is not md5, sha1 or sha512", repo.Format, name)
		}
		hashes[name] = h
	}
	sink := &failing{w: s.w}
	ws := []io.Writer{sink}
	for _, h := range hashes {
		ws = append(ws, h)
	}
	var insp *failing
	if in.Inspector != nil {
		insp = &failing{w: in.Inspector}
		ws = append(ws, insp)
	}
	n, err := io.Copy(io.MultiWriter(ws...), up.Body)
	switch {
	case insp != nil && insp.err != nil:
		return format.UploadedFile{}, invalid(fmt.Errorf("the %s plugin's inspector: %w", repo.Format, insp.err))
	case sink.err != nil:
		return format.UploadedFile{}, fmt.Errorf("stream to the registry: %w", sink.err)
	case err != nil:
		return format.UploadedFile{}, fmt.Errorf("read the body: %w", err)
	case up.File.Size >= 0 && n != up.File.Size:
		return format.UploadedFile{}, fmt.Errorf("%w: the body has %d bytes, Content-Length says %d", ErrInvalid, n, up.File.Size)
	}
	if in.Inspector != nil {
		if err := in.Inspector.Close(); err != nil {
			return format.UploadedFile{}, invalid(fmt.Errorf("the %s plugin's inspector: %w", repo.Format, err))
		}
	}
	f := up.File
	f.Size, f.Open, f.Inspector, f.Checksums = n, nil, in.Inspector, make(map[string]string, len(hashes))
	for name, h := range hashes {
		f.Checksums[name] = hex.EncodeToString(h.Sum(nil))
	}
	s.desc = ocispec.Descriptor{MediaType: sessionBlobType, Digest: digest.NewDigestFromEncoded(digest.SHA256, f.Checksums["sha256"]), Size: n}
	return f, nil
}

// newHash returns the hash of a checksum name of format.Inspection, nil for another name.
func newHash(name string) hash.Hash {
	switch name {
	case "md5":
		return md5.New() //nolint:gosec // see the import
	case "sha1":
		return sha1.New() //nolint:gosec // see the import
	case "sha512":
		return sha512.New()
	}
	return nil
}

// failing is a writer that keeps the first error of the writer under it, so that read tells the
// writers of its io.MultiWriter apart from the body.
type failing struct {
	w   io.Writer
	err error
}

func (f *failing) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil && f.err == nil {
		f.err = err
	}
	return n, err
}

// holds reports whether d is the digest of the streamed file, whose blob the repository may not
// have before the commit; false without one.
func (s *streamed) holds(d digest.Digest) bool { return s != nil && s.desc.Digest == d }

// later reports whether d is a blob that the push stores after the checks (commit): the streamed
// file or a held blob; false without a streamed file.
func (s *streamed) later(d digest.Digest) bool {
	_, held := s.heldBlob(d)
	return held || s.holds(d)
}

// layerOf reports whether a layer of m is the streamed file, by digest and size.
func (s *streamed) layerOf(m ocispec.Manifest) bool {
	for _, l := range m.Layers {
		if l.Digest == s.desc.Digest && l.Size == s.desc.Size {
			return true
		}
	}
	return false
}

// commit closes the session of t's streamed file with its digest, the last step before the
// manifest's PUT, unless the repository holds the blob already: the session is then left to
// cancelStreamed. Then it pushes the held blobs that the repository does not have: after the
// file, so that a commit the registry refuses leaves none of them. Nothing to do without a
// streamed file.
func (t *target) commit(ctx context.Context) error {
	s := t.streamed
	if s == nil || s.done {
		return nil
	}
	ok, err := t.rr.Exists(ctx, s.desc)
	if err != nil {
		return fmt.Errorf("ingest: blob %s in %s: %w", s.desc.Digest, t.name, err)
	}
	if !ok {
		if _, err := s.w.Commit(ctx, s.desc.Digest); err != nil {
			return fmt.Errorf("ingest: commit blob %s of %d bytes to %s: %w", s.desc.Digest, s.desc.Size, t.name, err)
		}
	}
	s.done = true
	for _, b := range s.held {
		if err := t.pushBlob(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

// cancelStreamed drops the session of t's streamed file unless commit closed it (Cancel after
// Commit does nothing), also when ctx is done.
func (p *Pipeline) cancelStreamed(ctx context.Context, t *target) {
	if t.streamed == nil {
		return
	}
	err := t.streamed.w.Cancel(context.WithoutCancel(ctx))
	if err != nil && !errors.Is(err, provider.ErrUnsupported) {
		p.log.WarnContext(ctx, "ingest: an upload session was not cancelled; the registry expires it",
			"repo", t.repo.Format+"/"+t.repo.Name, "repository", t.name, "error", err.Error())
	}
}
