package ingest_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// hangingUp is a registry whose client hangs up: once the delete of the manifest after has gone
// through, cancel ends the request's context, and from then on every call made with that
// context fails with its error before it reaches the registry, as an HTTP client's does.
type hangingUp struct {
	provider.Provider
	after  digest.Digest
	cancel context.CancelFunc
}

func (f hangingUp) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Provider.Repository(ctx, name)
	return hungUpRepo{rr, f}, err
}

type hungUpRepo struct {
	provider.Repository
	f hangingUp
}

func (r hungUpRepo) Delete(ctx context.Context, desc ocispec.Descriptor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := r.Repository.Delete(ctx, desc)
	if err == nil && desc.Digest == r.f.after {
		r.f.cancel()
	}
	return err
}

func (r hungUpRepo) Resolve(ctx context.Context, reference string) (ocispec.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return ocispec.Descriptor{}, err
	}
	return r.Repository.Resolve(ctx, reference)
}

func (r hungUpRepo) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.Repository.Fetch(ctx, desc)
}

func (r hungUpRepo) PushReference(ctx context.Context, desc ocispec.Descriptor, content io.Reader, tag string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Repository.PushReference(ctx, desc, content, tag)
}

func (r hungUpRepo) Referrers(ctx context.Context, desc ocispec.Descriptor, artifactType string, fn func([]ocispec.Descriptor) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Repository.Referrers(ctx, desc, artifactType, fn)
}

func (r hungUpRepo) Tags(ctx context.Context, last string, fn func([]string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Repository.Tags(ctx, last, fn)
}

// TestDeleteClientGone: a client that hangs up right after the registry deleted the version's
// manifest leaves no row of it (#710). The delete finishes on a context of its own: the rows
// go, the package's too, the audit row is ok, and the same upload again stores the manifest
// instead of answering an idempotent repeat of a version the registry does not have.
func TestDeleteClientGone(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, rec, subject := referrerEnv(t, api)
		a := attach(t, e, subject, api)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		opts := e.options(hangingUp{registry{Fake: e.prov, api: api, rec: rec}, subject.Digest, cancel})
		opts.Index = rec
		_, err := ingest.New(opts).Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: a.tag, ClientIP: "192.0.2.7"}, alice)
		if ctx.Err() == nil {
			t.Fatalf("api %t: the client did not hang up", api)
		}
		if err != nil {
			t.Errorf("api %t: Delete after the client hung up: %v", api, err)
		}
		wantVersionGone(t, e, a)
		if ev := e.rows(t).Audit[0]; ev.State != index.AuditOK {
			t.Errorf("api %t: the audit row of the delete is %s %q, want ok", api, ev.State, ev.Error)
		}
		built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "the subject"}))
		res, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
		if res.Existed || e.tagged(t, "raw/scratch/notes", built.Tag) != subject.Digest || len(e.rows(t).Versions) != 1 {
			t.Errorf("api %t: the same upload after the delete: %+v, want it stored again", api, res)
		}
	}
}

// TestDeleteReferrerClientGone: without the referrers API, a client that hangs up right after
// the delete of a signature took its subject's tag-schema index leaves the attestation listed
// (#711): the delete finishes on a context of its own, and the index is pushed again without the
// signature alone.
func TestDeleteReferrerClientGone(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	a := attach(t, e, subject, false)
	sig, att := a.refs[1], a.refs[2]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := e.options(hangingUp{registry{Fake: e.prov, rec: rec}, a.index, cancel})
	opts.Index = rec
	_, err := ingest.New(opts).Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: sig.String()}, alice)
	if ctx.Err() == nil {
		t.Fatal("the client did not hang up")
	}
	if err != nil {
		t.Errorf("Delete after the client hung up: %v", err)
	}
	gone(t, e, sig.String())
	same(t, "the referrers the subject's tag-schema index lists", []digest.Digest{att}, listedBy(t, e, "sha256-"+subject.Digest.Encoded()))
}

// listedBy returns the digests that the image index under tag in raw/scratch/notes lists, nil
// when the tag names nothing.
func listedBy(t *testing.T, e *env, tag string) []digest.Digest {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	desc, err := rr.Resolve(t.Context(), tag)
	if err != nil {
		return nil
	}
	rc, err := rr.Fetch(t.Context(), desc)
	must(t, err)
	defer func() { _ = rc.Close() }()
	var idx ocispec.Index
	must(t, json.NewDecoder(rc).Decode(&idx))
	var out []digest.Digest
	for _, m := range idx.Manifests {
		out = append(out, m.Digest)
	}
	return out
}
