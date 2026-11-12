package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/proxy"
)

// actionConvert is the audit action of a proxy that became a hosted repo (docs/spec/rbac.md §7).
const actionConvert = "repo.convert"

// convertClaimStale is the age at which a fetch claim no longer counts as a fetch running: the
// stale age of the claims (docs/spec/index-schema.md, Proxy and group repos).
const convertClaimStale = 2 * time.Minute

// maxConvertSkipped bounds the skipped of the audit row of a conversion; the rest is counted.
const maxConvertSkipped = 100

// Journaler writes journal ops through the one write path: the ingest pipeline
// (format.Publisher.Journal).
type Journaler interface {
	Journal(ctx context.Context, repo index.Repo, pkgPath string, ops []format.JournalOp, principal auth.Principal) ([]int64, error)
}

// adoption is what a conversion writes of one package: its journal ops.
type adoption struct {
	pkg index.Package
	ops []format.JournalOp
}

// Convert turns the proxy repo formatName/name into a hosted repo that keeps what it holds
// (docs/spec/proxy-and-group.md §2.6): the plugin turns each held package's kept document into
// journal ops (format.Adopter); one transaction refuses a group of the repo that has a hosted
// member, converts the row (index.Store.ConvertRepo, which refuses a running fetch),
// invalidates the repo's documents and writes the audit row repo.convert; the ops go through j
// by principal; and the repo's proxy_meta rows are dropped. On a hosted repo it resumes a
// conversion that failed after its flip, from the proxy_meta rows left, and changes nothing
// without them. A group, or a format without the hook, wraps format.ErrInvalid.
func (s *Repos) Convert(ctx context.Context, formatName, name string, j Journaler, principal auth.Principal, o Origin) (Repo, error) {
	row, err := s.Index.GetRepo(ctx, formatName, name)
	if err != nil {
		return Repo{}, repoError("convert", formatName, name, err)
	}
	id := formatName + "/" + name
	if row.Kind == index.KindGroup {
		return Repo{}, invalid("kind: %s is a group; only a proxy repo becomes a hosted one", id)
	}
	adopter, ok := s.adopter(formatName)
	if !ok {
		return Repo{}, invalid("kind: format %s cannot turn a proxy repo into a hosted one", formatName)
	}
	plan, skipped, err := s.adoptions(ctx, row, adopter)
	if err != nil {
		return Repo{}, err
	}
	if row.Kind == index.KindProxy {
		if err := s.flip(ctx, row, plan, skipped, o); err != nil {
			return Repo{}, err
		}
	}
	row.Kind, row.Upstream = index.KindHosted, nil
	for _, a := range plan {
		if _, err := j.Journal(ctx, *row, a.pkg.NameOCI, a.ops, principal); err != nil {
			return Repo{}, fmt.Errorf("api: %s is a hosted repo now, but the journal of %s is not written; the same PATCH resumes: %w",
				id, a.pkg.Name, err)
		}
	}
	if err := s.Index.Tx(ctx, func(w index.Writer) error { return dropUpstreamCache(ctx, w, row.ID, "") }); err != nil {
		return Repo{}, fmt.Errorf("api: %s is a hosted repo with its journal written; its cached upstream documents are not dropped: %w", id, err)
	}
	if row, err = s.Index.GetRepoByID(ctx, row.ID); err != nil {
		return Repo{}, repoError("convert", formatName, name, err)
	}
	return resource(ctx, s.Index, row)
}

// adoptions reads the kept document of each package the repo holds and returns the journal ops
// that carry its state over, and what was left out, each prefixed with the package's name.
func (s *Repos) adoptions(ctx context.Context, row *index.Repo, adopter format.Adopter) ([]adoption, []string, error) {
	pkgs, err := s.Index.ListPackages(ctx, row.ID, "")
	if err != nil {
		return nil, nil, err
	}
	var plan []adoption
	var skipped []string
	for _, pkg := range pkgs {
		key, err := adopter.Key(pkg.Name)
		if err != nil {
			continue // a name the proxy could not have cached
		}
		kept, err := s.Index.GetProxyMeta(ctx, row.ID, key, adopter.AdoptKind())
		switch {
		case errors.Is(err, index.ErrNotFound) || (err == nil && kept.State != index.ProxyMetaPresent):
			if row.Kind == index.KindProxy {
				skipped = append(skipped, pkg.Name+": no cached document of the upstream; the hosted repo's own dist-tags")
			}
			continue
		case err != nil:
			return nil, nil, err
		}
		versions, err := s.Index.ListVersions(ctx, pkg.ID)
		if err != nil {
			return nil, nil, err
		}
		held := make([]string, 0, len(versions))
		for _, v := range versions {
			held = append(held, v.Version)
		}
		ops, left, err := adopter.AdoptOps(pkg.Name, kept.Content, held)
		if err != nil {
			skipped = append(skipped, pkg.Name+": "+err.Error())
			continue
		}
		for _, l := range left {
			skipped = append(skipped, pkg.Name+": "+l)
		}
		if len(ops) > 0 {
			plan = append(plan, adoption{pkg: pkg, ops: ops})
		}
	}
	return plan, skipped, nil
}

// flip converts the row in one transaction: a group of the repo that has another hosted member
// is index.ErrConflict naming it; then ConvertRepo, the repo's documents invalidated, and the
// audit row with the counts of plan and what was skipped.
func (s *Repos) flip(ctx context.Context, row *index.Repo, plan []adoption, skipped []string, o Origin) error {
	settings, _ := proxy.ParseSettings(row.Upstream)
	ops := 0
	for _, a := range plan {
		ops += len(a.ops)
	}
	details := map[string]any{"from": index.KindProxy, "to": index.KindHosted, "upstream": settings.Upstream, "ops": ops}
	if len(skipped) > maxConvertSkipped {
		details["skippedMore"] = len(skipped) - maxConvertSkipped
		skipped = skipped[:maxConvertSkipped]
	}
	details["skipped"] = append([]string{}, skipped...)
	var ev index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		groups, err := w.ListGroupsOf(ctx, row.ID)
		if err != nil {
			return err
		}
		for _, g := range groups {
			members, err := w.ListGroupMembers(ctx, g.ID)
			if err != nil {
				return err
			}
			for _, m := range members {
				if m.ID != row.ID && m.Kind == index.KindHosted {
					return &clientError{msg: fmt.Sprintf("kind: %s/%s is a member of the group %s/%s, which has the hosted member %s already",
						row.Format, row.Name, g.Format, g.Name, m.Name), err: index.ErrConflict}
				}
			}
		}
		if err := w.ConvertRepo(ctx, row.ID, s.now().Add(-convertClaimStale)); err != nil {
			return err
		}
		if err := w.InvalidateIndexCache(ctx, row.ID, ""); err != nil {
			return err
		}
		pkgs, err := w.ListPackages(ctx, row.ID, "")
		if err != nil {
			return err
		}
		details["packages"] = len(pkgs)
		ev, err = s.record(ctx, w, actionConvert, row, o, details)
		return err
	})
	var ce *clientError
	switch {
	case err == nil:
	case errors.As(err, &ce):
		return err
	case errors.Is(err, index.ErrConflict): // ConvertRepo: a fetch running, or converted meanwhile
		return &clientError{msg: fmt.Sprintf("kind: %s/%s cannot become hosted now: %s", row.Format, row.Name, err.Error()), err: err}
	default:
		return repoError("convert", row.Format, row.Name, err)
	}
	s.writeAudit(ctx, ev)
	return nil
}

// adopter returns the format's conversion hook, format.Adopter of its plugin.
func (s *Repos) adopter(formatName string) (format.Adopter, bool) {
	px, ok := s.proxier(formatName)
	if !ok {
		return nil, false
	}
	a, ok := px.(format.Adopter)
	return a, ok
}

// convertRepo answers a PATCH whose body is {"kind": "hosted"}: the admin role turns a proxy
// into a hosted repo (Repos.Convert), 200 with the resource. Any other key beside kind, or
// another kind, is 400.
func (h *handler) convertRepo(r *http.Request, f, name string, body patchBody) (int, any, error) {
	if body.AnonymousRead != nil || body.Policy != nil || body.Upstream != nil || body.Group != nil || body.Format != nil ||
		body.Name != nil || body.Registry != nil {
		return 0, nil, invalid("kind: changes alone, in a body of its own")
	}
	if string(body.Kind) != `"hosted"` {
		return 0, nil, invalid("kind: %s; the one change of a kind is a proxy repo to \"hosted\"", body.Kind)
	}
	if err := h.allowAdmin(r); err != nil {
		return 0, nil, err
	}
	if h.journal == nil {
		return 0, nil, &clientError{msg: "converting a proxy repo is not enabled in this server", err: format.ErrUnsupported}
	}
	repo, err := h.repos.Convert(r.Context(), f, name, h.journal, auth.PrincipalFrom(r.Context()), origin(r))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, repo, nil
}
