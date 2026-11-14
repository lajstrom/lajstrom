package ingest_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// unknownFold is a registry that folds the case of tags while its capabilities do not say so: a
// registry the vendor table does not know, without capabilities.tagsFoldCase.
type unknownFold struct{ provider.Provider }

func (u unknownFold) Capabilities(ctx context.Context) (provider.Capabilities, error) {
	c, err := u.Provider.Capabilities(ctx)
	c.TagsFoldCase = false
	return c, err
}

// TestIngestFoldsCase (#426): on a registry that folds the case of tags and says so
// (Capabilities.TagsFoldCase), a version with capitals is stored as anywhere, the same version
// again is the idempotent repeat, a version whose tag differs only in case is a 409 that names
// the version holding the tag, through Ingest and Upload alike, and a publish of a tag of another
// case under way holds the claim, so nothing is pushed. A registry that keeps case takes both
// versions, and so does the claim of a folding registry Lajstrom does not know as one.
func TestIngestFoldsCase(t *testing.T) {
	const name, upper, lower = "notes", "1.0-RC1", "1.0-rc1"
	folding := func(f *providertest.Fake) provider.Provider { return providertest.FoldCase(f) }
	keeping := func(f *providertest.Fake) provider.Provider { return f }
	unknown := func(f *providertest.Fake) provider.Provider { return unknownFold{providertest.FoldCase(f)} }

	t.Run("a version with capitals, then again", func(t *testing.T) {
		e := newEnv(t)
		e.pipe = ingest.New(e.options(folding(e.prov)))
		built, raw, pkgPath := e.prepare(t, upload(name, upper, file{"notes.txt", "first"}))
		res, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
		same(t, "the row's tag", upper, e.rows(t).Versions[0].Tag)
		again, err := e.ingest(t, built, raw, pkgPath)
		must(t, err)
		if !again.Existed || again.Digest != res.Digest {
			t.Errorf("the same version again: %+v, want the idempotent repeat of %s", again, res.Digest)
		}
		e.noClaim(t, name, lower)
	})

	tests := []struct {
		name     string
		registry func(*providertest.Fake) provider.Provider
		upload   bool   // the second version through Upload, which may take over a manifest of the same files
		want     string // in the error of the second version; "" when it is stored
	}{
		{name: "a version of another case", registry: folding,
			want: `registry local folds the case of tags, and "1.0-rc1" is the tag of version 1.0-RC1 of notes there`},
		{name: "a version of another case, uploaded", registry: folding, upload: true,
			want: `registry local folds the case of tags, and "1.0-rc1" is the tag of version 1.0-RC1 of notes there`},
		{name: "a registry that keeps case", registry: keeping},
		{name: "a folding registry not known as one", registry: unknown, want: `index: tag "1.0-rc1" already points at`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.pipe = ingest.New(e.options(tt.registry(e.prov)))
			built, raw, pkgPath := e.prepare(t, upload(name, upper, file{"notes.txt", "first"}))
			first, err := e.ingest(t, built, raw, pkgPath)
			must(t, err)
			before := e.rows(t)

			var second digest.Digest
			if tt.upload {
				res, uerr := e.pipe.Upload(t.Context(), e.repo, upload(name, lower, file{"notes.txt", "second"}), alice)
				if res != nil {
					second = res.Digest
				}
				err = uerr
			} else {
				built, raw, pkgPath := e.prepare(t, upload(name, lower, file{"notes.txt", "second"}))
				var res *ingest.Result
				res, err = e.ingest(t, built, raw, pkgPath)
				if res != nil {
					second = res.Digest
				}
			}
			if tt.want == "" {
				must(t, err)
				same(t, "versions", len(before.Versions)+1, len(e.rows(t).Versions))
				same(t, "the tag of the second version", second, e.tagged(t, "raw/scratch/notes", lower))
				return
			}
			var conflict *index.TagConflictError
			if !errors.As(err, &conflict) || conflict.Tag != lower || conflict.Existing != first.Digest || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the second version: %v, want a *index.TagConflictError on %s naming %s, with %q", err, lower, first.Digest, tt.want)
			}
			if folds := strings.Contains(err.Error(), "folds the case"); folds != strings.Contains(tt.want, "folds the case") {
				t.Errorf("the message says that the registry folds the case of tags: %v, want %v\n%v", folds, !folds, err)
			}
			after := e.rows(t)
			same(t, "versions after the 409", before.Versions, after.Versions)
			same(t, "the folded tag, as the fake keeps it", first.Digest, e.tagged(t, "raw/scratch/notes", lower))
			e.noClaim(t, name, lower)
		})
	}
}

// TestIngestFoldsCaseClaim (#426): on a registry that folds the case of tags, a fresh claim of
// another digest on the tag lower-cased, a publish of the tag of another case under way, refuses
// the publish of a tag with capitals before anything is pushed; on a registry that keeps case,
// and on a folding one not known as one, the two are two claims and the publish goes on.
func TestIngestFoldsCaseClaim(t *testing.T) {
	const upper, lower = "1.0-RC1", "1.0-rc1"
	tests := []struct {
		name     string
		registry func(*providertest.Fake) provider.Provider
		refused  bool
	}{
		{"folding", func(f *providertest.Fake) provider.Provider { return providertest.FoldCase(f) }, true},
		{"keeping case", func(f *providertest.Fake) provider.Provider { return f }, false},
		{"folding, not known as one", func(f *providertest.Fake) provider.Provider { return unknownFold{providertest.FoldCase(f)} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			e := newEnv(t)
			e.pipe = ingest.New(e.options(tt.registry(e.prov)))
			other := index.TagClaim{RepoID: e.repo.ID, Package: "notes", Tag: lower, Digest: digest.FromString("another publish"),
				Principal: "user:bob", ClaimedAt: now}
			must(t, e.st.ClaimTag(ctx, &other, now.Add(-time.Minute)))

			built, raw, pkgPath := e.prepare(t, upload("notes", upper, file{"notes.txt", "first"}))
			_, err := e.ingest(t, built, raw, pkgPath)
			if !tt.refused {
				must(t, err)
				return
			}
			var conflict *index.TagConflictError
			want := `registry local folds the case of tags, so the claim on "1.0-RC1" is on "1.0-rc1"`
			if !errors.As(err, &conflict) || conflict.Existing != other.Digest || !strings.Contains(err.Error(), want) {
				t.Fatalf("the publish of %s: %v, want a *index.TagConflictError naming %s, with %q", upper, err, other.Digest, want)
			}
			if refs := append(e.pushRefs(upper), e.pushRefs(lower)...); len(refs) != 0 {
				t.Errorf("pushed under the claim of another: %+v", refs)
			}
			if c, err := e.st.GetTagClaim(ctx, e.repo.ID, "notes", lower); err != nil || c.Digest != other.Digest {
				t.Errorf("the other claim: %+v, %v; want it as it was", c, err)
			}
		})
	}
}
