package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// attached is a version with what refers to it in the backend: cosign's legacy signature under
// sha256-<hex>.sig, a signature, an attestation and a signature of the attestation (a referrer
// of a referrer); without the referrers API also the tag-schema index sha256-<hex>, which the
// fake does not write itself.
type attached struct {
	subject ocispec.Descriptor
	tag     string
	refs    []digest.Digest // every referrer, the legacy one included
	index   digest.Digest   // the tag-schema index; "" with the referrers API
}

// attach pushes the referrers of the env's version through PutReferrer.
func attach(t *testing.T, e *env, subject ocispec.Descriptor, api bool) attached {
	t.Helper()
	ctx := t.Context()
	rr, err := e.prov.Repository(ctx, "raw/scratch/notes")
	must(t, err)
	for _, b := range []string{"attestation", "countersignature"} {
		must(t, rr.Push(ctx, ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromString(b), Size: int64(len(b))}, bytes.NewReader([]byte(b))))
	}
	a := attached{subject: subject, tag: e.rows(t).Versions[0].Tag}
	put := func(r ingest.Referrer) ocispec.Descriptor {
		_, err := e.pipe.PutReferrer(ctx, e.repo, r, alice)
		must(t, err)
		a.refs = append(a.refs, r.Desc.Digest)
		return r.Desc
	}
	legacy := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	put(ingest.Referrer{Path: "notes", Tag: "sha256-" + subject.Digest.Encoded() + ".sig", Manifest: legacy, Subject: subject.Digest,
		Desc: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(legacy), Size: int64(len(legacy))}})
	sig := put(signature(t, subject, "signature"))
	att := put(signature(t, subject, "attestation"))
	counter := signature(t, att, "countersignature")
	counter.Subject = att.Digest
	put(counter)
	if !api {
		b, err := json.Marshal(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex,
			Manifests: []ocispec.Descriptor{sig, att}})
		must(t, err)
		desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(b), Size: int64(len(b))}
		must(t, rr.PushReference(ctx, desc, bytes.NewReader(b), "sha256-"+subject.Digest.Encoded()))
		a.index = desc.Digest
	}
	return a
}

// gone fails the test when the backend still has one of refs, by digest or by tag.
func gone(t *testing.T, e *env, refs ...string) {
	t.Helper()
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	for _, ref := range refs {
		if _, err := rr.Resolve(t.Context(), ref); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("the backend has %s: %v", ref, err)
		}
	}
}

// TestDeleteVersion: a version deleted by its tag and by its digest, with and without the
// referrers API: the referrers go, theirs too, the legacy signature and the tag-schema index,
// then the manifest; the rows go, the package with its last version, the repo's rendered
// documents too, and one audit row counts the referrers. With the API or without, it runs under
// one lock, the tag lock of the version.
func TestDeleteVersion(t *testing.T) {
	for _, tt := range []struct {
		name string
		api  bool
		ref  func(attached) string
	}{
		{"by tag, referrers API", true, func(a attached) string { return a.tag }},
		{"by digest, tag schema", false, func(a attached) string { return a.subject.Digest.String() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, rec, subject := referrerEnv(t, tt.api)
			a := attach(t, e, subject, tt.api)
			putCache(t, e.st, &index.Cache{RepoID: e.repo.ID, Scope: "", Kind: "listing", Content: []byte("{}"), RenderedAt: now})
			rec.locks = nil
			got, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: tt.ref(a), ClientIP: "192.0.2.7"}, alice)
			must(t, err)
			if got.Digest != subject.Digest || got.Version == nil || got.Version.Version != "1.0" || got.Referrers != 4 {
				t.Errorf("Deleted = %+v, want %s, version 1.0, 4 referrers", got, subject.Digest)
			}
			wantVersionGone(t, e, a)
			same(t, "locks", []string{"raw/scratch/notes@" + subject.Digest.String()}, rec.locks)
		})
	}
}

// wantVersionGone checks what the delete of the version of a leaves: nothing of it in the
// backend, no rows, no rendered document, and the audit row with its four referrers.
func wantVersionGone(t *testing.T, e *env, a attached) {
	t.Helper()
	refs := []string{a.subject.Digest.String(), "sha256-" + a.subject.Digest.Encoded(), "sha256-" + a.subject.Digest.Encoded() + ".sig"}
	for _, r := range a.refs {
		refs = append(refs, r.String())
	}
	gone(t, e, refs...)
	rows := e.rows(t)
	if len(rows.Packages) != 0 || len(rows.Versions) != 0 {
		t.Errorf("rows left: %+v", rows)
	}
	if c, err := e.st.GetIndexCache(t.Context(), e.repo.ID, "", "listing"); !errors.Is(err, index.ErrNotFound) {
		t.Errorf("index_cache kept %+v, %v", c, err)
	}
	ev := rows.Audit[0]
	var details map[string]int
	must(t, json.Unmarshal(ev.Details, &details))
	if ev.Action != "delete" || ev.Subject != "notes 1.0" || ev.Digest != a.subject.Digest || ev.IP != "192.0.2.7" ||
		ev.Principal != alice.String() || ev.Repo != "raw/scratch" || details["referrers"] != 4 {
		t.Errorf("audit event %+v, details %v", ev, details)
	}
}

// refusing is a registry whose capabilities say it deletes manifests and which refuses every
// delete, as distribution with deletes off answers until the provider learns it; it records
// what was asked.
type refusing struct {
	registry
	mu      *sync.Mutex
	deletes *[]digest.Digest
}

func (f refusing) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.registry.Repository(ctx, name)
	return refusingRepo{rr, f}, err
}

type refusingRepo struct {
	provider.Repository
	f refusing
}

func (r refusingRepo) Delete(_ context.Context, desc ocispec.Descriptor) error {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	*r.f.deletes = append(*r.f.deletes, desc.Digest)
	return provider.ErrUnsupported
}

// TestDeleteRefused: a backend that cannot delete manifests changes nothing. With
// DeleteManifest false nothing is asked, and no audit row is written; one that refuses the
// first delete is asked a plain delete first, the legacy signature (no subject: no tag-schema
// index is rewritten for it), and nothing else, and the delete has its audit row, failed with
// the refusal (#473).
func TestDeleteRefused(t *testing.T) {
	t.Run("capability", func(t *testing.T) {
		e, _, subject := referrerEnv(t, false)
		attach(t, e, subject, false)
		before := e.rows(t)
		e.prov.NoDeleteManifest = true
		_, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: subject.Digest.String()}, alice)
		if !errors.Is(err, provider.ErrUnsupported) {
			t.Fatalf("Delete = %v, want ErrUnsupported", err)
		}
		same(t, "rows", before, e.rows(t))
	})
	t.Run("first delete", func(t *testing.T) {
		e, rec, subject := referrerEnv(t, false)
		a := attach(t, e, subject, false)
		before := e.rows(t)
		var deletes []digest.Digest
		opts := e.options(refusing{registry{Fake: e.prov, api: false, rec: rec}, &sync.Mutex{}, &deletes})
		opts.Index = rec
		_, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: a.tag}, alice)
		if !errors.Is(err, provider.ErrUnsupported) {
			t.Fatalf("Delete = %v, want ErrUnsupported", err)
		}
		same(t, "deletes asked", []digest.Digest{a.refs[0]}, deletes)
		after := e.rows(t)
		if ev := after.Audit[0]; ev.Action != "delete" || ev.Digest != subject.Digest || ev.State != index.AuditFailed ||
			!strings.Contains(ev.Error, provider.ErrUnsupported.Error()) {
			t.Errorf("the newest audit row %+v, want the delete of %s, failed with the refusal", ev, subject.Digest)
		}
		after.Audit = after.Audit[1:]
		same(t, "rows", before, after)
	})
}

// TestDeleteNotFound: a reference that names no version is index.ErrNotFound, and so is a
// package path the repo does not have; a version whose manifest the backend lost goes from the
// index all the same, its referrers by its digest.
func TestDeleteNotFound(t *testing.T) {
	e, _, subject := referrerEnv(t, true)
	for _, del := range []format.Deletion{
		{Path: "notes", Reference: "9.9"},
		{Path: "notes", Reference: digest.FromString("no such manifest").String()},
		{Path: "elsewhere", Reference: "1.0"},
	} {
		if _, err := e.pipe.Delete(t.Context(), e.repo, del, alice); !errors.Is(err, index.ErrNotFound) {
			t.Errorf("Delete %+v = %v, want ErrNotFound", del, err)
		}
	}
	a := attach(t, e, subject, true)
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	must(t, rr.Delete(t.Context(), subject))
	got, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: a.tag}, alice)
	must(t, err)
	if got.Referrers != 4 || len(e.rows(t).Versions) != 0 {
		t.Errorf("Deleted %+v, rows %+v", got, e.rows(t))
	}
}

// TestDeleteThenReindex: a delete leaves the index as reindex rebuilds it. A run with prune over
// the repo afterwards adds, reads again and removes nothing, keeps the other package, and finds
// no orphan referrer: the version's went with it.
func TestDeleteThenReindex(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, _, subject := referrerEnv(t, api)
		attach(t, e, subject, api)
		built, raw, pkgPath := e.prepare(t, upload("kept", "2.0", file{"kept", "kept"}))
		_, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
		_, err = e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: subject.Digest.String()}, alice)
		must(t, err)
		w := reindex.New(reindex.Options{Index: e.st, Providers: provider.Map{"local": e.prov}, Inspector: e.pipe, Log: slog.New(slog.DiscardHandler)})
		req := reindex.Request{Repo: &e.repo, Prune: true, Source: "cli", Principal: alice.String()}
		run, err := w.Start(t.Context(), req)
		must(t, err)
		rep, err := w.Run(t.Context(), run, req)
		must(t, err)
		if r := rep.Repos[0]; r.Added != 0 || r.Updated != 0 || r.Removed != 0 || r.Unchanged != 1 || len(r.OrphanReferrers) != 0 || len(r.Errors) != 0 {
			t.Errorf("api %t: reindex after the delete: %+v", api, r)
		}
	}
}

// failingTx is the store whose transactions fail.
type failingTx struct{ index.Store }

func (failingTx) Tx(context.Context, func(index.Writer) error) error { return errors.New("boom") }

// TestDeleteRowsFail: when the index fails after the registry deleted the manifest, the error
// has no sentinel (500, not a 404 of the index) and says so, and the delete has its audit row,
// failed with that error (#474); reindex repairs the rows.
func TestDeleteRowsFail(t *testing.T) {
	e, _, subject := referrerEnv(t, true)
	opts := e.options(e.prov)
	opts.Index = failingTx{e.st}
	_, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: subject.Digest.String()}, alice)
	if err == nil || errors.Is(err, index.ErrNotFound) || !strings.Contains(err.Error(), "after the registry deleted") {
		t.Errorf("Delete = %v", err)
	}
	gone(t, e, subject.Digest.String())
	if ev := e.rows(t).Audit[0]; ev.Action != "delete" || ev.Digest != subject.Digest || ev.State != index.AuditFailed || ev.Error != err.Error() {
		t.Errorf("the newest audit row %+v, want the delete of %s, failed with %q", ev, subject.Digest, err)
	}
}

// TestDeleteKeepsPackage: a version that is not the last of its package leaves the package.
func TestDeleteKeepsPackage(t *testing.T) {
	e := newEnv(t)
	for _, content := range []string{"one", "two"} {
		built, raw, pkgPath := e.prepare(t, upload("notes", content, file{"notes", content}))
		_, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
	}
	_, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: "one"}, alice)
	must(t, err)
	rows := e.rows(t)
	if len(rows.Packages) != 1 || len(rows.Versions) != 1 || rows.Versions[0].Version != "two" {
		t.Errorf("rows %+v, want the package with version two", rows)
	}
}

// TestDeleteReferrer: a referrer deleted on its own, with its own referrers, and no version
// with it; the audit row names its subject. Without the referrers API the subject's tag-schema
// index is written again without it. Each delete runs under the subject's lock, with the API
// too: the tag lock of a referrer.
func TestDeleteReferrer(t *testing.T) {
	for _, api := range []bool{true, false} {
		e, rec, subject := referrerEnv(t, api)
		a := attach(t, e, subject, api)
		sig, att := a.refs[1], a.refs[2]
		rec.locks = nil
		for _, tt := range []struct {
			ref  digest.Digest
			want int
		}{{sig, 0}, {att, 1}} {
			got, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: tt.ref.String()}, alice)
			must(t, err)
			if got.Version != nil || got.Digest != tt.ref || got.Referrers != tt.want {
				t.Errorf("api %t: Deleted %+v, want %s with %d referrers and no version", api, got, tt.ref, tt.want)
			}
		}
		gone(t, e, sig.String(), att.String(), a.refs[3].String())
		rows := e.rows(t)
		if len(rows.Versions) != 1 || rows.Audit[0].Action != "delete" || rows.Audit[0].Subject != subject.Digest.String() {
			t.Errorf("api %t: rows %+v", api, rows)
		}
		key := "raw/scratch/notes@" + subject.Digest.String()
		same(t, "locks", []string{key, key}, rec.locks)
		if api {
			continue
		}
		// The index listed sig and att: without sig it lists att, without att it is gone.
		gone(t, e, "sha256-"+subject.Digest.Encoded())
		if pushes := e.pushRefs("sha256-" + subject.Digest.Encoded()); len(pushes) != 2 {
			t.Errorf("index pushes %+v: want the test's and one without the signature", pushes)
		}
	}
}

// TestDeleteReferrerRefused: without the referrers API, the first delete of a referrer on its
// own is its subject's index, a plain delete; refused, nothing changed.
func TestDeleteReferrerRefused(t *testing.T) {
	e, rec, subject := referrerEnv(t, false)
	a := attach(t, e, subject, false)
	var deletes []digest.Digest
	opts := e.options(refusing{registry{Fake: e.prov, api: false, rec: rec}, &sync.Mutex{}, &deletes})
	opts.Index = rec
	_, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: a.refs[1].String()}, alice)
	if !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Delete = %v, want ErrUnsupported", err)
	}
	same(t, "deletes asked", []digest.Digest{a.index}, deletes)
	if d := e.tagged(t, "raw/scratch/notes", "sha256-"+subject.Digest.Encoded()); d != a.index {
		t.Errorf("the index tag names %s, want %s", d, a.index)
	}
}

// TestDeleteManifests: a manifest of an oci repo by its tag, audited with its repository; a
// foreign tag of a managed repo that names a version's manifest deletes the version.
func TestDeleteManifests(t *testing.T) {
	e, _, subject := referrerEnv(t, true)
	ctx := t.Context()
	repo := index.Repo{Format: "oci", Name: "images", Registry: "local", Prefix: "oci/images", Kind: "hosted"}
	must(t, e.st.CreateRepo(ctx, &repo))
	rr, err := e.prov.Repository(ctx, "oci/images/app")
	must(t, err)
	img := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":` +
		`{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + ocispec.DescriptorEmptyJSON.Digest.String() + `","size":2},"layers":[]}`)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(img), Size: int64(len(img))}
	must(t, rr.PushReference(ctx, desc, bytes.NewReader(img), "v1"))
	got, err := e.pipe.Delete(ctx, repo, format.Deletion{Path: "app", Reference: "v1"}, alice)
	must(t, err)
	if got.Digest != desc.Digest || got.Version != nil {
		t.Errorf("Deleted %+v", got)
	}
	if ev := e.rows(t).Audit[0]; ev.Repo != "oci/images" || ev.Subject != "oci/images/app" || ev.Digest != desc.Digest {
		t.Errorf("audit event %+v", ev)
	}
	notes, err := e.prov.Repository(ctx, "raw/scratch/notes")
	must(t, err)
	must(t, notes.Tag(ctx, subject, "foreign"))
	got, err = e.pipe.Delete(ctx, e.repo, format.Deletion{Path: "notes", Reference: "foreign"}, alice)
	must(t, err)
	if got.Version == nil || len(e.rows(t).Versions) != 0 {
		t.Errorf("Deleted %+v: the foreign tag's manifest is the version", got)
	}
}
