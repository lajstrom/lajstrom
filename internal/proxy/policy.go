package proxy

import (
	"context"
	"errors"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/policy"
)

// The policy of proxy repos (docs/spec/policy.md §4, §6.1, §6.4): the decision at step 3 of a
// fetch, the versions a proxy's document leaves out, and the clock of a quarantine.

// decideHeld decides the held file m of the proxy repo of p, a download (step 3).
func (c *Core) decideHeld(ctx context.Context, p *proxied, m *index.FileMatch) error {
	s := policy.Subject{Repo: p.repo, Name: m.Package.Name, Version: m.Version.Version, Variant: m.Version.Variant,
		Digest: m.Version.Digest, Published: m.Version.CreatedAt}
	return c.decide(ctx, policy.SurfaceDownload, s)
}

// decideEntry decides the artifact a, which the repo does not hold, on its entry of the name's
// document before it is fetched (step 3): its publish time, else the first time the repo saw
// it, is the quarantine's clock.
func (c *Core) decideEntry(ctx context.Context, p *proxied, a artifact, entry *format.UpstreamEntry, surface string) error {
	if c.o.Policy == nil {
		return nil
	}
	s := policy.Subject{Repo: p.repo, Name: a.ref.Name, Version: a.ref.Version, Variant: a.ref.Variant, Published: entry.Published}
	if s.Published.IsZero() && policy.Quarantine(p.repo) > 0 {
		s.Published = c.seen(ctx, c.o.Index, p, a.key, a.ref.Version, a.ref.Variant)
	}
	return c.decide(ctx, surface, s)
}

// decide decides s, and writes the audit row of a refusal on surface.
func (c *Core) decide(ctx context.Context, surface string, s policy.Subject) error {
	err := c.o.Policy.Check(ctx, s)
	var denied *policy.DeniedError
	if errors.As(err, &denied) {
		c.o.Policy.Refuse(ctx, surface, denied, auth.PrincipalFrom(ctx))
	}
	return err
}

// seen returns the first time the repo of p saw the version in its upstream's document, the
// policy_seen row it writes when there is none (docs/spec/policy.md §4); zero when the row
// cannot be written, which makes the clock now.
func (c *Core) seen(ctx context.Context, w index.Writer, p *proxied, key, version, variant string) (t time.Time) {
	row := index.PolicySeen{RepoID: p.repo.ID, Name: key, Version: version, Variant: variant, SeenAt: c.o.Now()}
	if err := w.PutPolicySeen(ctx, &row); err != nil {
		c.o.Log.WarnContext(ctx, "proxy: the first sight of a version not recorded", "repo", p.id(), "name", key,
			"version", version, "error", err.Error())
		return time.Time{}
	}
	return row.SeenAt
}

// trim returns kept, the document of kind of name, without the versions view refuses, when the
// plugin of p trims documents (format.Trimmer); kept as it is otherwise. A version the repo holds
// is decided on its row (its digest and created_at), another on its entry of kept, whose clock is
// its publish time, else the policy_seen row that w writes for it.
func (c *Core) trim(ctx context.Context, w index.Writer, p *proxied, view *policy.View, name, key, kind string, kept []byte) ([]byte, error) {
	t, ok := p.hooks.(format.Trimmer)
	if view == nil || kept == nil || !ok {
		return kept, nil
	}
	parsed, err := p.hooks.ParseMetadata(name, kind, kept)
	if err != nil {
		return nil, err
	}
	held, err := heldVersions(ctx, w, p, name)
	if err != nil {
		return nil, err
	}
	published := map[[2]string]time.Time{}
	for _, e := range parsed.Entries {
		published[[2]string{e.Version, e.Variant}] = e.Published
	}
	quarantine := policy.Quarantine(p.repo) > 0
	return t.Trim(name, kind, kept, func(version, variant string) bool {
		s := policy.Subject{Repo: p.repo, Name: name, Version: version, Variant: variant}
		if v, ok := held[[2]string{version, variant}]; ok {
			s.Digest, s.Published = v.Digest, v.CreatedAt
		} else {
			s.Published = published[[2]string{version, variant}]
			if s.Published.IsZero() && quarantine {
				s.Published = c.seen(ctx, w, p, key, version, variant)
			}
		}
		return !view.Decide(s).Allowed()
	})
}

// heldVersions returns the versions of name the repo of p holds, by version and variant.
func heldVersions(ctx context.Context, r index.Reader, p *proxied, name string) (map[[2]string]index.Version, error) {
	out := map[[2]string]index.Version{}
	pkgPath, err := p.plugin.EscapeName(name)
	if err != nil {
		return out, nil //nolint:nilerr // a name the format cannot escape is not held
	}
	pkg, err := index.PackageAt(ctx, r, p.repo.ID, pkgPath)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return out, nil
	case err != nil:
		return nil, err
	}
	vs, err := r.ListVersions(ctx, pkg.ID)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		out[[2]string{v.Version, v.Variant}] = v
	}
	return out, nil
}
