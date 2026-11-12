package generic

import (
	"context"
	"net/http"
	"strings"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// catalogClient sends the catalog requests of the probe and of ListRepositories. oras-go's auth
// client sends the credentials only after a challenge, and a registry that lets anonymous
// callers read its catalog never challenges it: Quay answers an anonymous _catalog with 200 and
// its public repositories, none of Lajstrom's (#802). So when the registry has credentials and
// a catalog request that went out without them is answered 200, catalogClient sends it again
// with them (catalogAuth). A registry that challenges the catalog, and one whose Basic scheme
// the token cache knows, is asked once, as before.
type catalogClient struct{ p *Provider }

// Do implements remote.Client.
func (c catalogClient) Do(req *http.Request) (*http.Response, error) {
	p := c.p
	resp, err := p.clients.follow.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK || !p.clients.credentials || sentCredentials(resp) {
		return resp, err
	}
	ctx, ok, err := p.catalogAuth(req.Context())
	switch {
	case err != nil:
		discard(resp)
		return nil, err
	case !ok:
		return resp, nil
	}
	discard(resp)
	p.log.DebugContext(ctx, "the catalog answered a request without credentials; asking again with them")
	return p.clients.follow.Do(req.Clone(ctx))
}

// catalogAuth readies the auth client to send the credentials with a catalog request of ctx: it
// sends GET /v2/, which a registry with credentials challenges, and returns the context of the
// request again, false when the registry did not challenge (the credentials are not needed). For
// Basic, the token cache now holds the scheme and the auth client sends it unasked. For a bearer
// token, the context asks for the scopes of the token that /v2/'s challenge got, usually none:
// Quay refuses registry:catalog:* at its token endpoint (400) and lists the catalog for the
// token asked without a scope; a registry that wants registry:catalog:* challenges the request
// with it, and the auth client answers that. The token comes from the realm that realmTransport
// allowed for /v2/, and is sent to the registry only.
func (p *Provider) catalogAuth(ctx context.Context) (context.Context, bool, error) {
	id, err := p.ping(auth.WithScopesForHost(ctx, p.host))
	if err != nil {
		return nil, false, err
	}
	switch scheme, _ := p.clients.cache.GetScheme(ctx, p.host); scheme {
	case auth.SchemeBasic:
		return ctx, true, nil
	case auth.SchemeBearer:
		_, scope := challengeParam(id.challenge, "scope")
		return auth.WithScopesForHost(ctx, p.host, strings.Fields(scope)...), true, nil
	case auth.SchemeUnknown:
		// No scheme in the cache: the registry never challenged, and wants no credentials.
	}
	return nil, false, nil
}

// sentCredentials reports whether the request that got resp carried credentials; true when it
// cannot tell.
func sentCredentials(resp *http.Response) bool {
	return resp.Request == nil || resp.Request.Header.Get("Authorization") != ""
}

// catalogRegistry is the registry of p with catalogClient for its client, for the catalog walk.
func (p *Provider) catalogRegistry() *remote.Registry {
	reg := &remote.Registry{RepositoryListPageSize: p.registry.RepositoryListPageSize}
	reg.Reference = p.registry.Reference
	reg.PlainHTTP = p.registry.PlainHTTP
	reg.Client = catalogClient{p: p}
	return reg
}
