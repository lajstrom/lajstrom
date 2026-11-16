package server

import (
	"encoding/json"
	"errors"
	"net/http"
)

// oidcTokenPath is the token endpoint of /-/oidc/ (docs/spec/http-routing.md §7): the device grant
// of the OIDC login (Options.OIDC) and the token exchange for CI (Options.CIExchange).
const oidcTokenPath = "/-/oidc/token" //nolint:gosec // a path, no credential

// tokenExchangeGrant is the grant_type of RFC 8693, which Options.CIExchange answers.
const tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // a grant's name, no credential

// maxOIDCTokenBody bounds the body of POST /-/oidc/token: the token exchange's (ci.MaxBody), a
// CI platform's token being a kilobyte or two.
const maxOIDCTokenBody = 16 << 10

// oidcToken serves POST /-/oidc/token while Options.CIExchange is set: it reads the form, then
// hands the token exchange to Options.CIExchange and any other grant to Options.OIDC, the device
// grant of the login; without the login another grant is 400 unsupported_grant_type. A body over
// maxOIDCTokenBody is 413 invalid_request, one that does not parse 400 invalid_request, in OAuth's
// form (RFC 6749 §5.2).
func (s *Server) oidcToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.ContentLength > maxOIDCTokenBody {
		writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_request", "the body is at most 16384 bytes")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOIDCTokenBody)
	if err := r.ParseForm(); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			writeOAuthError(w, http.StatusRequestEntityTooLarge, "invalid_request", "the body is at most 16384 bytes")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form")
		return
	}
	switch {
	case r.PostForm.Get("grant_type") == tokenExchangeGrant:
		s.opts.CIExchange.ServeHTTP(w, r)
	case s.opts.OIDC != nil:
		s.opts.OIDC.ServeHTTP(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "the grant type is "+tokenExchangeGrant)
	}
}

// writeOAuthError writes an OAuth error (RFC 6749 §5.2).
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	b, _ := json.Marshal(map[string]string{"error": code, "error_description": description}) // strings always marshal
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}
