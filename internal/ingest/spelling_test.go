package ingest_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/ingest"
)

// wantSpelling fails the test unless err is the 409 of another spelling of a package path: an
// ErrConflict that is not a tag's, whose message has want.
func wantSpelling(t *testing.T, what string, err error, want string) {
	t.Helper()
	var tc *index.TagConflictError
	if !errors.Is(err, ingest.ErrConflict) || errors.As(err, &tc) || !strings.Contains(fmt.Sprint(err), want) ||
		format.StatusOf(err) != http.StatusConflict {
		t.Errorf("%s: %v, want the 409 of another spelling with %q", what, err, want)
	}
}

// TestIngestSpelling: after Docs/Readme.md is published, docs/readme.md, the other spelling of
// its package path, is the 409 naming Docs/Readme.md under the same tag and under another,
// through Ingest (the facade's path) and Upload (the frontends'). Nothing of it reaches the
// registry: no manifest, and from Upload no blob either. No claim is left, and the first
// spelling goes on publishing.
func TestIngestSpelling(t *testing.T) {
	e := newEnv(t)
	first, firstRaw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "first"}))
	_, err := e.ingest(t, first, firstRaw, pkgPath)
	must(t, err)
	lower := strings.ToLower(readme)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		other, otherRaw, otherPath := e.prepare(t, upload(lower, version, file{lower, "other " + version}))
		if otherPath != pkgPath {
			t.Fatalf("package path of %s = %s, want %s", lower, otherPath, pkgPath)
		}
		_, err := e.ingest(t, other, otherRaw, otherPath)
		wantSpelling(t, "Ingest of "+version, err, `exists as "`+readme+`"`)
		pushes := len(e.prov.Pushes())
		_, err = e.pipe.Upload(t.Context(), e.repo, upload(lower, version, file{lower, "upload " + version}), alice)
		wantSpelling(t, "Upload of "+version, err, `exists as "`+readme+`"`)
		if n := len(e.prov.Pushes()) - pushes; n != 0 {
			t.Errorf("Upload of %s pushed %d blobs of the refused spelling", version, n)
		}
		e.noClaim(t, readme, version)
	}
	if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Digest != digest.FromBytes(firstRaw) {
		t.Errorf("PushReferences = %+v, want the first spelling's only", refs)
	}
	e.noClaim(t, readme, "")

	next, nextRaw, _ := e.prepare(t, upload(readme, "2.0.0", file{readme, "next"}))
	if _, err := e.ingest(t, next, nextRaw, pkgPath); err != nil {
		t.Errorf("the first spelling under 2.0.0: %v", err)
	}
}

// TestIngestSpellingRace runs a publish of Docs/Race-N.md through Ingest and one of
// docs/race-n.md through Upload at once, under one tag and under two. The gate holds the first
// that gets past the claims in front of the registry until the other arrives: without the
// claim on the package path both would find the path free, in the index and in the backend,
// and push. With it exactly one succeeds, the other is the 409 of another spelling, and the
// registry sees one manifest in the OCI repository.
func TestIngestSpellingRace(t *testing.T) {
	const rounds = 5
	for _, sameTag := range []bool{true, false} {
		t.Run(fmt.Sprintf("same tag %v", sameTag), func(t *testing.T) {
			e := newEnv(t)
			var current atomic.Pointer[gate]
			e.pipe = ingest.New(e.options(hooked{Fake: e.prov, beforeResolve: func() { current.Load().pass(100 * time.Millisecond) }}))
			for i := range rounds {
				name := fmt.Sprintf("Docs/Race-%d.md", i)
				lower, tagA, tagB := strings.ToLower(name), fmt.Sprintf("1.0.%d", i), fmt.Sprintf("2.0.%d", i)
				if sameTag {
					tagB = tagA
				}
				a, aRaw, pkgPath := e.prepare(t, upload(name, tagA, file{name, "a"}))
				current.Store(newGate())
				errs := race(func() error { _, err := e.ingest(t, a, aRaw, pkgPath); return err },
					func() error {
						_, err := e.pipe.Upload(t.Context(), e.repo, upload(lower, tagB, file{lower, "b"}), alice)
						return err
					})
				// The loser names the winner when it is published already; while it is being
				// published, only the package path.
				switch {
				case errs[0] == nil && errs[1] != nil:
					wantSpelling(t, "Upload of "+lower, errs[1], "package path "+pkgPath)
				case errs[1] == nil && errs[0] != nil:
					wantSpelling(t, "Ingest of "+name, errs[0], "package path "+pkgPath)
				default:
					t.Errorf("round %d: errors %v, want exactly one nil", i, errs)
				}
				var pushed []string
				for _, r := range e.prov.PushReferences() {
					if r.Name == a.RepoPath {
						pushed = append(pushed, r.Tag)
					}
				}
				if len(pushed) != 1 {
					t.Errorf("round %d: manifests pushed to %s under %v, want one", i, a.RepoPath, pushed)
				}
				e.noClaim(t, name, "")
			}
		})
	}
}

// TestIngestPathClaim checks the claim on the package path that another publish left: a fresh
// one of another spelling is the 409 and nothing is pushed, a stale one is taken over, and a
// stale one of the spelling is made again, so that a publish of another spelling meanwhile is
// refused rather than taking it over in turn.
func TestIngestPathClaim(t *testing.T) {
	lower := strings.ToLower(readme)
	for _, tt := range []struct {
		name    string
		age     time.Duration
		of      string // the spelling of the claim left
		refused bool
	}{
		{"fresh, another spelling", time.Minute, lower, true},
		{"stale, another spelling", 11 * time.Minute, lower, false},
		{"stale, this spelling", 11 * time.Minute, readme, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			built, raw, pkgPath := e.prepare(t, upload(readme, "1.0.0", file{readme, "hello"}))
			left := index.TagClaim{RepoID: e.repo.ID, Package: pkgPath, Digest: digest.FromString(tt.of), Principal: "user:bob",
				ClaimedAt: now.Add(-tt.age)}
			must(t, e.st.ClaimTag(t.Context(), &left, now.Add(-time.Hour)))
			// While the publish is in front of the registry, docs/readme.md tries too, once: should
			// it get as far, its own Resolve passes.
			var meanwhile error
			var tried atomic.Bool
			e.pipe = ingest.New(e.options(hooked{Fake: e.prov, beforeResolve: func() {
				if tried.CompareAndSwap(false, true) {
					other, otherRaw, _ := e.prepare(t, upload(lower, "2.0.0", file{lower, "meanwhile"}))
					_, meanwhile = e.ingest(t, other, otherRaw, pkgPath)
				}
			}}))
			_, err := e.ingest(t, built, raw, pkgPath)
			if tt.refused {
				wantSpelling(t, "Ingest", err, "being published")
				if n := len(e.prov.PushReferences()); n != 0 {
					t.Errorf("%d PushReference calls under a fresh claim of another spelling, want none", n)
				}
				return
			}
			must(t, err)
			wantSpelling(t, "the other spelling meanwhile", meanwhile, "being published")
			if refs := e.prov.PushReferences(); len(refs) != 1 || refs[0].Tag != "1.0.0" {
				t.Errorf("PushReferences = %+v, want 1.0.0 only", refs)
			}
			e.noClaim(t, readme, "")
		})
	}
}
