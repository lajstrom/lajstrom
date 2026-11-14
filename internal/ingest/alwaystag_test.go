package ingest_test

import (
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// alwaysTagEnv is referrerEnv on a registry with the referrers API, with referrers.alwaysTag or
// without.
func alwaysTagEnv(t *testing.T, alwaysTag bool) (*env, *lockRecorder, ocispec.Descriptor) {
	t.Helper()
	e, rec, subject := referrerEnv(t, true)
	opts := e.options(registry{Fake: e.prov, api: true, alwaysTag: alwaysTag, rec: rec})
	opts.Index = rec
	e.pipe = ingest.New(opts)
	return e, rec, subject
}

// TestReferrersAlwaysTag (#326): with the referrers API and referrers.alwaysTag, ingest keeps the
// subject's sha256-<hex> index as it does without the API: a referrer pushed by digest to an oci
// repo takes the referrers lock of its subject, the delete of one referrer pushes its subject's
// index again without it, and RepairReferrers pushes a lost index again. With the API alone it
// does none of the three.
func TestReferrersAlwaysTag(t *testing.T) {
	for _, alwaysTag := range []bool{true, false} {
		name := map[bool]string{true: "the API and alwaysTag", false: "the API alone"}[alwaysTag]
		t.Run(name+": a push to an oci repo", func(t *testing.T) {
			e, rec, _ := alwaysTagEnv(t, alwaysTag)
			nowhere := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("nowhere"), Size: 2}
			r := signature(t, nowhere, "a layer the backend checks, not ingest")
			r.Path = "app"
			_, err := e.pipe.PutReferrer(t.Context(), ociRepo(t, e), r, alice)
			must(t, err)
			var want []string
			if alwaysTag {
				want = []string{"oci/images/app@" + nowhere.Digest.String()}
			}
			same(t, "locks", want, rec.locks)
		})

		t.Run(name+": the delete of a referrer", func(t *testing.T) {
			e, _, subject := alwaysTagEnv(t, alwaysTag)
			refs, _ := threeReferrers(t, e, subject)
			got, err := e.pipe.Delete(t.Context(), e.repo, format.Deletion{Path: "notes", Reference: refs[0].String()}, alice)
			must(t, err)
			same(t, "deleted", refs[0], got.Digest)
			gone(t, e, refs[0].String())
			want := refs[1:]
			if !alwaysTag {
				want = refs // the index is not Lajstrom's to keep: left as it was
			}
			same(t, "the referrers the index lists", want, listedBy(t, e, "sha256-"+subject.Digest.Encoded()))
		})

		t.Run(name+": the repair of a lost index", func(t *testing.T) {
			ctx := t.Context()
			e, _, subject := alwaysTagEnv(t, alwaysTag)
			refs, lost := threeReferrers(t, e, subject)
			rr, err := e.prov.Repository(ctx, "raw/scratch/notes")
			must(t, err)
			must(t, rr.Delete(ctx, lost))
			got, err := e.pipe.RepairReferrers(ctx, e.repo, map[digest.Digest][]string{subject.Digest: {"notes"}}, alice)
			must(t, err)
			tag := "sha256-" + subject.Digest.Encoded()
			if !alwaysTag {
				if got != nil || listedBy(t, e, tag) != nil {
					t.Errorf("RepairReferrers with the API alone: %+v, want nothing pushed", got)
				}
				return
			}
			if len(got) != 1 || got[0].Err != nil || got[0].Index == "" {
				t.Fatalf("RepairReferrers: %+v, want the index pushed", got)
			}
			same(t, "added", refs, got[0].Added)
			same(t, "the referrers the index lists", refs, listedBy(t, e, tag))
		})
	}
}
