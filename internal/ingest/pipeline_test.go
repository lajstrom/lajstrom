package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

const readme = "Docs/Readme.md"

// TestIngest checks the happy path: one PushReference, every column of the index rows, the
// repo's index_cache emptied, one audit event, no claim left; then the same bytes again are
// Existed and write nothing.
func TestIngest(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	cache := index.Cache{RepoID: e.repo.ID, Scope: "simple/x", Kind: "simple", Content: []byte("old"), SHA256: "s"}
	must(t, e.st.PutIndexCache(ctx, &cache))
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	d := digest.FromBytes(raw)

	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	entry, err := validateRaw(ctx, e.repo, built.Manifest, nil)
	must(t, err)
	same(t, "Result", &ingest.Result{Digest: d, Tag: "1.0.0", RepoPath: "raw/scratch/docs/readme.md", Entry: *entry}, res)
	same(t, "PushReferences", []providertest.PushRecord{{Name: "raw/scratch/docs/readme.md", Tag: "1.0.0", Digest: d, Size: int64(len(raw))}},
		e.prov.PushReferences())

	got := e.rows(t)
	if len(got.Packages) != 1 || len(got.Versions) != 1 || len(got.OCIRepos) != 1 || len(got.Audit) != 1 {
		t.Fatalf("rows: %+v", got)
	}
	pkg, v := got.Packages[0], got.Versions[0]
	same(t, "rows", rows{
		Packages: []index.Package{{ID: pkg.ID, RepoID: e.repo.ID, Name: readme, NameOCI: "docs/readme.md", CreatedAt: now}},
		Versions: []index.Version{{ID: v.ID, PackageID: pkg.ID, Version: "1.0.0", Tag: "1.0.0", Digest: d,
			ArtifactType: "application/vnd.lajstrom.raw.v1", Attrs: map[string]string{"lajstrom.raw.path": readme},
			Metadata: json.RawMessage(`{"files":[{"name":"Docs/Readme.md","size":5}]}`), CreatedAt: created, IndexedAt: now}},
		Files: [][]index.File{{{ID: got.Files[0][0].ID, VersionID: v.ID, Name: readme, MediaType: profile.MediaTypeRawFile,
			Digest: digest.FromString("hello"), Size: 5}}},
		OCIRepos: []index.OCIRepo{{ID: got.OCIRepos[0].ID, RepoID: e.repo.ID, Path: "raw/scratch/docs/readme.md", SeenAt: now}},
		Audit: []index.AuditEvent{{ID: got.Audit[0].ID, At: now, Principal: "user:alice", Action: "push", Repo: "raw/scratch",
			Subject: readme + " 1.0.0", Digest: d}},
	}, got)
	if _, err := e.st.GetIndexCache(ctx, e.repo.ID, cache.Scope, cache.Kind); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("index_cache of the repo after ingest: %v, want ErrNotFound", err)
	}
	e.noClaim(t, readme, "1.0.0")

	again, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	want := *res
	want.Existed = true
	same(t, "Result of the same bytes again", &want, again)
	if n := len(e.prov.PushReferences()); n != 1 {
		t.Errorf("%d PushReference calls after the repeat, want 1", n)
	}
	same(t, "rows after the repeat", got, e.rows(t))
	e.noClaim(t, readme, "1.0.0")
}

// TestIngestConflict checks that other bytes under a confirmed tag are ErrConflict with the
// existing digest, without a PushReference.
func TestIngestConflict(t *testing.T) {
	e := newEnv(t)
	first, firstRaw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	_, err := e.ingest(t, first, firstRaw, pkgPath)
	must(t, err)
	other, otherRaw, _ := e.prepare(t, upload(readme, "1.0.0", file{readme, "other bytes"}))
	_, err = e.ingest(t, other, otherRaw, pkgPath)
	wantTagConflict(t, err, digest.FromBytes(firstRaw))
	if n := len(e.prov.PushReferences()); n != 1 {
		t.Errorf("%d PushReference calls, want 1", n)
	}
	e.noClaim(t, readme, "1.0.0")
}

// wantTagConflict fails the test unless err is ErrConflict with a TagConflictError naming
// existing.
func wantTagConflict(t *testing.T, err error, existing digest.Digest) {
	t.Helper()
	var tc *index.TagConflictError
	if !errors.Is(err, ingest.ErrConflict) || !errors.As(err, &tc) || tc.Existing != existing {
		t.Errorf("error = %v, want ErrConflict with a TagConflictError naming %s", err, existing)
	}
	if s := format.StatusOf(err); s != http.StatusConflict {
		t.Errorf("status of %v = %d, want 409", err, s)
	}
}

// ingestArgs are what Ingest gets besides the repo; raw is encoded from m.
type ingestArgs struct {
	pkgPath, tag string
	m            ocispec.Manifest
}

// withAnnotation returns m with the annotation key set to value.
func withAnnotation(m ocispec.Manifest, key, value string) ocispec.Manifest {
	m.Annotations = maps.Clone(m.Annotations)
	m.Annotations[key] = value
	return m
}

// TestIngestInvalid checks every ErrInvalid of Ingest: nothing is pushed, written or left
// claimed.
func TestIngestInvalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, e *env, a *ingestArgs)
		also   error // another error the chain must contain
	}{
		{"profile", func(_ *testing.T, _ *env, a *ingestArgs) {
			a.m.Config = ocispec.Descriptor{MediaType: "application/json", Digest: digest.FromString("{}"), Size: 2}
		}, profile.ErrConfig},
		{"format", func(t *testing.T, _ *env, a *ingestArgs) {
			md := []byte(`{}`)
			m, err := profile.Build(profile.Annotations{Format: "pypi", Repo: "scratch", Name: readme, Version: "1.0.0", Created: created},
				[]profile.Layer{{MediaType: profile.MediaTypePypiWheel, Digest: digest.FromString("w"), Size: 1, Title: "x.whl"}},
				&profile.Layer{Digest: digest.FromBytes(md), Size: int64(len(md))})
			must(t, err)
			a.m = m
		}, nil},
		{"repo", func(_ *testing.T, _ *env, a *ingestArgs) { a.m = withAnnotation(a.m, profile.AnnotationRepo, "other") }, nil},
		{"path", func(_ *testing.T, _ *env, a *ingestArgs) { a.pkgPath = "elsewhere" }, nil},
		{"tag", func(_ *testing.T, _ *env, a *ingestArgs) { a.tag = "2.0.0" }, nil},
		{"plugin", func(_ *testing.T, e *env, _ *ingestArgs) {
			e.plugin.ValidateFunc = func(context.Context, index.Repo, ocispec.Manifest, format.BlobFetcher) (*format.Entry, error) {
				return nil, errors.New("the bytes say version 2.0.0")
			}
		}, nil},
		{"layer not uploaded", func(t *testing.T, e *env, a *ingestArgs) {
			rr, err := e.prov.Repository(t.Context(), "raw/scratch/"+a.pkgPath)
			must(t, err)
			must(t, rr.Delete(t.Context(), a.m.Layers[0]))
		}, nil},
		{"fetch of a blob the manifest does not name", func(_ *testing.T, e *env, _ *ingestArgs) {
			e.plugin.ValidateFunc = func(ctx context.Context, _ index.Repo, _ ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
				rc, err := fetch(ctx, digest.FromString("not in the manifest"))
				if err == nil {
					_ = rc.Close()
				}
				return nil, err
			}
		}, nil},
		{"metadata not JSON", func(_ *testing.T, e *env, _ *ingestArgs) {
			e.plugin.ValidateFunc = func(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
				entry, err := validateRaw(ctx, repo, m, fetch)
				if entry != nil {
					entry.Metadata = json.RawMessage("not json")
				}
				return entry, err
			}
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			built, _, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
			a := ingestArgs{pkgPath: pkgPath, tag: built.Tag, m: built.Manifest}
			tt.mutate(t, e, &a)
			raw, _, err := profile.Encode(a.m)
			must(t, err)
			_, err = e.pipe.Ingest(t.Context(), e.repo, a.pkgPath, a.tag, a.m, raw, alice)
			if !errors.Is(err, ingest.ErrInvalid) || (tt.also != nil && !errors.Is(err, tt.also)) {
				t.Fatalf("Ingest: %v, want ErrInvalid (and %v)", err, tt.also)
			}
			if s := format.StatusOf(err); s != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", s)
			}
			if n := len(e.prov.PushReferences()); n != 0 {
				t.Errorf("%d PushReference calls, want none", n)
			}
			if r := e.rows(t); len(r.Versions) != 0 || len(r.Audit) != 0 {
				t.Errorf("rows after an invalid ingest: %+v", r)
			}
			e.noClaim(t, readme, a.tag)
		})
	}
}

// TestIngestFetcher checks that the plugin reads the blobs of the manifest through the
// fetcher, and that a blob whose bytes are not its descriptor's fails the read.
func TestIngestFetcher(t *testing.T) {
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	var read []byte
	e.plugin.ValidateFunc = func(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
		rc, err := fetch(ctx, m.Layers[0].Digest)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		if read, err = io.ReadAll(rc); err != nil {
			return nil, err
		}
		return validateRaw(ctx, repo, m, fetch)
	}
	_, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if string(read) != "hello" {
		t.Errorf("the plugin read %q through the fetcher, want %q", read, "hello")
	}

	// A layer that claims a size its blob does not have: the push of the blob fails in the
	// Fake, so the descriptor is changed after the blobs are in.
	second, _, _ := e.prepare(t, upload(readme, "2.0.0", file{readme, "hello"}))
	m := second.Manifest
	m.Layers = append([]ocispec.Descriptor(nil), m.Layers...)
	m.Layers[0].Size++
	raw2, _, err := profile.Encode(m)
	must(t, err)
	_, err = e.pipe.Ingest(t.Context(), e.repo, pkgPath, "2.0.0", m, raw2, alice)
	if !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Ingest of a layer with the wrong size: %v, want ErrInvalid", err)
	}
}

// hooked is a provider whose repositories call hooks around the calls of the Fake: before
// every Resolve, and after every successful PushReference. A nil hook is not called.
type hooked struct {
	*providertest.Fake
	beforeResolve func()
	afterPush     func()
}

func (h hooked) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := h.Fake.Repository(ctx, name)
	return hookedRepository{Repository: rr, h: h}, err
}

type hookedRepository struct {
	provider.Repository
	h hooked
}

func (r hookedRepository) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	if r.h.beforeResolve != nil {
		r.h.beforeResolve()
	}
	return r.Repository.Resolve(ctx, ref)
}

func (r hookedRepository) PushReference(ctx context.Context, desc ocispec.Descriptor, rd io.Reader, ref string) error {
	err := r.Repository.PushReference(ctx, desc, rd, ref)
	if err == nil && r.h.afterPush != nil {
		r.h.afterPush()
	}
	return err
}

// gate holds the first caller of pass until a second one arrives, or for at most d; it lets
// two publishes that both got past the claim meet before either pushes.
type gate struct {
	once  sync.Once
	other chan struct{}
}

func newGate() *gate { return &gate{other: make(chan struct{})} }

func (g *gate) pass(d time.Duration) {
	first := false
	g.once.Do(func() { first = true })
	if !first {
		close(g.other) // two callers per gate
		return
	}
	select {
	case <-g.other:
	case <-time.After(d):
	}
}

// TestIngestPushFails checks that a failed PushReference writes no index rows and releases the
// claim, so that a retry succeeds, and that a registry refusing the tag is ErrConflict.
func TestIngestPushFails(t *testing.T) {
	for _, tt := range []struct {
		name     string
		fail     error
		conflict bool
	}{
		{"registry down", errors.New("connection refused"), false},
		{"immutable tag", fmt.Errorf("tag is immutable: %w", provider.ErrConflict), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
			e.prov.FailPushReference = tt.fail
			_, err := e.ingest(t, built, raw, pkgPath)
			switch {
			case !errors.Is(err, tt.fail):
				t.Fatalf("Ingest: %v, want the registry's error", err)
			case errors.Is(err, ingest.ErrConflict) != tt.conflict || errors.Is(err, ingest.ErrInvalid):
				t.Errorf("Ingest: %v, ErrConflict %v and not ErrInvalid", err, tt.conflict)
			}
			if r := e.rows(t); len(r.Packages) != 0 || len(r.Versions) != 0 || len(r.Audit) != 0 {
				t.Errorf("rows after a failed push: %+v", r)
			}
			e.noClaim(t, readme, "1.0.0")

			e.prov.FailPushReference = nil
			res, err := e.ingest(t, built, raw, pkgPath)
			must(t, err)
			if res.Existed || len(e.rows(t).Versions) != 1 || len(e.prov.PushReferences()) != 1 {
				t.Errorf("retry: %+v, rows %+v", res, e.rows(t))
			}
		})
	}
}

// TestIngestIndexFailsAfterPush checks a confirm that fails after the registry accepted the
// manifest: the error is returned, the manifest stays in the registry for reindex, and the
// claim is released.
func TestIngestIndexFailsAfterPush(t *testing.T) {
	t.Run("repo deleted meanwhile", func(t *testing.T) {
		e := newEnv(t)
		built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
		e.pipe = ingest.New(e.options(hooked{Fake: e.prov, afterPush: func() { must(t, e.st.DeleteRepo(context.Background(), e.repo.ID)) }}))
		_, err := e.ingest(t, built, raw, pkgPath)
		if err == nil || format.StatusOf(err) != http.StatusInternalServerError {
			t.Fatalf("Ingest: %v (status %d), want a 500", err, format.StatusOf(err))
		}
		if got := e.tagged(t, built.RepoPath, "1.0.0"); got != digest.FromBytes(raw) {
			t.Errorf("the registry has %q under the tag, want the manifest %s", got, digest.FromBytes(raw))
		}
		e.noClaim(t, readme, "1.0.0")
	})
	t.Run("version written meanwhile", func(t *testing.T) {
		e := newEnv(t)
		built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
		other := digest.FromString("a reindex found another manifest")
		e.pipe = ingest.New(e.options(hooked{Fake: e.prov, afterPush: func() {
			ctx := context.Background()
			p := index.Package{RepoID: e.repo.ID, Name: readme, NameOCI: pkgPath}
			must(t, e.st.UpsertPackage(ctx, &p))
			must(t, e.st.UpsertVersion(ctx, &index.Version{PackageID: p.ID, Version: "1.0.0", Tag: "1.0.0", Digest: other,
				ArtifactType: "application/vnd.lajstrom.raw.v1"}))
		}}))
		_, err := e.ingest(t, built, raw, pkgPath)
		wantTagConflict(t, err, other)
		if got := e.tagged(t, built.RepoPath, "1.0.0"); got != digest.FromBytes(raw) {
			t.Errorf("the registry has %q under the tag, want the manifest %s", got, digest.FromBytes(raw))
		}
		e.noClaim(t, readme, "1.0.0")
	})
}

// TestIngestTagInBackend checks the tag in the backend: another digest there is ErrConflict
// even when the index does not know the tag, the same digest is indexed, also when a backend
// with immutable tags refuses to push it again.
func TestIngestTagInBackend(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	rr, err := e.prov.Repository(ctx, built.RepoPath)
	must(t, err)
	d := digest.FromBytes(raw)
	must(t, rr.PushReference(ctx, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: d, Size: int64(len(raw))},
		bytes.NewReader(raw), "1.0.0")) // a push that bypassed Lajstrom

	other, otherRaw, _ := e.prepare(t, upload(readme, "1.0.0", file{readme, "other bytes"}))
	_, err = e.ingest(t, other, otherRaw, pkgPath)
	wantTagConflict(t, err, d)
	if n := len(e.prov.PushReferences()); n != 1 {
		t.Errorf("%d PushReference calls, want only the bypassing one", n)
	}
	e.noClaim(t, readme, "1.0.0")

	e.prov.FailPushReference = fmt.Errorf("tag 1.0.0 is immutable: %w", provider.ErrConflict)
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if res.Existed || len(e.rows(t).Versions) != 1 {
		t.Errorf("ingest of the manifest the backend has: %+v, rows %+v", res, e.rows(t))
	}
	e.noClaim(t, readme, "1.0.0")
}

// TestIngestConcurrent runs two Ingest calls for one tag at once. With different digests
// exactly one succeeds, the other gets ErrConflict naming the winner's digest, and the
// registry sees one PushReference for the tag; with the same bytes both succeed, one of them
// Existed.
func TestIngestConcurrent(t *testing.T) {
	const rounds = 10
	e := newEnv(t)
	// Without the claim both calls would find the tag free in the index and in the backend:
	// the gate makes them resolve the tag together, before either pushes.
	var current atomic.Pointer[gate]
	e.pipe = ingest.New(e.options(hooked{Fake: e.prov, beforeResolve: func() { current.Load().pass(100 * time.Millisecond) }}))
	for i := range rounds {
		tag := "1.0." + strconv.Itoa(i)
		a, aRaw, pkgPath := e.prepare(t, upload(readme, tag, file{readme, "a" + tag}))
		b, bRaw, _ := e.prepare(t, upload(readme, tag, file{readme, "b" + tag}))
		current.Store(newGate())
		errs := race(func() error { _, err := e.ingest(t, a, aRaw, pkgPath); return err },
			func() error { _, err := e.ingest(t, b, bRaw, pkgPath); return err })
		digests := []digest.Digest{digest.FromBytes(aRaw), digest.FromBytes(bRaw)}
		switch {
		case errs[0] == nil && errs[1] != nil:
			wantTagConflict(t, errs[1], digests[0])
		case errs[1] == nil && errs[0] != nil:
			wantTagConflict(t, errs[0], digests[1])
		default:
			t.Errorf("tag %s: errors %v, want exactly one nil", tag, errs)
		}
		if refs := e.pushRefs(tag); len(refs) != 1 {
			t.Errorf("tag %s: PushReference calls %v, want exactly one", tag, refs)
		}
		e.noClaim(t, readme, tag)
	}

	for i := range rounds {
		tag := "2.0." + strconv.Itoa(i)
		built, raw, pkgPath := e.prepare(t, upload(readme, tag, file{readme, tag}))
		current.Store(newGate())
		results := make([]*ingest.Result, 2)
		errs := race(func() (err error) { results[0], err = e.ingest(t, built, raw, pkgPath); return err },
			func() (err error) { results[1], err = e.ingest(t, built, raw, pkgPath); return err })
		if errs[0] != nil || errs[1] != nil || results[0].Existed == results[1].Existed {
			t.Errorf("tag %s: %v, %v; want both nil, exactly one Existed (%+v, %+v)", tag, errs[0], errs[1], results[0], results[1])
		}
		e.noClaim(t, readme, tag)
	}
	if n := len(e.rows(t).Audit); n != 2*rounds {
		t.Errorf("%d audit events, want one per tag (%d)", n, 2*rounds)
	}
}

// race runs fns at once and returns their errors.
func race(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Go(func() {
			<-start
			errs[i] = fn()
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// TestIngestStaleClaim checks ingest.claimTTL: a claim of another digest older than ten
// minutes is taken over, a younger one is ErrConflict.
func TestIngestStaleClaim(t *testing.T) {
	for _, tt := range []struct {
		age   time.Duration
		stale bool
	}{{11 * time.Minute, true}, {time.Minute, false}} {
		t.Run(tt.age.String(), func(t *testing.T) {
			e := newEnv(t)
			built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
			other := digest.FromString("a publish that died")
			c := index.TagClaim{RepoID: e.repo.ID, Package: readme, Tag: "1.0.0", Digest: other, Principal: "user:bob", ClaimedAt: now.Add(-tt.age)}
			must(t, e.st.ClaimTag(t.Context(), &c, now.Add(-time.Hour)))
			_, err := e.ingest(t, built, raw, pkgPath)
			if tt.stale {
				must(t, err)
				e.noClaim(t, readme, "1.0.0")
				return
			}
			wantTagConflict(t, err, other)
			if n := len(e.prov.PushReferences()); n != 0 {
				t.Errorf("%d PushReference calls under a fresh claim, want none", n)
			}
		})
	}
}

// TestUpload checks the frontend path: the blobs the registry lacks are pushed, the manifest is
// profile.Encode of what the plugin built, and it is indexed.
func TestUpload(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	up := upload("tool", "1.0.0", file{"tool-linux", "linux binary"}, file{"tool-darwin", "darwin binary"})
	want, err := e.plugin.Build(ctx, e.repo, up)
	must(t, err)
	wantRaw, _, err := profile.Encode(want.Manifest)
	must(t, err)
	rr, err := e.prov.Repository(ctx, "raw/scratch/tool")
	must(t, err)
	must(t, rr.Push(ctx, want.Manifest.Layers[0], bytes.NewReader([]byte("linux binary")))) // already there
	before := len(e.prov.Pushes())

	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	entry, err := validateRaw(ctx, e.repo, want.Manifest, nil)
	must(t, err)
	same(t, "Published", &format.Published{Digest: digest.FromBytes(wantRaw), Tag: "1.0.0", RepoPath: "raw/scratch/tool", Entry: *entry}, pub)
	var pushed []digest.Digest
	for _, p := range e.prov.Pushes()[before:] {
		pushed = append(pushed, p.Digest)
	}
	same(t, "blobs pushed", []digest.Digest{profile.EmptyConfig.Digest, want.Manifest.Layers[1].Digest, want.Manifest.Layers[2].Digest}, pushed)
	desc, err := rr.Resolve(ctx, "1.0.0")
	must(t, err)
	rc, err := rr.Fetch(ctx, desc)
	must(t, err)
	stored, err := io.ReadAll(rc)
	must(t, err)
	if string(stored) != string(wantRaw) {
		t.Errorf("manifest in the registry:\n%s\nwant profile.Encode of the build:\n%s", stored, wantRaw)
	}
	r := e.rows(t)
	if len(r.Versions) != 1 || r.Versions[0].Digest != pub.Digest || len(r.Files[0]) != 2 ||
		r.Files[0][0].Name != "tool-linux" || r.Files[0][1].Name != "tool-darwin" {
		t.Errorf("rows: %+v", r)
	}

	if _, err := e.pipe.Upload(ctx, e.repo, up, alice); err != nil {
		t.Errorf("Upload of the same files again: %v", err)
	}
	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool-linux", "rebuilt"}), alice)
	wantTagConflict(t, err, pub.Digest)
	if n := len(e.rows(t).Audit); n != 1 {
		t.Errorf("%d audit events, want 1", n)
	}

}

// TestUploadRepeat checks the idempotent repeat of an upload: a build of the same files an
// hour later differs by created only and writes nothing, the registry keeps the first
// manifest; other files, and the same manifest through Ingest, are the conflict.
func TestUploadRepeat(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	up := upload("tool", "1.0.0", file{"tool-linux", "linux binary"}, file{"tool-darwin", "darwin binary"})
	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)

	// A build of the same files an hour later differs by created only: the upload is the
	// idempotent repeat, the registry is not touched, the manifest there stays the first.
	e.plugin.BuildFunc = buildRawAt(created.Add(time.Hour))
	pushes, refs := len(e.prov.Pushes()), len(e.pushRefs("1.0.0"))
	again, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	if again.Digest != pub.Digest || again.Tag != pub.Tag || again.RepoPath != pub.RepoPath ||
		again.Entry.Name != pub.Entry.Name || again.Entry.Version != pub.Entry.Version || len(again.Entry.Files) != 2 {
		t.Errorf("repeat: %+v, want the first publish %+v", again, pub)
	}
	if n := len(e.prov.Pushes()); n != pushes {
		t.Errorf("the repeat pushed %d blobs", n-pushes)
	}
	if n := len(e.pushRefs("1.0.0")); n != refs {
		t.Errorf("the repeat pushed %d manifests", n-refs)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != pub.Digest || len(r.Audit) != 1 {
		t.Errorf("rows after the repeat: %+v", r)
	}
	// Same files, other order: still the repeat. Other files: still the conflict.
	if again, err := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool-darwin", "darwin binary"}, file{"tool-linux", "linux binary"}), alice); err != nil || again.Digest != pub.Digest {
		t.Errorf("repeat in another order: %+v, %v", again, err)
	}
	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool-linux", "linux binary"}), alice)
	wantTagConflict(t, err, pub.Digest)

	// The facade path keeps the OCI meaning: another manifest under the tag is the conflict,
	// the same layers or not.
	later, err := e.plugin.Build(ctx, e.repo, up)
	must(t, err)
	rawLater, _, err := profile.Encode(later.Manifest)
	must(t, err)
	_, err = e.pipe.Ingest(ctx, e.repo, "tool", "1.0.0", later.Manifest, rawLater, alice)
	wantTagConflict(t, err, pub.Digest)
	e.noClaim(t, "tool", "1.0.0")
}

// TestUploadRepeatRace checks the repeat rule under the claim: the version appears after the
// check before the blobs (here: while the plugin validates), and the claim transaction finds
// it with the same files, so the second publish is the repeat of the first and the registry
// holds one manifest.
func TestUploadRepeatRace(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	up := upload("tool", "1.0.0", file{"tool-linux", "linux binary"})
	o := e.options(e.prov)
	o.Plugins = func(name string) (format.Plugin, bool) { return newRawPlugin(), name == "raw" }
	other := ingest.New(o)
	var once sync.Once
	e.plugin.BuildFunc = buildRawAt(created.Add(time.Hour))
	e.plugin.ValidateFunc = func(ctx context.Context, repo index.Repo, m ocispec.Manifest, fetch format.BlobFetcher) (*format.Entry, error) {
		once.Do(func() {
			if _, err := other.Upload(ctx, e.repo, up, alice); err != nil {
				t.Errorf("the other replica's Upload: %v", err)
			}
		})
		return validateRaw(ctx, repo, m, fetch)
	}

	pub, err := e.pipe.Upload(ctx, e.repo, up, alice)
	must(t, err)
	first := e.tagged(t, "raw/scratch/tool", "1.0.0")
	if pub.Digest != first {
		t.Errorf("Published.Digest = %s, want the manifest the other replica stored, %s", pub.Digest, first)
	}
	if n := len(e.pushRefs("1.0.0")); n != 1 {
		t.Errorf("%d manifests pushed, want 1", n)
	}
	if r := e.rows(t); len(r.Versions) != 1 || r.Versions[0].Digest != first || len(r.Audit) != 1 {
		t.Errorf("rows: %+v", r)
	}
	e.noClaim(t, "tool", "1.0.0")
}

// TestUploadErrors checks that a Build error is ErrInvalid unless the plugin does not build
// (501), and that a blob whose bytes are not its descriptor's is ErrInvalid.
func TestUploadErrors(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	e.plugin.BuildFunc = func(context.Context, index.Repo, format.Upload) (*format.Built, error) {
		return nil, errors.New("no name parameter")
	}
	if _, err := e.pipe.Upload(ctx, e.repo, format.Upload{}, alice); !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Upload with a failing Build: %v, want ErrInvalid", err)
	}
	e.plugin.BuildFunc = nil
	if _, err := e.pipe.Upload(ctx, e.repo, format.Upload{}, alice); format.StatusOf(err) != http.StatusNotImplemented {
		t.Errorf("Upload to a plugin without Build: %v, want 501", err)
	}

	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		built, err := buildRaw(ctx, repo, up)
		if err == nil {
			built.Blobs[1].Open = func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte("changed"))), nil
			}
		}
		return built, err
	}
	_, err := e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "spooled"}), alice)
	if !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Upload of a file that changed after Build: %v, want ErrInvalid", err)
	}

	e.plugin.BuildFunc = func(ctx context.Context, repo index.Repo, up format.Upload) (*format.Built, error) {
		built, err := buildRaw(ctx, repo, up)
		if err == nil {
			built.Blobs = slices.Delete(built.Blobs, 1, 2) // the file layer
		}
		return built, err
	}
	_, err = e.pipe.Upload(ctx, e.repo, upload("tool", "1.0.0", file{"tool", "never uploaded"}), alice)
	if !errors.Is(err, ingest.ErrInvalid) {
		t.Errorf("Upload of a manifest whose layer is neither built nor in the registry: %v, want ErrInvalid", err)
	}
	if n := len(e.prov.PushReferences()); n != 0 {
		t.Errorf("%d PushReference calls, want none", n)
	}
}

// auditLog is an audit.Writer that keeps the events.
type auditLog struct {
	mu     sync.Mutex
	events []index.AuditEvent
}

func (a *auditLog) Write(_ context.Context, ev index.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

// TestIngestAuditWriter checks that with an audit.Writer the event goes to it, once, and not
// to the index.
func TestIngestAuditWriter(t *testing.T) {
	e := newEnv(t)
	log := &auditLog{}
	o := e.options(e.prov)
	o.Audit = log
	e.pipe = ingest.New(o)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	for range 2 {
		_, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
	}
	same(t, "events written", []index.AuditEvent{{At: now, Principal: "user:alice", Action: "push", Repo: "raw/scratch",
		Subject: readme + " 1.0.0", Digest: digest.FromBytes(raw)}}, log.events)
	if n := len(e.rows(t).Audit); n != 0 {
		t.Errorf("%d audit rows in the index, want none", n)
	}
}

// TestIngestLookups checks a repo whose format has no plugin (501) and one whose registry is
// not configured (500).
func TestIngestLookups(t *testing.T) {
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	for _, tt := range []struct {
		repo   index.Repo
		status int
	}{
		{index.Repo{ID: e.repo.ID, Format: "pypi", Name: "scratch", Registry: "local"}, http.StatusNotImplemented},
		{index.Repo{ID: e.repo.ID, Format: "raw", Name: "scratch", Registry: "elsewhere"}, http.StatusInternalServerError},
	} {
		_, err := e.pipe.Ingest(t.Context(), tt.repo, pkgPath, built.Tag, built.Manifest, raw, alice)
		if err == nil || format.StatusOf(err) != tt.status {
			t.Errorf("Ingest to %s/%s on %s: %v, want status %d", tt.repo.Format, tt.repo.Name, tt.repo.Registry, err, tt.status)
		}
		if _, err := e.pipe.Upload(t.Context(), tt.repo, format.Upload{}, alice); err == nil || format.StatusOf(err) != tt.status {
			t.Errorf("Upload to %s/%s on %s: %v, want status %d", tt.repo.Format, tt.repo.Name, tt.repo.Registry, err, tt.status)
		}
	}
}

// TestIngestPushesTheEmptyConfig checks that ingest pushes the empty config, which is
// Lajstrom's own, when the client did not.
func TestIngestPushesTheEmptyConfig(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
	rr, err := e.prov.Repository(ctx, built.RepoPath)
	must(t, err)
	must(t, rr.Delete(ctx, profile.EmptyConfig))
	_, err = e.ingest(t, built, raw, pkgPath)
	must(t, err)
	if ok, err := rr.Exists(ctx, profile.EmptyConfig); !ok || err != nil {
		t.Errorf("empty config after ingest: %v, %v", ok, err)
	}
}

// TestUploadDoesNotHoldTheFile uploads a 512 MiB file from a generator: Build reads it once to
// digest it and the push streams it once more, and nothing on the way allocates it.
func TestUploadDoesNotHoldTheFile(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 1 GiB")
	}
	const size = 512 << 20
	ctx := t.Context()
	e := newEnv(t)
	var read atomic.Int64
	var midway uint64
	f := format.UploadedFile{Name: "big.bin", Size: size, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(&counter{r: io.LimitReader(zeros{}, size), n: &read, at: size + size/2, fn: func() {
			midway = heapInuse()
		}}), nil
	}}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	pub, err := e.pipe.Upload(ctx, e.repo, format.Upload{Files: []format.UploadedFile{f}, Params: map[string]string{"name": "big.bin", "version": "1.0.0"}}, alice)
	must(t, err)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if n := read.Load(); n != 2*size {
		t.Errorf("%d bytes read from the file, want twice its size: Build digests it, the push streams it", n)
	}
	const limit = 64 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > limit {
		t.Errorf("the upload allocated %d MiB, want less than %d MiB", allocated>>20, limit>>20)
	}
	if midway == 0 || midway > limit {
		t.Errorf("heap in use halfway through the push: %d MiB, want less than %d MiB", midway>>20, limit>>20)
	}
	runtime.GC()
	if h := heapInuse(); h > limit {
		t.Errorf("heap in use after the upload: %d MiB, want less than %d MiB", h>>20, limit>>20)
	}
	if len(pub.Entry.Files) != 1 || pub.Entry.Files[0].Size != size {
		t.Errorf("Published entry: %+v", pub.Entry)
	}
}

// heapInuse returns runtime.MemStats.HeapInuse.
func heapInuse() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// counter counts the bytes read from r into n and calls fn once, when n passes at.
type counter struct {
	r    io.Reader
	n    *atomic.Int64
	at   int64
	fn   func()
	once sync.Once
}

func (c *counter) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	if c.n.Add(int64(k)) >= c.at {
		c.once.Do(c.fn)
	}
	return k, err
}

// zeros reads zero bytes forever.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
