package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

const bundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// lockRecorder is the store with WithReferrersLock recorded: the keys locked, and for every
// manifest pushed whether it was pushed under a lock.
type lockRecorder struct {
	index.Store
	mu     sync.Mutex
	held   bool
	locks  []string
	pushes []bool
}

func (s *lockRecorder) WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error {
	return s.Store.WithReferrersLock(ctx, name, subject, func(ctx context.Context) error {
		s.mu.Lock()
		s.held, s.locks = true, append(s.locks, name+"@"+subject.String())
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.held = false; s.mu.Unlock() }()
		return fn(ctx)
	})
}

// lockLost is the store whose next lost referrers locks end after their function returned nil
// with the error of a lock whose transaction the database ended meanwhile (#615).
type lockLost struct {
	index.Store
	lost int
}

func (s *lockLost) WithReferrersLock(ctx context.Context, name string, subject digest.Digest, fn func(context.Context) error) error {
	err := s.Store.WithReferrersLock(ctx, name, subject, fn)
	if err == nil && s.lost > 0 {
		s.lost--
		return fmt.Errorf("%w: the referrers lock of %s@%s was lost before its function returned", index.ErrUnavailable, name, subject)
	}
	return err
}

// TestPutReferrerLockLost: a referrer pushed under a lock that was lost (#615) is
// index.ErrUnavailable, a 503, with its audit row failed; the same push again stores it.
func TestPutReferrerLockLost(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	opts := e.options(registry{Fake: e.prov, rec: rec})
	opts.Index = &lockLost{Store: rec, lost: 1}
	e.pipe = ingest.New(opts)
	r := signature(t, subject, "signature")
	if _, err := e.pipe.PutReferrer(t.Context(), e.repo, r, alice); !errors.Is(err, index.ErrUnavailable) {
		t.Fatalf("PutReferrer under a lost lock: %v, want index.ErrUnavailable", err)
	}
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditFailed || !strings.Contains(ev.Error, "was lost") {
		t.Errorf("the audit row of the push under a lost lock: %s %q", ev.State, ev.Error)
	}
	d, err := e.pipe.PutReferrer(t.Context(), e.repo, r, alice)
	must(t, err)
	if ev := e.rows(t).Audit[0]; ev.State != index.AuditOK || ev.Digest != d {
		t.Errorf("the audit row of the retry: %s %s", ev.State, ev.Digest)
	}
	same(t, "locks", []string{"raw/scratch/notes@" + subject.Digest.String(), "raw/scratch/notes@" + subject.Digest.String()}, rec.locks)
}

// registry is the fake with the referrers API or without, whose repositories tell rec of each
// manifest push.
type registry struct {
	*providertest.Fake
	api bool
	rec *lockRecorder
}

func (f registry) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := f.Fake.Capabilities(ctx)
	c.Referrers = f.api
	return c, err
}

func (f registry) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return recorded{rr, f.rec}, err
}

type recorded struct {
	provider.Repository
	rec *lockRecorder
}

func (r recorded) Push(ctx context.Context, d ocispec.Descriptor, c io.Reader) error {
	r.note(d)
	return r.Repository.Push(ctx, d, c)
}

func (r recorded) PushReference(ctx context.Context, d ocispec.Descriptor, c io.Reader, tag string) error {
	r.note(d)
	return r.Repository.PushReference(ctx, d, c, tag)
}

func (r recorded) note(d ocispec.Descriptor) {
	if d.MediaType == ocispec.MediaTypeImageManifest {
		r.rec.mu.Lock()
		r.rec.pushes = append(r.rec.pushes, r.rec.held)
		r.rec.mu.Unlock()
	}
}

// referrerEnv is the env with the pipeline over the recorder and the registry, and a file
// published to raw/scratch/notes, the subject; the blobs of a signature are in the backend.
func referrerEnv(t *testing.T, api bool) (*env, *lockRecorder, ocispec.Descriptor) {
	t.Helper()
	e := newEnv(t)
	rec := &lockRecorder{Store: e.st}
	opts := e.options(registry{Fake: e.prov, api: api, rec: rec})
	opts.Index = rec
	e.pipe = ingest.New(opts)
	built, raw, pkgPath := e.prepare(t, upload("notes", "1.0", file{"notes", "the subject"}))
	res, err := e.ingest(t, built, raw, pkgPath)
	must(t, err)
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	sig := []byte("signature")
	must(t, rr.Push(t.Context(), ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(sig), Size: int64(len(sig))}, bytes.NewReader(sig)))
	rec.pushes = nil
	return e, rec, ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: res.Digest, Size: int64(len(raw))}
}

// signature is a referrer of subject: cosign v3's sigstore bundle, with layer layer.
func signature(t *testing.T, subject ocispec.Descriptor, layer string) ingest.Referrer {
	t.Helper()
	m := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		ArtifactType: bundleType, Config: ocispec.DescriptorEmptyJSON, Subject: &subject,
		Layers:      []ocispec.Descriptor{{MediaType: bundleType, Digest: digest.FromString(layer), Size: int64(len(layer))}},
		Annotations: map[string]string{"dev.sigstore.bundle.predicateType": "https://sigstore.dev/cosign/sign/v1"}}
	b, err := json.Marshal(m)
	must(t, err)
	return ingest.Referrer{Path: "notes", Manifest: b, Subject: subject.Digest,
		Desc: ocispec.Descriptor{MediaType: m.MediaType, Digest: digest.FromBytes(b), Size: int64(len(b))}}
}

// TestPutReferrer: a referrer is pushed, written to no derived table, and audited once with
// its kind and artifact type; under the referrers lock of its subject, its subject's tag lock,
// with the referrers API or without (#806).
func TestPutReferrer(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, rec, subject := referrerEnv(t, api)
		before := e.rows(t)
		r := signature(t, subject, "signature")
		d, err := e.pipe.PutReferrer(t.Context(), e.repo, r, alice)
		must(t, err)
		same(t, "digest", r.Desc.Digest, d)
		rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
		must(t, err)
		if _, err := rr.Resolve(t.Context(), d.String()); err != nil {
			t.Errorf("api %t: the referrer is not in the backend: %v", api, err)
		}
		after := e.rows(t)
		if len(after.Audit) != len(before.Audit)+1 {
			t.Fatalf("api %t: %d audit events after %d", api, len(after.Audit), len(before.Audit))
		}
		ev := after.Audit[0]
		before.Audit, after.Audit = nil, nil
		same(t, "derived rows", before, after)
		same(t, "locks", []string{"raw/scratch/notes@" + subject.Digest.String()}, rec.locks)
		same(t, "manifest pushes under the lock", []bool{true}, rec.pushes)
		if ev.Action != "push" || ev.Subject != subject.Digest.String() || ev.Digest != d || ev.Principal != alice.String() || ev.Repo != "raw/scratch" {
			t.Errorf("api %t: audit event %+v", api, ev)
		}
		var details map[string]string
		must(t, json.Unmarshal(ev.Details, &details))
		same(t, "audit details", map[string]string{"kind": "signature", "artifactType": bundleType}, details)
	}
}

// TestPutReferrerRefused: the checks of a referrer of a managed repo, each ErrInvalid naming
// what failed; nothing is pushed, locked or audited.
func TestPutReferrerRefused(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	other := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("no such manifest"), Size: 2}
	ok := signature(t, subject, "signature")
	tagged := ok
	tagged.Tag = "v2"
	legacy := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	otherTag := ingest.Referrer{Path: "notes", Tag: "sha256-" + other.Digest.Encoded() + ".sig", Manifest: legacy, Subject: subject.Digest,
		Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(legacy), Size: int64(len(legacy))}}
	mismatch := ok
	mismatch.Subject = other.Digest
	badBytes := ok
	badBytes.Desc.Digest = digest.FromString("other bytes")
	for _, tt := range []struct {
		name, want string
		r          ingest.Referrer
	}{
		{"a subject the repository does not have", other.Digest.String(), signature(t, other, "signature")},
		{"a layer not uploaded", digest.FromString("never uploaded").String(), signature(t, subject, "never uploaded")},
		{"a tag that is not cosign's", `"v2"`, tagged},
		{"cosign's tag of another subject", "sha256-" + other.Digest.Encoded() + ".sig", otherTag},
		{"a subject other than the manifest's", other.Digest.String(), mismatch},
		{"bytes of another digest", badBytes.Desc.Digest.String(), badBytes},
	} {
		_, err := e.pipe.PutReferrer(t.Context(), e.repo, tt.r, alice)
		if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want ErrInvalid naming %s", tt.name, err, tt.want)
		}
	}
	same(t, "locks", []string(nil), rec.locks)
	same(t, "manifest pushes", []bool(nil), rec.pushes)
	if audit := e.rows(t).Audit; len(audit) != 1 { // the subject's publish
		t.Errorf("audit events %+v", audit)
	}
}

// TestPutReferrerOCI: an oci repo takes a referrer as it comes, a subject it does not have
// included (the conformance suite pushes one). By tag it is pushed under the lock of its
// subject, the tag lock, with the referrers API or without; by digest only without the API.
func TestPutReferrerOCI(t *testing.T) {
	for _, api := range []bool{false, true} {
		e, rec, _ := referrerEnv(t, api)
		repo := ociRepo(t, e)
		nowhere := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("nowhere"), Size: 2}
		r := signature(t, nowhere, "a layer the backend checks, not ingest")
		r.Path, r.Tag = "app", "anything"
		_, err := e.pipe.PutReferrer(t.Context(), repo, r, alice)
		must(t, err)
		key := "oci/images/app@" + nowhere.Digest.String()
		same(t, "locks", []string{key}, rec.locks)
		r.Tag = ""
		_, err = e.pipe.PutReferrer(t.Context(), repo, r, alice)
		must(t, err)
		wantLocks := []string{key, key}
		if api {
			wantLocks = wantLocks[:1]
		}
		same(t, "locks after a push by digest", wantLocks, rec.locks)
	}
}

// annotated is the referrer r with the annotation k: v added, its bytes and digest made again.
func annotated(t *testing.T, r ingest.Referrer, k, v string) ingest.Referrer {
	t.Helper()
	var m ocispec.Manifest
	must(t, json.Unmarshal(r.Manifest, &m))
	m.Annotations[k] = v
	b, err := json.Marshal(m)
	must(t, err)
	r.Manifest = b
	r.Desc.Digest, r.Desc.Size = digest.FromBytes(b), int64(len(b))
	return r
}

// TestPutReferrerOrigin (#762): a referrer of a managed repo, hosted or proxy, with a
// lajstrom.origin.* key is ErrInvalid naming the key, as a version's manifest is, and nothing
// is pushed, locked or audited: only the proxy fetcher writes those keys, and it writes no
// referrer. An oci repo takes one as it comes (manifest-profile.md §3.1).
func TestPutReferrerOrigin(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	proxy := e.repo
	proxy.Kind = index.KindProxy
	for _, key := range []string{profile.AnnotationOriginURL, profile.AnnotationOriginChecksum, profile.AnnotationOriginPrefix + "anything"} {
		r := annotated(t, signature(t, subject, "signature"), key, "https://elsewhere.example/notes")
		for _, repo := range []index.Repo{e.repo, proxy} {
			if _, err := e.pipe.PutReferrer(t.Context(), repo, r, alice); !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), key) {
				t.Errorf("%s repo, %s: %v, want ErrInvalid naming the key", repo.Kind, key, err)
			}
		}
	}
	same(t, "locks", []string(nil), rec.locks)
	same(t, "manifest pushes", []bool(nil), rec.pushes)
	if audit := e.rows(t).Audit; len(audit) != 1 { // the subject's publish
		t.Errorf("audit events %+v", audit)
	}

	r := annotated(t, signature(t, subject, "a layer the backend checks, not ingest"), profile.AnnotationOriginURL, "https://elsewhere.example/app")
	r.Path = "app"
	if _, err := e.pipe.PutReferrer(t.Context(), ociRepo(t, e), r, alice); err != nil {
		t.Errorf("a referrer with %s on an oci repo: %v, want it stored as it came", profile.AnnotationOriginURL, err)
	}
}

// existsGate is a registry whose first blob Exists waits: it closes in, and goes on once open
// is closed. checkReferrer asks it right after it resolved the subject (#806).
type existsGate struct {
	provider.Provider
	once     *sync.Once
	in, open chan struct{}
}

func newExistsGate(p provider.Provider) existsGate {
	return existsGate{Provider: p, once: &sync.Once{}, in: make(chan struct{}), open: make(chan struct{})}
}

func (g existsGate) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := g.Provider.Repository(ctx, name)
	return existsRepo{rr, g}, err
}

type existsRepo struct {
	provider.Repository
	g existsGate
}

func (r existsRepo) Exists(ctx context.Context, d ocispec.Descriptor) (bool, error) {
	r.g.once.Do(func() { close(r.g.in); <-r.g.open })
	return r.Repository.Exists(ctx, d)
}

// raceSubjectDelete pushes r to the repo of e through pipe, over g, and while the push waits at
// its first blob check, after it resolved the subject, deletes the version of subject through
// pipe. It returns the push's error.
func raceSubjectDelete(ctx context.Context, t *testing.T, e *env, pipe *ingest.Pipeline, g existsGate, r ingest.Referrer, subject digest.Digest) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := pipe.PutReferrer(context.WithoutCancel(ctx), e.repo, r, alice); done <- err }()
	select {
	case <-g.in:
	case err := <-done:
		t.Fatalf("PutReferrer returned before its blob check: %v", err)
	}
	_, err := pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: subject.String()}, alice)
	close(g.open)
	must(t, err)
	return <-done
}

// TestPutReferrerSubjectDeleted (#806): a referrer of a managed repo whose subject is deleted
// after the checks resolved it and before the push is refused, with the referrers API and
// without: the push resolves its subject again under the subject's tag lock, which the delete
// held, and is ErrInvalid naming it; nothing is pushed, no tag-schema index comes back, and its
// audit row is failed. A referrer pushed before the delete went with the subject.
func TestPutReferrerSubjectDeleted(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, rec, subject := referrerEnv(t, api)
		early := signature(t, subject, "signature")
		_, err := e.pipe.PutReferrer(t.Context(), e.repo, early, alice)
		must(t, err)
		rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
		must(t, err)
		must(t, rr.Push(t.Context(), ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString("late"), Size: 4},
			strings.NewReader("late")))
		g := newExistsGate(registry{Fake: e.prov, api: api, rec: rec})
		opts := e.options(g)
		opts.Index = rec
		r := signature(t, subject, "late")
		rec.locks, rec.pushes = nil, nil
		err = raceSubjectDelete(t.Context(), t, e, ingest.New(opts), g, r, subject.Digest)
		if !errors.Is(err, ingest.ErrInvalid) || !strings.Contains(err.Error(), subject.Digest.String()) {
			t.Errorf("api %t: PutReferrer after its subject's delete: %v, want ErrInvalid naming %s", api, err, subject.Digest)
		}
		gone(t, e, subject.Digest.String(), early.Desc.Digest.String(), r.Desc.Digest.String(), "sha256-"+subject.Digest.Encoded())
		var listed []ocispec.Descriptor
		must(t, rr.Referrers(t.Context(), subject, "", func(page []ocispec.Descriptor) error { listed = append(listed, page...); return nil }))
		same(t, "referrers listed after the delete", []ocispec.Descriptor(nil), listed)
		lock := "raw/scratch/notes@" + subject.Digest.String()
		same(t, "locks: the delete's, then the push's", []string{lock, lock}, rec.locks)
		same(t, "manifest pushes", []bool(nil), rec.pushes)
		if ev := e.rows(t).Audit[0]; ev.Action != "push" || ev.Digest != r.Desc.Digest || ev.State != index.AuditFailed {
			t.Errorf("api %t: the audit row of the refused push: %s %s %s", api, ev.Action, ev.Digest, ev.State)
		}
	}
}
