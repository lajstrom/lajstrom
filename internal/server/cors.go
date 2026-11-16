package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// The headers of the CORS answers to an origin of server.corsOrigins (docs/spec/http-routing.md
// §4, #949). A web UI sends the DPoP proof of its sign-in (RFC 9449) on every request, reads why
// a proof was refused in WWW-Authenticate (RFC 9449 §7.1), and sets the iat of its proofs by the
// server's clock in Date when its own is off. There is never Access-Control-Allow-Credentials: a
// web UI sends its token with credentials "omit", and no page of another origin reads an answer
// that cached Basic credentials made.
const (
	corsAllowMethods  = "GET, HEAD, POST, PUT, PATCH, DELETE"
	corsAllowHeaders  = "Authorization, Content-Type, DPoP"
	corsExposeHeaders = "Date, Retry-After, WWW-Authenticate, " + requestIDHeader
	// corsMaxAge is how long a browser keeps a preflight, in seconds: WebKit keeps none longer.
	corsMaxAge = "600"
)

// corsPath reports whether path answers CORS to a listed origin: every path under /api/, and
// /-/version. The frontends, /v2, /-/oidc/ and the other system endpoints do not: a link to a
// file or to the login is a navigation.
func corsPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || path == "/-/version"
}

// onMainHost reports whether the request of ctx came on the main host: not on a repo host of
// the FQDN mode, nor on a host below its domain that names no repo.
func onMainHost(ctx context.Context) bool {
	_, repoHost := RepoHostFrom(ctx)
	_, refused := ctx.Value(hostRefusalKey{}).(hostRefusal)
	return !repoHost && !refused
}

// cors answers CORS to the origins of Options.CORSOrigins on corsPath of the main host. Every
// answer of those paths gets Vary: Origin. A request whose Origin is listed gets
// Access-Control-Allow-Origin with it and Access-Control-Expose-Headers, set before the next
// handler runs, so that the 401, 403 and 429 of the middleware have them too; its preflight, an
// OPTIONS with Access-Control-Request-Method, is answered 204 here, before authentication,
// whatever the path and the method it asks for. An origin that is not listed gets nothing, and
// its OPTIONS goes on to the route, which answers 404. Without origins it adds nothing.
func (s *Server) cors(next http.Handler) http.Handler {
	if len(s.opts.CORSOrigins) == 0 {
		return next
	}
	listed := make(map[string]bool, len(s.opts.CORSOrigins))
	for _, o := range s.opts.CORSOrigins {
		listed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !corsPath(r.URL.Path) || !onMainHost(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if !listed[origin] {
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		next.ServeHTTP(w, r)
	})
}

// listedProtection returns the CrossOriginProtection of /api/ when there are CORSOrigins: deny
// answers what it refuses, and each listed origin is trusted, as the server's own origin is. It
// returns nil without origins, and an error naming an origin that AddTrustedOrigin refuses.
func listedProtection(origins []string, deny http.Handler) (*http.CrossOriginProtection, error) {
	if len(origins) == 0 {
		return nil, nil //nolint:nilnil // no origins is not an error
	}
	c := http.NewCrossOriginProtection()
	c.SetDenyHandler(deny)
	for _, o := range origins {
		if err := c.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("server: CORSOrigins: %w", err)
		}
	}
	return c, nil
}
