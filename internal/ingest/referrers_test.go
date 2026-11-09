package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
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
// its kind and artifact type; under the referrers lock of its subject when the backend has no
// referrers API, and without one when it has.
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
		wantLocks := []string{"raw/scratch/notes@" + subject.Digest.String()}
		if api {
			wantLocks = nil
		}
		same(t, "locks", wantLocks, rec.locks)
		same(t, "manifest pushes under the lock", []bool{!api}, rec.pushes)
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
// included (the conformance suite pushes one), under the lock when the backend has no
// referrers API.
func TestPutReferrerOCI(t *testing.T) {
	e, rec, _ := referrerEnv(t, false)
	repo := index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: "hosted"}
	must(t, e.st.CreateRepo(t.Context(), &repo))
	nowhere := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("nowhere"), Size: 2}
	r := signature(t, nowhere, "a layer the backend checks, not ingest")
	r.Path, r.Tag = "app", "anything"
	_, err := e.pipe.PutReferrer(t.Context(), repo, r, alice)
	must(t, err)
	same(t, "locks", []string{"oci/images/app@" + nowhere.Digest.String()}, rec.locks)
}
