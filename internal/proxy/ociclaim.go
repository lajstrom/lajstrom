package proxy

import "context"

// ociClaim takes the fetch claim of ref, a digest, at p's path (§5, step 6, with the key of
// §11.4): the claim's package is the path and its tag the reference. It is Core.claim: while
// another replica holds a fresh claim the request waits, asking done, and shared is true when
// done reports that the other replica's fetch is there. The claim taken beats until release,
// which removes it; a claim that another replica took over is logged, and the fetch goes on:
// the backend takes the second copy of the same bytes.
func (c *Core) ociClaim(ctx context.Context, p *ociProxied, ref string, done func(context.Context) (bool, error)) (release func(), shared bool, err error) {
	return c.ociClaimAt(ctx, p, p.path, ref, done)
}

// ociClaimAt is ociClaim of the claim (repo, pkg, ref): "" for pkg is the repo's claim of a
// digest, which the completions at every path of the repo take (ociCompleteFetch).
func (c *Core) ociClaimAt(ctx context.Context, p *ociProxied, pkg, ref string, done func(context.Context) (bool, error)) (release func(), shared bool, err error) {
	return c.claim(ctx, fetchClaim{repoID: p.repo.ID, pkg: pkg, tag: ref, repo: p.id(), what: p.path + "@" + ref,
		attrs: []any{"path", p.path, "ref", ref}, done: done})
}
