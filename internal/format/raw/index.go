package raw

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

const (
	// kindListing is the index_cache kind of the JSON listing.
	kindListing = "listing"
	// listingContentType is the Content-Type of the JSON listing.
	listingContentType = "application/json"
)

// listing is the JSON listing of a directory prefix (docs/spec/http-routing.md §3, raw).
type listing struct {
	Entries []listingEntry `json:"entries"`
}

// listingEntry is one version of one path in a listing.
type listingEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest"`  // the manifest digest, the value ?digest= takes
	Created string `json:"created"` // RFC 3339 UTC
}

// RenderIndex renders the listing of scope, a directory prefix that is empty (the root) or ends
// with "/": one entry per version of every path that starts with the prefix, at any depth, with
// the file size, the manifest digest and created, sorted by path and then created, as one
// compact JSON document of kind "listing". A prefix below the root that holds no path is
// index.ErrNotFound and renders nothing, so that requests for prefixes that do not exist add no
// index_cache rows; the root always renders, {"entries":[]} when the repo is empty.
func (Plugin) RenderIndex(ctx context.Context, repo index.Repo, scope format.IndexScope, st index.Reader) ([]format.Document, error) {
	prefix := string(scope)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		return nil, fmt.Errorf("%w: raw listing scope %q is not a directory prefix", index.ErrNotFound, prefix)
	}
	pkgs, err := st.ListPackages(ctx, repo.ID, prefix)
	if err != nil {
		return nil, fmt.Errorf("raw: list %s/%s %q: %w", repo.Format, repo.Name, prefix, err)
	}
	if len(pkgs) == 0 && prefix != "" {
		return nil, fmt.Errorf("%w: raw/%s has no path below %q", index.ErrNotFound, repo.Name, prefix)
	}
	entries := []listingEntry{}
	for _, p := range pkgs {
		e, err := packageEntries(ctx, st, p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e...)
	}
	slices.SortStableFunc(entries, func(a, b listingEntry) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), strings.Compare(a.Created, b.Created))
	})
	content, err := json.Marshal(listing{Entries: entries})
	if err != nil {
		return nil, fmt.Errorf("raw: encode the listing of %q: %w", prefix, err)
	}
	return []format.Document{{Kind: kindListing, Path: prefix, ContentType: listingContentType, Content: content}}, nil
}

// packageEntries returns the listing entries of the versions of p, oldest first. A version
// without exactly one file is not a raw version and is left out.
func packageEntries(ctx context.Context, st index.Reader, p index.Package) ([]listingEntry, error) {
	versions, err := st.ListVersions(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("raw: versions of %q: %w", p.Name, err)
	}
	entries := make([]listingEntry, 0, len(versions))
	for _, v := range versions {
		files, err := st.ListFiles(ctx, v.ID)
		if err != nil {
			return nil, fmt.Errorf("raw: files of %q %s: %w", p.Name, v.Version, err)
		}
		if len(files) != 1 {
			continue
		}
		entries = append(entries, listingEntry{Path: p.Name, Size: files[0].Size, Digest: v.Digest.String(),
			Created: v.CreatedAt.UTC().Format(time.RFC3339)})
	}
	return entries, nil
}
