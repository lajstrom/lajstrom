package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Invalidate invalidates, on w, the rendered documents (index_cache) of every repo mask matches,
// and, when mask names a group exactly, of the group's members (docs/spec/policy.md §6.1): what
// the creation or the deletion of a rule of mask, or the rows of a verdict of scope mask, may
// change. A mask that names one repo reads that repo only.
func Invalidate(ctx context.Context, w index.Writer, mask string) error {
	format, name, err := auth.ParseMask(mask)
	if err != nil {
		return fmt.Errorf("policy: invalidate %q: %w", mask, err)
	}
	var repos []index.Repo
	if !strings.Contains(mask, "*") {
		r, err := w.GetRepo(ctx, format, name)
		switch {
		case err == nil:
			repos = []index.Repo{*r}
		case !isNotFound(err):
			return err
		}
	} else {
		all, err := w.ListRepos(ctx, "")
		if err != nil {
			return err
		}
		for _, r := range all {
			if auth.MaskMatches(mask, r.Format, r.Name) {
				repos = append(repos, r)
			}
		}
	}
	for _, r := range repos {
		if r.Kind == index.KindGroup && !strings.Contains(mask, "*") {
			members, err := w.ListGroupMembers(ctx, r.ID)
			if err != nil {
				return err
			}
			repos = append(repos, members...)
			continue
		}
		if err := w.InvalidateIndexCache(ctx, r.ID, ""); err != nil {
			return err
		}
	}
	return nil
}

// isNotFound reports whether err is the index's not found.
func isNotFound(err error) bool { return errors.Is(err, index.ErrNotFound) }
