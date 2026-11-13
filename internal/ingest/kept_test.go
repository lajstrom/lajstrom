package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
)

// keeping is a registry that answers 404 to the delete of the manifests of keep and still
// serves them, as Quay 3.18 does for a referrer pushed by digest (#817); with gone, it deletes
// the manifest and answers 404 all the same, as for a manifest an earlier attempt deleted.
type keeping struct {
	registry
	keep map[digest.Digest]bool
	gone bool
}

func (f keeping) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.registry.Repository(ctx, name)
	return keepingRepo{rr, f}, err
}

type keepingRepo struct {
	provider.Repository
	f keeping
}

func (r keepingRepo) Delete(ctx context.Context, desc ocispec.Descriptor) error {
	if !r.f.keep[desc.Digest] {
		return r.Repository.Delete(ctx, desc)
	}
	if r.f.gone {
		if err := r.Repository.Delete(ctx, desc); err != nil {
			return err
		}
	}
	return fmt.Errorf("keeping: delete %s: %w", desc.Digest, provider.ErrNotFound)
}

// TestDeleteKept: a referrer whose delete the registry answers 404 while it still serves it is
// kept, not deleted: the delete goes on (the backend keeps it whatever Lajstrom asks), but the
// referrer is not counted, the audit row names it under kept, and a warning names it. A 404 for
// a manifest the registry does not serve any more is gone, as before (#668). Both for a version
// with its referrers and for a referrer deleted on its own, with and without the referrers API.
func TestDeleteKept(t *testing.T) {
	subject := func(a attached) digest.Digest { return a.subject.Digest }
	signature := func(a attached) digest.Digest { return a.refs[1] }
	for _, tt := range []keptCase{
		{name: "the signature of a version, referrers API", api: true, ref: subject, kept: signature, referrers: 3, version: true},
		{name: "the signature of a version, tag schema", ref: subject, kept: signature, referrers: 3, version: true},
		{name: "a signature on its own, tag schema", ref: signature, kept: signature},
	} {
		t.Run(tt.name, func(t *testing.T) { tt.run(t) })
	}
}

// keptCase is a case of TestDeleteKept.
type keptCase struct {
	name string
	api  bool
	// ref is the reference deleted, kept the manifest the registry keeps, of a.
	ref, kept func(a attached) digest.Digest
	// referrers and version are what Deleted says.
	referrers int
	version   bool
}

// run deletes ref on a registry that keeps kept, and checks what Delete says, what the registry
// still serves, the audit row and the warning.
func (c keptCase) run(t *testing.T) {
	e, rec, subject := referrerEnv(t, c.api)
	a := attach(t, e, subject, c.api)
	ref, kept := c.ref(a), c.kept(a)
	var log bytes.Buffer
	opts := e.options(keeping{registry: registry{Fake: e.prov, api: c.api, rec: rec}, keep: map[digest.Digest]bool{kept: true}})
	opts.Index = rec
	opts.Log = slog.New(slog.NewTextHandler(&log, nil))
	got, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: ref.String()}, alice)
	must(t, err)
	if got.Digest != ref || (got.Version != nil) != c.version || got.Referrers != c.referrers {
		t.Errorf("Deleted = %+v, want %s with %d referrers, a version %t", got, ref, c.referrers, c.version)
	}
	rr, err := e.prov.Repository(t.Context(), "raw/scratch/notes")
	must(t, err)
	if _, err := rr.Resolve(t.Context(), kept.String()); err != nil {
		t.Errorf("the registry should still serve %s: %v", kept, err)
	}
	if c.version {
		var others []string
		for _, d := range a.refs {
			if d != kept {
				others = append(others, d.String())
			}
		}
		gone(t, e, others...)
	}
	ev := e.rows(t).Audit[0]
	var details struct {
		Referrers int             `json:"referrers"`
		Kept      []digest.Digest `json:"kept"`
	}
	must(t, json.Unmarshal(ev.Details, &details))
	if ev.Action != "delete" || ev.Digest != ref || details.Referrers != c.referrers || !slices.Equal(details.Kept, []digest.Digest{kept}) {
		t.Errorf("audit event %+v, details %s; want referrers %d, kept %s", ev, ev.Details, c.referrers, kept)
	}
	if !slices.ContainsFunc(slices.Collect(strings.Lines(log.String())), func(line string) bool {
		return strings.Contains(line, "level=WARN") && strings.Contains(line, kept.String())
	}) {
		t.Errorf("no warning names the manifest kept, %s:\n%s", kept, log.String())
	}
}

// TestDeleteGoneNotKept: a referrer whose delete the registry answers 404 and which it does not
// serve any more, as when an earlier attempt deleted it (#668), is counted as deleted, and the
// audit row has no kept detail.
func TestDeleteGoneNotKept(t *testing.T) {
	e, rec, subject := referrerEnv(t, true)
	a := attach(t, e, subject, true)
	opts := e.options(keeping{registry: registry{Fake: e.prov, api: true, rec: rec}, keep: map[digest.Digest]bool{a.refs[1]: true}, gone: true})
	opts.Index = rec
	got, err := ingest.New(opts).Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: a.tag, ClientIP: "192.0.2.7"}, alice)
	must(t, err)
	if got.Referrers != 4 {
		t.Errorf("Deleted = %+v, want 4 referrers", got)
	}
	wantVersionGone(t, e, a)
	if ev := e.rows(t).Audit[0]; strings.Contains(string(ev.Details), "kept") {
		t.Errorf("audit details %s: want no kept", ev.Details)
	}
}
