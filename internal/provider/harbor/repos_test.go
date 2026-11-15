package harbor

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lajstrom/lajstrom/internal/provider"
)

func TestListRepositories(t *testing.T) {
	f := newFakeHarbor(t)
	f.set(func(f *fakeHarbor) {
		f.repos["lajstrom"] = []string{
			"lajstrom/team/raw/scratch/b", "lajstrom/team/raw/scratch/a", "lajstrom/team/raw/assets/x",
			"lajstrom/team/pypi/internal/requests", "lajstrom/team/raw/scratch/c/d",
			"lajstrom/other/raw/scratch/a",             // below the project, not below the prefix
			"lajstrom/old/lajstrom/team/raw/scratch/e", // holds the prefix, does not start with it
			"lajstrom/team/raw/scratchpad/f",           // another repo
		}
	})
	p := f.provider(f.config("lajstrom/team/"), newClock().now)
	p.pageSize = 2
	got, err := p.ListRepositories(t.Context(), "raw/scratch/")
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if want := []string{"raw/scratch/a", "raw/scratch/b", "raw/scratch/c/d"}; !slices.Equal(got, want) {
		t.Errorf("ListRepositories = %q, want %q", got, want)
	}
	f.mu.Lock()
	uris := slices.Clone(f.uris)
	f.mu.Unlock()
	if len(uris) != 2 || !strings.Contains(uris[0], "q=name%3D~lajstrom%2Fteam%2Fraw%2Fscratch%2F") ||
		!strings.Contains(uris[0], "sort=name") || !strings.Contains(uris[1], "page=2") {
		t.Errorf("requests %q", uris)
	}

	got, err = p.ListRepositories(t.Context(), "")
	if err != nil || len(got) != 6 || got[0] != "pypi/internal/requests" {
		t.Errorf("ListRepositories of everything = %q, %v", got, err)
	}

	// The whole project: no filter.
	all := f.provider(f.config("lajstrom/"), newClock().now)
	got, err = all.ListRepositories(t.Context(), "")
	if err != nil || len(got) != 8 {
		t.Errorf("ListRepositories of the project = %q, %v", got, err)
	}
	f.mu.Lock()
	last := f.uris[len(f.uris)-1]
	f.mu.Unlock()
	if strings.Contains(last, "q=") {
		t.Errorf("the listing of the project asked %s", last)
	}

	// maxPages bounds the walk: the first page, the name of another prefix and one of this.
	p.maxPages = 1
	got, err = p.ListRepositories(t.Context(), "raw/scratch/")
	if err != nil || !slices.Equal(got, []string{"raw/scratch/a"}) {
		t.Errorf("ListRepositories of one page = %q, %v", got, err)
	}
}

func TestListRepositoriesUnsupported(t *testing.T) {
	no := false
	f := newFakeHarbor(t)
	cfg := f.config("lajstrom/")
	cfg.Capabilities.Catalog = &no
	if _, err := f.provider(cfg, newClock().now).ListRepositories(t.Context(), "raw/"); !errors.Is(err, provider.ErrUnsupported) {
		t.Errorf("ListRepositories with catalog false: %v", err)
	}
	if n := f.count(routeList); n != 0 {
		t.Errorf("catalog false asked Harbor %d times", n)
	}

	// The account lacks repository:list: unsupported, and the message says what it needs.
	f.set(func(f *fakeHarbor) { f.mayList = false })
	_, err := f.provider(f.config("lajstrom/"), newClock().now).ListRepositories(t.Context(), "raw/")
	if !errors.Is(err, provider.ErrUnsupported) || !strings.Contains(err.Error(), "repository:list") || strings.Contains(err.Error(), "lajstrom/") {
		t.Errorf("ListRepositories without repository:list: %v", err)
	}

	// Harbor fails: an error, not unsupported, so that reindex does not take oci_repos for the listing.
	f.set(func(f *fakeHarbor) { f.mayList, f.fail[routeList] = true, []int{500, 500, 500, 500} })
	_, err = f.provider(f.config("lajstrom/"), newClock().now).ListRepositories(t.Context(), "raw/")
	if err == nil || errors.Is(err, provider.ErrUnsupported) {
		t.Errorf("ListRepositories on a 500: %v", err)
	}
}

func TestDeleteRepository(t *testing.T) {
	f := newFakeHarbor(t)
	f.set(func(f *fakeHarbor) {
		f.repos["lajstrom"] = []string{"lajstrom/team/raw/scratch/a", "lajstrom/team/raw/scratch/held"}
		f.immutable["lajstrom/team/raw/scratch/held"] = true
	})
	p := f.provider(f.config("lajstrom/team/"), newClock().now)
	if err := p.DeleteRepository(t.Context(), "raw/scratch/a"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	f.mu.Lock()
	last := f.uris[len(f.uris)-1]
	left := slices.Clone(f.repos["lajstrom"])
	f.mu.Unlock()
	if !strings.HasSuffix(last, "/repositories/team%252Fraw%252Fscratch%252Fa") || slices.Contains(left, "lajstrom/team/raw/scratch/a") {
		t.Errorf("DELETE %s; left %q", last, left)
	}

	tests := []struct {
		name, repo string
		want       error
	}{
		{"gone", "raw/scratch/a", provider.ErrNotFound},
		{"an immutable tag", "raw/scratch/held", provider.ErrConflict},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.DeleteRepository(t.Context(), tt.repo)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) || strings.Contains(err.Error(), "configured as immutable") {
				t.Errorf("DeleteRepository: %v; want %v", err, tt.want)
			}
		})
	}

	f.set(func(f *fakeHarbor) { f.mayDelete = false })
	err := p.DeleteRepository(t.Context(), "raw/scratch/held")
	if !errors.Is(err, provider.ErrUnauthorized) || !strings.Contains(err.Error(), "repository:delete") {
		t.Errorf("DeleteRepository without repository:delete: %v", err)
	}
}
