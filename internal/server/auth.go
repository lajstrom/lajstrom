package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// basicChallenge is the WWW-Authenticate header of a 401 outside /v2/, and on /v2/token: clients
// answer it with HTTP Basic credentials (docs/spec/rbac.md §1 and §3), the challenge of
// format.WriteError.
const basicChallenge = `Basic realm="lajstrom"`

// tokenPath is the token endpoint of the facade, the one path under /v2/ that takes Basic.
const tokenPath = "/v2/token"

// Messages of the answers of the authentication middleware.
const (
	msgBadCredentials  = "credentials are needed: the ones sent were refused"
	msgAuthFailed      = "Internal Server Error"
	msgAuthUnavailable = "Service Unavailable"
	msgThrottled       = "too many failed logins: try again after Retry-After seconds"
	msgTooManyRequests = "too many token requests: try again after Retry-After seconds"
)

// principalHolder carries the principal the authentication middleware settled on back to the
// access log, which runs outside it and sees only its own request.
type principalHolder struct{ p auth.Principal }

type principalHolderKey struct{}

// withPrincipalHolder returns a copy of ctx with a new holder, and the holder.
func withPrincipalHolder(ctx context.Context) (context.Context, *principalHolder) {
	h := &principalHolder{p: auth.Anonymous}
	return context.WithValue(ctx, principalHolderKey{}, h), h
}

// HoldPrincipal records p as the principal of the request of ctx for its access log line, when
// ctx has the holder of the access log. The authentication middleware records the principal of
// HTTP Basic credentials; HoldPrincipal exists for the OCI facade, which verifies the bearer
// tokens of /v2 itself (issue #26) and records their principal through it.
func HoldPrincipal(ctx context.Context, p auth.Principal) {
	if h, ok := ctx.Value(principalHolderKey{}).(*principalHolder); ok {
		h.p = p
	}
}

// authenticate reads the request's credentials and puts its principal and the credential it
// authenticated with in the context (auth.WithPrincipal, auth.WithCredential), for the
// authorizers and the audit log. Every route goes through it, the system endpoints included:
// they simply allow the anonymous principal.
//
//   - No Authorization header: the request goes on as auth.Anonymous. What anonymous may do is
//     the authorizers' to decide: read where anonymous read is on, nothing else (rbac.md §3).
//   - Basic (RFC 7617; the first colon splits the user name from the secret): the credentials
//     are authenticated (a password, user:<personal access token> or deploy:<deploy token>),
//     and the request goes on as the user or the deploy token. Credentials that are refused,
//     or that do not decode, are answered at once with 401 and the Basic challenge, whatever
//     the route, in the error format of the route's family (writeRefusal).
//   - Bearer with a token secret (ljs_pat_… or ljs_dep_…, as npm's _authToken sends it) on
//     every route but /v2: authenticated as a personal access token or a deploy token
//     (auth.Authenticator.AuthenticateBearer), and refused like Basic credentials.
//   - The OIDC login (oidcPrefix, docs/spec/http-routing.md §7): the request goes on as
//     anonymous with the header untouched; its routes take no credential.
//   - The legacy login of npm (npmLogin): the request goes on as anonymous with the header
//     untouched, whatever it says. Its body carries the password, which the npm plugin checks,
//     and npm sends with it the token it had, revoked or not (docs/spec/http-routing.md, npm).
//   - Any other bearer, a bearer on /v2 and any other scheme: the request goes on as anonymous
//     with the header untouched. The OCI facade verifies the bearer of /v2, a JWT, and records
//     its principal (HoldPrincipal).
//
// Every request also gets an evaluation scope (auth.WithEvaluationScope): the RBAC evaluator
// reads the index's auth_version once per request, however many repos the request checks. A
// request to /v2/token is counted against its client address first (tokenThrottled).
//
// A nil Options.Authenticator authenticates nobody: every request is anonymous, as in the unit
// tests of other packages.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.opts.Authenticator == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(auth.WithEvaluationScope(r.Context()))
		if s.tokenThrottled(w, r) {
			return
		}
		if r.Method == http.MethodPut && npmLogin.MatchString(r.URL.Path) || strings.HasPrefix(r.URL.Path, oidcPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		scheme, value, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		var (
			p    auth.Principal
			cred auth.Credential
			err  error
		)
		switch {
		case strings.EqualFold(scheme, "Basic"):
			user, secret, ok := r.BasicAuth()
			if !ok {
				s.log.DebugContext(r.Context(), "authentication refused", "reason", "credentials that do not decode",
					"requestId", RequestID(r.Context()))
				s.writeRefusal(w, r, http.StatusUnauthorized, msgBadCredentials)
				return
			}
			p, cred, err = s.opts.Authenticator.Authenticate(r.Context(), user, secret)
		case strings.EqualFold(scheme, "Bearer") && tokenBearer(r.URL.Path, strings.TrimSpace(value)):
			p, cred, err = s.opts.Authenticator.AuthenticateBearer(r.Context(), strings.TrimSpace(value))
		default:
			next.ServeHTTP(w, r)
			return
		}
		var throttled *auth.ThrottledError
		switch {
		case errors.Is(err, auth.ErrBadCredentials):
			s.writeRefusal(w, r, http.StatusUnauthorized, msgBadCredentials)
			return
		case errors.As(err, &throttled): // a failed-login throttle: account or address
			s.throttled(w, r, throttled, msgThrottled)
			return
		case err != nil:
			// Not the user name: as it was typed, it may be a password or a token (threat model L-3).
			s.log.ErrorContext(r.Context(), "authentication failed", "error", err.Error(), "requestId", RequestID(r.Context()))
			status, msg := http.StatusInternalServerError, msgAuthFailed
			if format.StatusOf(err) == http.StatusServiceUnavailable { // the database does not answer
				status, msg = http.StatusServiceUnavailable, msgAuthUnavailable
				format.SetRetryAfter(w.Header(), err)
			}
			s.writeRefusal(w, r, status, msg)
			return
		}
		HoldPrincipal(r.Context(), p)
		next.ServeHTTP(w, r.WithContext(auth.WithCredential(auth.WithPrincipal(r.Context(), p), cred)))
	})
}

// oidcPrefix is the prefix of the routes of the OIDC login.
const oidcPrefix = "/-/oidc/"

// npmLogin is the path of npm's legacy login, PUT /npm/<repo>/-/user/org.couchdb.user:<name>.
var npmLogin = regexp.MustCompile(`^/npm/[^/]+/-/user/org\.couchdb\.user:[^/]+$`)

// tokenBearer reports whether the bearer secret on path is the authenticator's: a token secret
// (auth.PATPrefix or auth.DeployPrefix) outside /v2, whose bearer is the facade's JWT.
func tokenBearer(path, secret string) bool {
	if path == "/v2" || strings.HasPrefix(path, "/v2/") {
		return false
	}
	return strings.HasPrefix(secret, auth.PATPrefix) || strings.HasPrefix(secret, auth.DeployPrefix)
}

// tokenThrottled answers 429 and reports true when r is a request to the token endpoint from a
// client address that made too many (auth.Authenticator.AllowTokenRequest). It runs before
// the credentials are checked, so that a flood of token requests costs no argon2id.
func (s *Server) tokenThrottled(w http.ResponseWriter, r *http.Request) bool {
	var te *auth.ThrottledError
	if r.URL.Path != tokenPath || !errors.As(s.opts.Authenticator.AllowTokenRequest(r.Context()), &te) {
		return false
	}
	s.throttled(w, r, te, msgTooManyRequests)
	return true
}

// throttled answers a request that a throttle refused: 429 with Retry-After, in the format of
// the route's family (writeRefusal), counted in lajstrom_auth_throttled_total and logged at
// info with the account, when there is one, and the client address.
func (s *Server) throttled(w http.ResponseWriter, r *http.Request, te *auth.ThrottledError, msg string) {
	metrics.Throttled.WithLabelValues(te.Throttle).Inc()
	attrs := []any{"throttle", te.Throttle}
	if te.Account != "" {
		attrs = append(attrs, "account", te.Account)
	}
	s.log.InfoContext(r.Context(), "throttled", append(attrs, "address", ClientAddr(r.Context()),
		"retryAfter", te.RetryAfter, "requestId", RequestID(r.Context()))...)
	w.Header().Set("Retry-After", retryAfter(te.RetryAfter))
	s.writeRefusal(w, r, http.StatusTooManyRequests, msg)
}

// retryAfter is the Retry-After header of a wait d: whole seconds, rounded up, at least 1.
func retryAfter(d time.Duration) string {
	return strconv.Itoa(max(int(math.Ceil(d.Seconds())), 1))
}

// writeRefusal answers a request that the middleware refuses, 401 with a challenge, 403, 429,
// 500 or 503, in the error format of the route's family: the error document of the distribution
// spec under /v2/ (internal/facade), the envelope of the management API under /api/
// (internal/api), plain text elsewhere (format.WriteError). A 401 is UNAUTHORIZED under /v2/
// and UNAUTHENTICATED under /api/ (#579); a 403 is DENIED under /v2/ and FORBIDDEN under /api/;
// a 429 is TOOMANYREQUESTS under /v2/ and UNAVAILABLE under /api/, a 503 UNAVAILABLE under
// both, and the caller has set Retry-After.
// The challenge of a 401 under /v2/ is the bearer challenge of the token
// flow (docs/spec/rbac.md §4) without a scope, as the facade answers: the client goes to the
// realm with the credentials that were refused, and learns there that they are wrong. On
// /v2/token itself, the one endpoint of /v2 that takes Basic, and everywhere else, it is Basic.
func (s *Server) writeRefusal(w http.ResponseWriter, r *http.Request, status int, msg string) {
	path := r.URL.Path
	v2 := path == "/v2" || strings.HasPrefix(path, "/v2/")
	if status == http.StatusUnauthorized {
		challenge := basicChallenge
		if v2 && path != tokenPath {
			realm, service := token.Realm(s.opts.ExternalURL, r)
			challenge = token.Challenge(realm, service, "", false)
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	var body any
	switch {
	case v2:
		code := "UNKNOWN"
		switch status {
		case http.StatusUnauthorized:
			code = "UNAUTHORIZED"
		case http.StatusForbidden:
			code = "DENIED"
		case http.StatusTooManyRequests:
			code = "TOOMANYREQUESTS"
		case http.StatusServiceUnavailable:
			code = "UNAVAILABLE"
		}
		body = map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}}
	case strings.HasPrefix(path, "/api/"):
		code := "INTERNAL"
		switch status {
		case http.StatusUnauthorized:
			code = "UNAUTHENTICATED"
		case http.StatusForbidden:
			code = "FORBIDDEN"
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			code = "UNAVAILABLE"
		}
		body = map[string]any{"error": map[string]string{"code": code, "message": msg}}
	default:
		writeText(w, status, msg+"\n")
		return
	}
	b, _ := json.Marshal(body) // maps of strings always encode
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
