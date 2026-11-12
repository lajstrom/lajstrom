package facade

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/provider"
	"github.com/lajstrom/lajstrom/internal/provider/providertest"
)

// catalog returns the names the catalog lists to a token that may read every repo, and the
// paths of the oci repo's oci_repos rows.
func (e *env) catalog() (names, rows []string) {
	e.t.Helper()
	bearer := "Bearer " + e.getToken(e.h, "", "registry:catalog:*")
	names = slices.Concat(e.listing(e.h, "/v2/_catalog", "repositories", "Authorization", bearer)...)
	ocis, err := e.st.ListOCIRepos(e.t.Context(), e.oci.ID)
	must(e.t, err)
	rows = []string{}
	for _, o := range ocis {
		rows = append(rows, o.Path)
	}
	return names, rows
}

// wantCatalog fails the test when the catalog and the oci repo's oci_repos do not name want.
func (e *env) wantCatalog(what string, want ...string) {
	e.t.Helper()
	want = append([]string{}, want...)
	if names, rows := e.catalog(); !slices.Equal(names, want) || !slices.Equal(rows, want) {
		e.t.Errorf("%s: the catalog lists %q and oci_repos has %q, want %q", what, names, rows, want)
	}
}

// TestCatalogAfterOCIDeletes (#748): an oci repository whose manifests are all deleted through
// /v2 leaves oci_repos and the catalog, which lists it while one tag is left. The fake keeps the
// emptied repository with an empty tag list, as Zot does.
func TestCatalogAfterOCIDeletes(t *testing.T) {
	e := newEnv(t, envOptions{})
	target := "/v2/oci/images/app/manifests/"
	var digests []digest.Digest
	for _, tag := range []string{"v1", "v2"} {
		body, d := manifest(t, []byte(tag), tag)
		digests = append(digests, d)
		wantStatus(t, "PUT "+tag, e.do(e.h, http.MethodPut, target+tag, bytes.NewReader(body), "Content-Type", ocispecManifest), http.StatusCreated)
	}
	e.wantCatalog("after the pushes", "oci/images/app")
	wantStatus(t, "DELETE v1 by its tag", e.do(e.h, http.MethodDelete, target+"v1", nil), http.StatusAccepted)
	e.wantCatalog("with v2 left", "oci/images/app")
	wantStatus(t, "DELETE v2 by its digest", e.do(e.h, http.MethodDelete, target+digests[1].String(), nil), http.StatusAccepted)
	if pages := e.listing(e.h, "/v2/oci/images/app/tags/list", "tags"); !equalPages(pages, [][]string{{}}) {
		t.Errorf("the backend's tag list of the emptied repository: %q, want one empty page (Zot's)", pages)
	}
	e.wantCatalog("after both deletes")
}

// pushAfterTags is the fake whose repositories call the function in after, once, right after
// the first tag list they read and before they answer with it: a push that lands between a
// caller's tag list and what the caller does next.
type pushAfterTags struct {
	*providertest.Fake
	after *atomic.Pointer[func()]
}

func (f pushAfterTags) Repository(ctx context.Context, name string) (provider.Repository, error) {
	rr, err := f.Fake.Repository(ctx, name)
	return pushAfterTagsRepo{rr, f}, err
}

type pushAfterTagsRepo struct {
	provider.Repository
	f pushAfterTags
}

func (r pushAfterTagsRepo) Tags(ctx context.Context, last string, fn func([]string) error) error {
	var tags []string
	err := r.Repository.Tags(ctx, last, func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if after := r.f.after.Swap(nil); after != nil {
		(*after)()
	}
	if err != nil {
		return err
	}
	return fn(tags)
}

// TestCatalogDeleteRacesPush (#748): a push to an oci repository that lands after the delete of
// its last manifest listed no tag, and so before the delete drops the oci_repos row, is not lost:
// the delete lists the tags again once the row went, finds the pushed tag and records the
// repository again.
func TestCatalogDeleteRacesPush(t *testing.T) {
	after := &atomic.Pointer[func()]{}
	e := newEnv(t, envOptions{registry: func(f *providertest.Fake) provider.Provider { return pushAfterTags{Fake: f, after: after} }})
	target := "/v2/oci/images/app/manifests/"
	first, d := manifest(t, []byte("v1"), "v1")
	wantStatus(t, "PUT v1", e.do(e.h, http.MethodPut, target+"v1", bytes.NewReader(first), "Content-Type", ocispecManifest), http.StatusCreated)
	second, _ := manifest(t, []byte("v2"), "v2")
	push := func() {
		rr := e.do(e.h, http.MethodPut, target+"v2", bytes.NewReader(second), "Content-Type", ocispecManifest)
		wantStatus(t, "PUT v2 after the delete's tag list", rr, http.StatusCreated)
		rows, err := e.st.ListOCIRepos(t.Context(), e.oci.ID)
		if err != nil || len(rows) != 1 {
			t.Errorf("oci_repos after the push: %v, %v; want its row", rows, err)
		}
	}
	after.Store(&push)
	wantStatus(t, "DELETE v1", e.do(e.h, http.MethodDelete, target+d.String(), nil), http.StatusAccepted)
	if after.Load() != nil {
		t.Fatal("the delete listed no tags")
	}
	e.wantCatalog("after the delete and the push", "oci/images/app")
	wantStatus(t, "GET v2", e.do(e.h, http.MethodGet, target+"v2", nil), http.StatusOK)
}
