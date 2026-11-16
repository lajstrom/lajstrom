package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// basicChallenge is the WWW-Authenticate header of a 401 outside /v2/, and on /v2/token: clients
// answer it with HTTP Basic credentials (docs/spec/rbac.md §1 and §3), the challenge of
// format.WriteError.
const basicChallenge = `Basic realm="lajstrom"`

// tokenPath is the token endpoint of the facade, the one path under /v2/ that takes Basic, and
// on POST the user name and password of the OAuth2 form (authenticateForm).
const tokenPath = "/v2/token"

// Messages of the answers of the authentication middleware.
const (
	msgBadCredentials  = "credentials are needed: the ones sent were refused"
	msgAuthFailed      = "Internal Server Error"
	msgAuthUnavailable = "Service Unavailable"
	msgThrottled       = "too many failed logins: try again after Retry-After seconds"
	msgTooManyRequests = "too many token requests: try again after Retry-After seconds"
	msgBound           = "the token is bound to a DPoP key: send it as Authorization: DPoP with a proof"
	msgIdle            = "the sign-in was idle for longer than the server allows: sign in again"
)

// idleDescription is the error_description of the DPoP challenge of an idle sign-in
// (docs/spec/http-routing.md §4, #1277), which a web UI reads to say why it signs the person out.
const idleDescription = "idle"

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
//     are authenticated (a password, user:<personal access token> or deploy:<deploy token>; on
//     /v2/token also a Kubernetes service-account token, authenticateSecret), and the request
//     goes on as the user, the deploy token or the service account. Credentials that are refused,
//     or that do not decode, are answered at once with 401 and the Basic challenge, whatever
//     the route, in the error format of the route's family (writeRefusal).
//   - POST /v2/token: the credential is the username and the password of the OAuth2 form
//     (token.ReadForm, docs/spec/http-routing.md §2, #908), authenticated as Basic credentials
//     are; the Authorization header is not read. A form that is refused is answered 400 or 413
//     in OAuth2's format before its credential is checked (authenticateForm).
//   - GET /v2/token without an Authorization header: a client certificate, from the connection
//     or from a trusted proxy's header (certificate, docs/spec/rbac.md §9), when
//     Options.ClientCerts is set; refused like Basic credentials. Without one, anonymous.
//   - Bearer with a token secret (ljs_pat_… or ljs_dep_…, as npm's _authToken sends it) on
//     every route but /v2: authenticated as a personal access token or a deploy token
//     (auth.Authenticator.AuthenticateBearer), or a CI token (ljs_ci_…, Options.CI), and refused
//     like Basic credentials. A CI token is the password of Basic credentials and of the form
//     too, whatever the user name (authenticateSecret).
//   - DPoP (RFC 9449 §7.1) on /api/: the token of a web UI's sign-in with a proof of the key it
//     is bound to (authenticateDPoP, docs/spec/rbac.md §10); refused elsewhere. A token bound to a
//     key is refused in Basic credentials and as a bearer (auth.ErrBound), on /api/ with the
//     DPoP challenge.
//   - The OIDC login (oidcPrefix, docs/spec/http-routing.md §7): the request goes on as
//     anonymous with the header untouched; its routes take no credential.
//   - The sign-in of a web UI, POST /api/v1/login (loginPath, docs/spec/http-routing.md §4): the
//     request goes on as anonymous with the header untouched. Its body carries the credential,
//     which the management API checks, and a UI may still send the token it had, revoked or not.
//   - The legacy login of npm (npmLogin): the request goes on as anonymous with the header
//     untouched, whatever it says. Its body carries the password, which the npm plugin checks,
//     and npm sends with it the token it had, revoked or not (docs/spec/http-routing.md, npm).
//   - Any other bearer, a bearer on /v2 and any other scheme: the request goes on as anonymous
//     with the header untouched. The OCI facade verifies the bearer of /v2, a JWT, and records
//     its principal (HoldPrincipal).
//
// Every request also gets an evaluation scope (auth.WithEvaluationScope): the RBAC evaluator
// reads the index's auth_version once per request, however many repos the request checks. A
// request to /v2/token or a POST of /-/oidc/token is counted against its client address first
// (tokenThrottled).
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
		if r.Method == http.MethodPut && npmLogin.MatchString(r.URL.Path) || strings.HasPrefix(r.URL.Path, oidcPrefix) ||
			r.Method == http.MethodPost && r.URL.Path == loginPath {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == tokenPath {
			s.authenticateForm(w, r, next)
			return
		}
		if scheme, value, _ := strings.Cut(r.Header.Get("Authorization"), " "); strings.EqualFold(scheme, "DPoP") {
			s.authenticateDPoP(w, r, next, strings.TrimSpace(value))
			return
		}
		p, cred, ok, err := s.credential(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		s.settle(w, r, next, p, cred, err)
	})
}

// credential authenticates the credential of r's Authorization header, or the client
// certificate of a GET of /v2/token without one, and reports ok false when r has none that the
// middleware reads: the request goes on as anonymous with the header untouched (authenticate).
// Basic credentials that do not decode are auth.ErrBadCredentials.
func (s *Server) credential(r *http.Request) (p auth.Principal, cred auth.Credential, ok bool, err error) {
	scheme, value, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	switch {
	case strings.EqualFold(scheme, "Basic"):
		user, secret, decoded := r.BasicAuth()
		if !decoded {
			s.log.DebugContext(r.Context(), "authentication refused", "reason", "credentials that do not decode",
				"requestId", RequestID(r.Context()))
			return auth.Principal{}, auth.Credential{}, true, auth.ErrBadCredentials
		}
		p, cred, err = s.authenticateSecret(r, user, secret)
		return p, cred, true, err
	case strings.EqualFold(scheme, "Bearer") && tokenBearer(r.URL.Path, strings.TrimSpace(value)):
		secret := strings.TrimSpace(value)
		if strings.HasPrefix(secret, auth.CIPrefix) {
			p, cred, err = s.ciToken(r, secret)
		} else {
			p, cred, err = s.opts.Authenticator.AuthenticateBearer(r.Context(), secret)
		}
		return p, cred, true, err
	case scheme == "" && r.Method == http.MethodGet && r.URL.Path == tokenPath && s.opts.ClientCerts != nil:
		return s.certificate(r)
	}
	return auth.Principal{}, auth.Credential{}, false, nil
}

// settle ends the authentication of r with the outcome of its credential: refused credentials
// are 401 with the challenge, a failed-login throttle 429, a failure to decide 500, or 503 when
// the database does not answer; otherwise the request goes on as p, with cred.
func (s *Server) settle(w http.ResponseWriter, r *http.Request, next http.Handler, p auth.Principal, cred auth.Credential, err error) {
	var throttled *auth.ThrottledError
	switch {
	case errors.Is(err, auth.ErrBound) && strings.HasPrefix(r.URL.Path, "/api/"):
		s.refuseDPoP(w, r, "invalid_token", msgBound)
		return
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
}

// authenticateDPoP authenticates Authorization: DPoP <secret> (RFC 9449 §7.1), the token of a
// web UI's sign-in, on /api/ only (docs/spec/http-routing.md §4): the proof of the request first
// (Options.DPoP, dpop.Verifier.Verify), then the token and the key it is bound to, which must be
// the proof's (auth.Authenticator.AuthenticateDPoP), and last the proof's jti, used once on any
// replica (dpop.Verifier.Spend), so that a request with a made-up token writes nothing. A refused
// proof is 401 with the challenge error="invalid_dpop_proof" and why, a refused token 401 with
// error="invalid_token". Outside /api/, and on a server without Options.DPoP, the scheme is
// refused as credentials are.
func (s *Server) authenticateDPoP(w http.ResponseWriter, r *http.Request, next http.Handler, secret string) {
	if !strings.HasPrefix(r.URL.Path, "/api/") || s.opts.DPoP == nil {
		s.log.DebugContext(r.Context(), "authentication refused", "reason", "a DPoP token outside /api/",
			"requestId", RequestID(r.Context()))
		s.writeRefusal(w, r, http.StatusUnauthorized, msgBadCredentials)
		return
	}
	proof, err := s.opts.DPoP.Verify(r, secret)
	if err != nil {
		s.refuseDPoP(w, r, "invalid_dpop_proof", dpop.Why(err))
		return
	}
	p, cred, err := s.opts.Authenticator.AuthenticateDPoP(r.Context(), secret, proof.JKT)
	var idle *auth.IdleError
	if errors.As(err, &idle) {
		s.refuseIdle(w, r, proof, idle)
		return
	}
	if errors.Is(err, auth.ErrBadCredentials) {
		s.refuseDPoP(w, r, "invalid_token", "")
		return
	}
	if err == nil {
		err = s.opts.DPoP.Spend(r.Context(), proof)
	}
	if why := dpop.Why(err); why != "" {
		s.refuseDPoP(w, r, "invalid_dpop_proof", why)
		return
	}
	s.settle(w, r, next, p, cred, err)
}

// refuseDPoP answers 401 on /api/ with the challenge of RFC 9449 §7.1: WWW-Authenticate: DPoP
// with the error code, why as error_description when it is not "", and algs ES256. why is fixed
// text of dpop.Why or of this package, never the client's; the answer is the envelope of /api/,
// UNAUTHENTICATED with why as its message, or msgBadCredentials.
func (s *Server) refuseDPoP(w http.ResponseWriter, r *http.Request, code, why string) {
	s.refuseDPoPWith(w, r, code, why, cmp.Or(why, msgBadCredentials))
}

// refuseDPoPWith is refuseDPoP with msg as the message of the envelope.
func (s *Server) refuseDPoPWith(w http.ResponseWriter, r *http.Request, code, why, msg string) {
	s.log.DebugContext(r.Context(), "authentication refused", "reason", cmp.Or(why, "a DPoP token refused"),
		"requestId", RequestID(r.Context()))
	challenge := `DPoP error="` + code + `"`
	if why != "" {
		challenge += `, error_description="` + why + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge+`, algs="`+dpop.Algorithm+`"`)
	writeJSONRefusal(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "UNAUTHENTICATED",
		"message": msg}})
}

// refuseIdle answers the token of a web UI's sign-in that the authenticator refused as idle
// (auth.IdleError, docs/spec/rbac.md §10, #1277). The proof's jti is spent first, as for a token
// that passes, so that a replayed proof is refused as one and revokes nothing; then the token is
// revoked (Options.IdleRevoker), and the answer is 401 with the challenge error="invalid_token",
// error_description="idle". A revocation that fails is logged at error and the token refused all
// the same: every replica refuses it while it is idle, and its next request revokes it.
func (s *Server) refuseIdle(w http.ResponseWriter, r *http.Request, proof *dpop.Proof, idle *auth.IdleError) {
	err := s.opts.DPoP.Spend(r.Context(), proof)
	if why := dpop.Why(err); why != "" {
		s.refuseDPoP(w, r, "invalid_dpop_proof", why)
		return
	}
	if err != nil {
		s.settle(w, r, nil, auth.Principal{}, auth.Credential{}, err)
		return
	}
	if s.opts.IdleRevoker != nil {
		ctx := r.Context()
		if err := s.opts.IdleRevoker.RevokeIdle(ctx, idle, auth.Origin{IP: ClientAddr(ctx), RequestID: RequestID(ctx)}); err != nil {
			s.log.ErrorContext(ctx, "an idle sign-in is not revoked", "token", idle.TokenID, "error", err.Error(), "requestId", RequestID(ctx))
		}
	}
	s.refuseDPoPWith(w, r, "invalid_token", idleDescription, msgIdle)
}

// IdleRevoker revokes the token of a sign-in refused as idle, with its audit row
// (auth.Tokens.RevokeIdle).
type IdleRevoker interface {
	RevokeIdle(ctx context.Context, e *auth.IdleError, o auth.Origin) error
}

// authenticateForm authenticates POST /v2/token, the token request in the OAuth2 form that
// containerd posts before it tries GET (#908): the form is read first (token.ReadForm), and one
// that is refused is answered 400 or 413 in OAuth2's format, its credential not checked and
// counted as no failed login; then its username and password are authenticated as the user
// name and the secret of Basic credentials are, with the same refusals and throttles (settle).
// The facade reads the scopes and the service of the form, which ParseForm keeps on the request.
func (s *Server) authenticateForm(w http.ResponseWriter, r *http.Request, next http.Handler) {
	f, fe := token.ReadForm(w, r)
	if fe != nil {
		s.log.DebugContext(r.Context(), "token form refused", "status", fe.Status, "error", fe.Code, "reason", fe.Description,
			"requestId", RequestID(r.Context()))
		token.WriteFormError(w, fe)
		return
	}
	p, cred, err := s.authenticateSecret(r, f.Username, f.Password)
	s.settle(w, r, next, p, cred, err)
}

// issuerRetryAfter is the Retry-After of a token of a Kubernetes cluster whose issuer does not
// answer.
const issuerRetryAfter = 30 * time.Second

// authenticateSecret authenticates the user name and the secret of Basic credentials or of the
// form of r. A CI token is that token on every route, whatever the user name (ciToken). On
// /v2/token a secret that is a service-account token of a cluster of auth.kubernetes is that
// token, whatever the user name (Options.Kubernetes, docs/spec/rbac.md §9), and an issuer that
// does not answer is 503 with Retry-After (settle); any other secret is the authenticator's.
func (s *Server) authenticateSecret(r *http.Request, user, secret string) (auth.Principal, auth.Credential, error) {
	if strings.HasPrefix(secret, auth.CIPrefix) {
		return s.ciToken(r, secret)
	}
	if r.URL.Path == tokenPath && s.opts.Kubernetes != nil && s.opts.Kubernetes.Recognizes(secret) {
		p, cred, err := s.opts.Kubernetes.Authenticate(r.Context(), secret)
		if errors.Is(err, auth.ErrIssuerUnavailable) {
			err = fmt.Errorf("%w: %w", &format.UnavailableError{RetryAfter: issuerRetryAfter}, err)
		}
		return p, cred, err
	}
	return s.opts.Authenticator.Authenticate(r.Context(), user, secret)
}

// ciToken authenticates the CI token secret (Options.CI, docs/spec/rbac.md §11): without
// Options.CI it is refused, and counts as no failed login, as a token secret does not.
func (s *Server) ciToken(r *http.Request, secret string) (auth.Principal, auth.Credential, error) {
	if s.opts.CI == nil {
		s.log.DebugContext(r.Context(), "authentication refused", "reason", "a CI token without auth.ci", "requestId", RequestID(r.Context()))
		return auth.Principal{}, auth.Credential{}, auth.ErrBadCredentials
	}
	return s.opts.CI.Authenticate(r.Context(), secret)
}

// CIAuthenticator authenticates a CI token (ci.Tokens): its ci: principal and a CredCI, an error
// wrapping auth.ErrBadCredentials for a token that is refused, or another for a failure to decide.
type CIAuthenticator interface {
	Authenticate(ctx context.Context, secret string) (auth.Principal, auth.Credential, error)
}

// oidcPrefix is the prefix of the routes of the OIDC login.
const oidcPrefix = "/-/oidc/"

// loginPath is the sign-in of a web UI, whose body is its credential (docs/spec/rbac.md §10).
const loginPath = "/api/v1/login"

// npmLogin is the path of npm's legacy login, PUT /npm/<repo>/-/user/org.couchdb.user:<name>.
var npmLogin = regexp.MustCompile(`^/npm/[^/]+/-/user/org\.couchdb\.user:[^/]+$`)

// tokenBearer reports whether the bearer secret on path is the middleware's: a token secret
// (auth.PATPrefix, auth.DeployPrefix or auth.CIPrefix) outside /v2, whose bearer is the facade's
// JWT.
func tokenBearer(path, secret string) bool {
	if path == "/v2" || strings.HasPrefix(path, "/v2/") {
		return false
	}
	return strings.HasPrefix(secret, auth.PATPrefix) || strings.HasPrefix(secret, auth.DeployPrefix) ||
		strings.HasPrefix(secret, auth.CIPrefix)
}

// tokenThrottled answers 429 and reports true when r is a request to the token endpoint, or a
// POST of /-/oidc/token (docs/spec/http-routing.md §7), from a client address that made too many
// (auth.Authenticator.AllowTokenRequest). It runs before the credentials are checked, so that a
// flood of token requests costs no argon2id and no verification of a CI platform's token.
func (s *Server) tokenThrottled(w http.ResponseWriter, r *http.Request) bool {
	var te *auth.ThrottledError
	exchange := r.Method == http.MethodPost && r.URL.Path == oidcTokenPath
	if (r.URL.Path != tokenPath && !exchange) || !errors.As(s.opts.Authenticator.AllowTokenRequest(r.Context()), &te) {
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
	if status == http.StatusUnauthorized {
		challenge := basicChallenge
		if (path == "/v2" || strings.HasPrefix(path, "/v2/")) && path != tokenPath {
			realm, service := token.Realm(s.opts.ExternalURL, r)
			challenge = token.Challenge(realm, service, "", false)
		}
		w.Header().Set("WWW-Authenticate", challenge)
	}
	writeRefusalBody(w, r, status, msg)
}

// writeRefusalBody writes the body of a refusal of the middleware, with status, in the error
// format of the route's family (writeRefusal); the caller has set the challenge of a 401.
func writeRefusalBody(w http.ResponseWriter, r *http.Request, status int, msg string) {
	path := r.URL.Path
	v2 := path == "/v2" || strings.HasPrefix(path, "/v2/")
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
	writeJSONRefusal(w, status, body)
}

// writeJSONRefusal writes body, the error document of a refusal of the middleware, as JSON with
// status.
func writeJSONRefusal(w http.ResponseWriter, status int, body any) {
	b, _ := json.Marshal(body) // maps of strings always encode
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
