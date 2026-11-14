package server

import (
	"bytes"
	"context"
)

// RepoHost is the repo host a request came on in the FQDN mode (docs/spec/http-routing.md §1.1):
// the server rewrote the request to the path form of the repo, and a handler writes the absolute
// URLs it emits for the repo under URL rather than under PathURL (ClientLinks).
type RepoHost struct {
	// Format and Repo name the repo.
	Format, Repo string
	// URL is the root of the repo as the clients of its host address it,
	// <scheme>://<repo host>[:<port>]/, with the scheme and the port of server.externalURL.
	URL string
	// PathURL is the root of the repo in the path form, <server.externalURL>/<format>/<repo>/.
	PathURL string
}

// repoHostKey is the context key of the RepoHost of a request.
type repoHostKey struct{}

// WithRepoHost returns a copy of ctx that carries h: the middleware of the FQDN mode calls it, and
// the tests of a plugin call it to serve a request as if it came on a repo host.
func WithRepoHost(ctx context.Context, h RepoHost) context.Context {
	return context.WithValue(ctx, repoHostKey{}, h)
}

// RepoHostFrom returns the repo host the request of ctx came on; ok is false on the main host.
func RepoHostFrom(ctx context.Context) (h RepoHost, ok bool) {
	h, ok = ctx.Value(repoHostKey{}).(RepoHost)
	return h, ok
}

// ClientLinks returns content with every link that follows anchor and starts with the PathURL of
// the repo host of ctx written under its URL instead: npm's `"tarball":"` links, rendered under
// server.externalURL, as a client of the repo host must see them. Content is returned as it is
// when the request came on the main host. Links of another repo do not start with the PathURL
// and stay as they are.
func ClientLinks(ctx context.Context, content []byte, anchor string) []byte {
	h, ok := RepoHostFrom(ctx)
	if !ok || h.PathURL == "" {
		return content
	}
	return bytes.ReplaceAll(content, []byte(anchor+h.PathURL), []byte(anchor+h.URL))
}
