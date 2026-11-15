package terraform

import (
	"context"
	"net/http"
	"net/url"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/oci/profile"
)

// AccessType is the type of the one access entry of the token of an archive URL
// (docs/spec/rbac.md §4): /v2 grants nothing for it.
const AccessType = "terraform-archive"

// tokenParam is the query parameter of an archive URL that holds its token.
const tokenParam = "token"

// ArchiveTokens issue and verify the tokens of archive URLs (docs/spec/rbac.md §4, ADR 0023):
// bearer tokens signed with the token key of the facade, whose one access entry, of type
// AccessType with the action pull, names the OCI reference of a module version,
// terraform/<repo>/<package path>:<tag>. serve passes an implementation over the facade's token
// keys; without one the download writes the archive's path alone.
type ArchiveTokens interface {
	// Issue returns a token for p that downloads ref, for the service of the request r.
	Issue(ctx context.Context, r *http.Request, p auth.Principal, ref string) (string, error)
	// Allows reports whether raw is a token for the service of the request r that downloads
	// ref: false for one that does not verify, has expired or names another reference. An error
	// is a failure to load the keys.
	Allows(ctx context.Context, r *http.Request, raw, ref string) (bool, error)
}

// WithArchiveTokens returns the plugin with t, whose routes sign the archive URLs of their
// downloads with it and take its tokens on the archive route.
func (p Plugin) WithArchiveTokens(t ArchiveTokens) format.Plugin {
	p.Tokens = t
	return p
}

// archiveRef returns the OCI reference of version v of m in the repo, as the facade names it,
// which the token of its archive URL names: terraform/<repo>/<package path>:<tag>. It is computed
// from the path of a request alone, so that the archive route checks a token before it looks
// the version up.
func archiveRef(repo index.Repo, m Module, v string) string {
	return Name + "/" + repo.Name + "/" + m.packagePath() + ":" + profile.Tag(v, "")
}

// signed returns loc with the token of an archive URL for the principal of r, which downloads
// version v of m: loc as it is for the anonymous principal, which has no credentials to carry
// over, and without ArchiveTokens.
func (h *handlers) signed(r *http.Request, repo index.Repo, m Module, v, loc string) (string, error) {
	p := auth.PrincipalFrom(r.Context())
	if h.tokens == nil || p.Kind == auth.KindAnonymous {
		return loc, nil
	}
	raw, err := h.tokens.Issue(r.Context(), r, p, archiveRef(repo, m, v))
	if err != nil {
		return "", err
	}
	return loc + "?" + tokenParam + "=" + url.QueryEscape(raw), nil
}

// authorizeArchive decides whether the archive of version v of m may be served: read on the repo
// for the principal of the request, else the token of the query when it downloads that version.
// A token that does not is none: the answer is the denial of read (docs/spec/rbac.md §3), so
// that it tells nothing of the repo.
func (h *handlers) authorizeArchive(r *http.Request, repo index.Repo, m Module, v string) error {
	denied := format.Authorize(r.Context(), h.deps, auth.ActionRead, repo)
	raw := r.URL.Query().Get(tokenParam)
	if denied == nil || raw == "" || h.tokens == nil {
		return denied
	}
	ok, err := h.tokens.Allows(r.Context(), r, raw, archiveRef(repo, m, v))
	switch {
	case err != nil:
		return err
	case !ok:
		return denied
	}
	return nil
}
