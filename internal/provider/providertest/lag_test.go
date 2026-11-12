package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/lajstrom/lajstrom/internal/provider"
)

// lagged pushes a manifest under tag to the repository name of l, and returns its descriptor.
func lagged(t *testing.T, l *Lag, name, tag string) ocispec.Descriptor {
	t.Helper()
	m, _ := json.Marshal(ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Annotations: map[string]string{"tag": tag}})
	desc := descriptor(ocispec.MediaTypeImageManifest, m)
	rr, err := l.Repository(t.Context(), name)
	if err == nil {
		err = rr.PushReference(t.Context(), desc, bytes.NewReader(m), tag)
	}
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// tagsOf lists the tags of the repository name of l.
func tagsOf(ctx context.Context, l *Lag, name string) ([]string, error) {
	rr, err := l.Repository(ctx, name)
	if err != nil {
		return nil, err
	}
	var tags []string
	err = rr.Tags(ctx, "", func(page []string) error { tags = append(tags, page...); return nil })
	return tags, err
}

// TestLagLists checks Lists: a tag pushed is left out of the next tag lists of its repository,
// a repository a push created out of the next catalogs, each as many times, and Serve ends it.
func TestLagLists(t *testing.T) {
	ctx := t.Context()
	l := &Lag{Fake: &Fake{}, Lists: 2}
	lagged(t, l, name, "1.0.0")
	for i, want := range [][]string{nil, nil, {name}} {
		if got, err := l.ListRepositories(ctx, "raw/"); err != nil || !slices.Equal(got, want) {
			t.Errorf("catalog %d = %v, %v; want %v", i+1, got, err, want)
		}
	}
	for i, want := range [][]string{nil, nil, {"1.0.0"}} {
		if got, err := tagsOf(ctx, l, name); err != nil || !slices.Equal(got, want) {
			t.Errorf("tag list %d = %v, %v; want %v", i+1, got, err, want)
		}
	}
	lagged(t, l, name, "2.0.0") // the repository exists: the catalog names it at once
	if got, err := l.ListRepositories(ctx, "raw/"); err != nil || !slices.Equal(got, []string{name}) {
		t.Errorf("catalog after a push to the repository = %v, %v", got, err)
	}
	if got, err := tagsOf(ctx, l, name); err != nil || !slices.Equal(got, []string{"1.0.0"}) {
		t.Errorf("tag list after the second push = %v, %v; want the first tag alone", got, err)
	}
	if l.Misses() != 5 {
		t.Errorf("Misses = %d, want 5", l.Misses())
	}
	l.Serve()
	if got, err := tagsOf(ctx, l, name); err != nil || !slices.Equal(got, []string{"1.0.0", "2.0.0"}) {
		t.Errorf("tag list after Serve = %v, %v", got, err)
	}
}

// TestLagDeletes checks Deletes: after the delete of a manifest, reads by its digest (Exists,
// Fetch, Resolve, StatManifest) find it as many times, so do reads by its tag and the tag lists
// of its repository, each counted on its own; then each answers as the Fake does.
func TestLagDeletes(t *testing.T) {
	ctx := t.Context()
	l := &Lag{Fake: &Fake{}, Deletes: 4}
	desc := lagged(t, l, name, "1.0.0")
	lagged(t, l, name, "2.0.0")
	rr, err := l.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := rr.Delete(ctx, desc); err != nil {
		t.Fatal(err)
	}
	byDigest := []struct {
		what string
		read func() error
	}{
		{"Exists", func() error {
			if ok, err := rr.Exists(ctx, desc); !ok || err != nil {
				return fmt.Errorf("%v, %w", ok, err)
			}
			return nil
		}},
		{"Fetch", func() error {
			rc, err := rr.Fetch(ctx, desc)
			if err != nil {
				return err
			}
			if b, _ := io.ReadAll(rc); int64(len(b)) != desc.Size {
				return fmt.Errorf("read %d bytes, want %d", len(b), desc.Size)
			}
			return nil
		}},
		{"Resolve by digest", func() error { _, err := rr.Resolve(ctx, desc.Digest.String()); return err }},
		{"StatManifest", func() error { _, err := l.StatManifest(ctx, name, desc.Digest); return err }},
	}
	for _, r := range byDigest {
		if err := r.read(); err != nil {
			t.Errorf("%s after the delete: %v", r.what, err)
		}
	}
	if _, err := l.StatManifest(ctx, name, desc.Digest); !errors.Is(err, provider.ErrManifestUnknown) {
		t.Errorf("a fifth read by digest: %v, want ErrManifestUnknown", err)
	}
	for i := range 5 {
		_, rerr := rr.Resolve(ctx, "1.0.0")
		tags, lerr := tagsOf(ctx, l, name)
		want := []string{"1.0.0", "2.0.0"}
		if i == 4 {
			want = []string{"2.0.0"}
		}
		if (rerr == nil) != (i < 4) || lerr != nil || !slices.Equal(tags, want) {
			t.Errorf("read %d of the tag after the delete: Resolve %v, tag list %v, %v; want %v", i+1, rerr, tags, lerr, want)
		}
	}
}

// TestLagDeleteAgain checks that a push of a manifest deleted ends the lag of its delete, and
// that Serve ends every lag of a delete.
func TestLagDeleteAgain(t *testing.T) {
	ctx := t.Context()
	l := &Lag{Fake: &Fake{}, Deletes: -1}
	desc := lagged(t, l, name, "1.0.0")
	rr, err := l.Repository(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := rr.Delete(ctx, desc); err != nil {
		t.Fatal(err)
	}
	// The same manifest again: what the registry serves is the push, so once the repository is
	// gone, unlagged, nothing of the delete is served either.
	lagged(t, l, name, "1.0.0")
	if err := l.DeleteRepository(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(ctx, "1.0.0"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Resolve of a tag pushed again after its delete: %v, want what the Fake answers", err)
	}
	lagged(t, l, name, "1.0.0")
	if err := rr.Delete(ctx, desc); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.Resolve(ctx, "1.0.0"); err != nil {
		t.Errorf("Resolve of the tag deleted with Deletes -1: %v", err)
	}
	l.Serve()
	if _, err := rr.Resolve(ctx, "1.0.0"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("Resolve of the tag deleted after Serve: %v, want ErrNotFound", err)
	}
}

// TestLagStatManifest checks that StatManifest misses a manifest pushed as the other reads by
// its digest do, with MANIFEST_UNKNOWN, as many times as Manifests says.
func TestLagStatManifest(t *testing.T) {
	ctx := t.Context()
	l := &Lag{Fake: &Fake{}, Manifests: 1}
	desc := lagged(t, l, name, "1.0.0")
	if _, err := l.StatManifest(ctx, name, desc.Digest); !errors.Is(err, provider.ErrManifestUnknown) ||
		!errors.Is(err, provider.ErrNotFound) {
		t.Errorf("StatManifest after the push: %v, want ErrManifestUnknown and ErrNotFound", err)
	}
	if got, err := l.StatManifest(ctx, name, desc.Digest); err != nil || got.Digest != desc.Digest {
		t.Errorf("StatManifest the second time = %v, %v", got, err)
	}
}
