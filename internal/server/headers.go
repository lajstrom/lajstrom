package server

import "net/http"

// contentSecurityPolicy is the policy of every answer of the main listener. Lajstrom answers
// JSON, plain text, package indexes and files, and none of them is a page that needs to run or
// load anything: the policy allows nothing, so that a file a writer uploaded with the type
// text/html or image/svg+xml runs no script, loads nothing, submits no form and is framed by
// no page when a browser opens it from the registry's own origin (docs/security/threat-model.md).
// The links of a PyPI simple page still work: navigation is not governed by the policy.
const contentSecurityPolicy = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// setSecurityHeaders sets the headers every answer of the main listener carries (#63):
// X-Content-Type-Options, so that a browser never sniffs a type other than the one sent; the
// Content-Security-Policy above; and Referrer-Policy, so that a link followed from an index
// does not send the registry's URL elsewhere. There is no Server header: net/http writes none,
// and no handler sets one.
func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("Referrer-Policy", "no-referrer")
}

// crossOrigin refuses a request that changes something (any method but GET, HEAD and OPTIONS)
// when a browser sends it from another origin, before its credentials are checked (#63). A
// browser that has the registry's HTTP Basic credentials cached sends them with a form that
// another site submits to it, and a form can POST a body of text/plain that the management
// API decodes as JSON: without this, a page an admin visits could create a user. net/http's
// CrossOriginProtection tells such a request by Sec-Fetch-Site, or by an Origin whose host is
// not the request's; a request with neither, which is what curl, pip, twine, Maven, apt,
// docker and oras send, passes. The refusal is a 403 in the route's error format.
func (s *Server) crossOrigin(next http.Handler) http.Handler {
	c := http.NewCrossOriginProtection()
	c.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeRefusal(w, r, http.StatusForbidden, "cross-origin request refused: a browser may not send it from another site")
	}))
	return c.Handler(next)
}

// securityHeaders sets the security headers before the request is handled, so that every
// answer has them, the 401 of authenticate, the 503 of the upload limit and a 404 of the
// ServeMux included; the 500 of recovery sets them again after it drops the handler's headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}
