package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// basicChallenge is the WWW-Authenticate header of a 401: clients answer it with HTTP Basic
// credentials (docs/spec/rbac.md §1 and §3), the challenge of format.WriteError.
const basicChallenge = `Basic realm="lajstrom"`

// Messages of the answers of the authentication middleware.
const (
	msgBadCredentials = "credentials are needed: the ones sent were refused"
	msgAuthFailed     = "Internal Server Error"
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

// holdPrincipal records p in the holder of ctx, when there is one.
func holdPrincipal(ctx context.Context, p auth.Principal) {
	if h, ok := ctx.Value(principalHolderKey{}).(*principalHolder); ok {
		h.p = p
	}
}

// authenticate reads the request's credentials and puts its principal in the context
// (auth.WithPrincipal), for the authorizers and the audit log. Every route goes through it,
// the system endpoints included: they simply allow the anonymous principal.
//
//   - No Authorization header: the request goes on as auth.Anonymous. What anonymous may do is
//     the authorizers' to decide (#45).
//   - Basic (RFC 7617; the first colon splits the user name from the secret): the credentials
//     are authenticated, and the request goes on as the user. Credentials that are refused, or
//     that do not decode, are answered at once with 401 and the Basic challenge, whatever the
//     route, in the error format of the route's family (writeRefusal).
//   - Bearer and any other scheme: the request goes on as anonymous with the header untouched.
//     The OCI facade reads the bearer of /v2 (#26).
//
// A nil Options.Authenticator authenticates nobody: every request is anonymous, as in the unit
// tests of other packages.
func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.opts.Authenticator == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Basic") {
			next.ServeHTTP(w, r)
			return
		}
		user, secret, ok := r.BasicAuth()
		if !ok {
			s.log.DebugContext(r.Context(), "authentication refused", "reason", "credentials that do not decode",
				"requestId", RequestID(r.Context()))
			writeRefusal(w, r, http.StatusUnauthorized, msgBadCredentials)
			return
		}
		p, err := s.opts.Authenticator.Authenticate(r.Context(), user, secret)
		switch {
		case errors.Is(err, auth.ErrBadCredentials):
			writeRefusal(w, r, http.StatusUnauthorized, msgBadCredentials)
			return
		case err != nil:
			s.log.ErrorContext(r.Context(), "authentication failed", "user", user, "error", err.Error(),
				"requestId", RequestID(r.Context()))
			writeRefusal(w, r, http.StatusInternalServerError, msgAuthFailed)
			return
		}
		holdPrincipal(r.Context(), p)
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// writeRefusal answers a request that the middleware refuses, 401 with the Basic challenge
// or 500, in the error format of the route's family: the error document of the distribution
// spec under /v2/ (internal/facade), the envelope of the management API under /api/
// (internal/api; 401 is FORBIDDEN there, told apart by the status and the challenge), plain
// text elsewhere (format.WriteError). The challenge of /v2 stays Basic, as the facade's, until
// #26 turns both into the bearer challenge of docs/spec/rbac.md §4.
func writeRefusal(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", basicChallenge)
	}
	var body any
	switch path := r.URL.Path; {
	case path == "/v2" || strings.HasPrefix(path, "/v2/"):
		code := "UNAUTHORIZED"
		if status != http.StatusUnauthorized {
			code = "UNKNOWN"
		}
		body = map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}}
	case strings.HasPrefix(path, "/api/"):
		code := "FORBIDDEN"
		if status != http.StatusUnauthorized {
			code = "INTERNAL"
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
