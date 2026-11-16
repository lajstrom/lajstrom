package ci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The token exchange of RFC 8693 (docs/spec/http-routing.md §7): names of grants and of token
// types, no credential.
//
//nolint:gosec // G101 reads "token" in the names of OAuth's types as a credential
const (
	// GrantTokenExchange is its grant_type.
	GrantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// The subject token types taken, the issued one, and the token type of the answer.
	typeJWT         = "urn:ietf:params:oauth:token-type:jwt"
	typeIDToken     = "urn:ietf:params:oauth:token-type:id_token"
	typeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	// MaxBody bounds the body of an exchange: a CI platform's token is a kilobyte or two.
	MaxBody = 16 << 10
	// retryAfter is the Retry-After of an issuer whose keys never loaded, in seconds.
	retryAfter = "30"
)

// ActionExchange is the audit action of a CI token minted (docs/spec/rbac.md §11).
const ActionExchange = "token.exchange"

// SourceCI is the source of the audit row of an exchange.
const SourceCI = "ci"

// Exchange answers the token exchange of POST /-/oidc/token (docs/spec/rbac.md §11,
// docs/spec/http-routing.md §7): a CI platform's OIDC token, verified by its issuer's keys and
// matched against the trust policies, for a CI token, with the token.exchange row of its
// exchange. Create it as a struct literal; it may serve requests concurrently.
type Exchange struct {
	// Policies give the issuers and the trust policies that count now (Live); required.
	Policies Source
	// Verifiers verify the tokens of each issuer, by its name (oidc.Verifier); required for every
	// issuer.
	Verifiers map[string]auth.TokenVerifier
	// Tokens mints the CI tokens with the token key; required.
	Tokens *token.Issuer
	// Lifetime is auth.ci.tokenLifetime.
	Lifetime time.Duration
	// ExternalURL is server.externalURL, whose host is the service, the iss of a CI token.
	ExternalURL string
	// Index records the audit row of each exchange; required.
	Index index.Store
	// Now is the clock of the audit rows and of their rate; nil is time.Now.
	Now func() time.Time
	// Log gets the refusals at info level with their reason; nil is slog.Default().
	Log *slog.Logger

	refusals refusals // the rate of the audit rows of refusals
}

// exchangeResponse is the answer of an exchange (RFC 8693 §2.2.1), with the principal and the
// policies of the CI token.
type exchangeResponse struct {
	AccessToken     string   `json:"access_token"`
	IssuedTokenType string   `json:"issued_token_type"`
	TokenType       string   `json:"token_type"`
	ExpiresIn       int      `json:"expires_in"`
	Principal       string   `json:"principal"`
	Policies        []string `json:"policies"`
}

// oauthError is a refusal in OAuth's form (RFC 6749 §5.2).
type oauthError struct {
	status      int
	code        string
	description string
}

// ServeHTTP answers one exchange. The body is read here unless the caller parsed it already
// (r.PostForm), as the dispatcher of /-/oidc/token does.
func (e *Exchange) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	subject, oe := readForm(w, r)
	if oe != nil {
		e.refuse(w, r, oe, "")
		return
	}
	ps, err := e.Policies.Current(ctx)
	if err != nil {
		e.fail(w, r, auth.Principal{}, err)
		return
	}
	i := ps.IssuerOf(unverifiedIssuer(subject))
	if i == nil {
		e.refuseToken(w, r, refusal{description: "the subject token is not a token of a configured issuer"})
		return
	}
	claims, err := e.Verifiers[i.Name].Verify(ctx, subject)
	switch {
	case errors.Is(err, auth.ErrIssuerUnavailable):
		e.log().WarnContext(ctx, "ci: the issuer does not answer", "issuer", i.Name, "error", err.Error(), "requestId", server.RequestID(ctx))
		w.Header().Set("Retry-After", retryAfter)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable",
			"error_description": "the issuer " + i.Name + " does not answer: try again after Retry-After seconds"})
		return
	case err != nil:
		e.refuseToken(w, r, refusal{description: "the subject token does not verify for " + i.Name, reason: err.Error(), issuer: i})
		return
	}
	m, err := i.Match(claims)
	if err != nil {
		e.refuseToken(w, r, refusal{description: strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "), issuer: i, claims: claims})
		return
	}
	_, service := token.Realm(e.ExternalURL, r)
	t, err := e.Tokens.IssueCI(ctx, m.Principal, service, m.Policies, e.Lifetime)
	if err == nil {
		err = e.record(ctx, r, m, t)
	}
	if err != nil {
		e.fail(w, r, m.Principal, err)
		return
	}
	e.log().InfoContext(ctx, "ci: token exchanged", "principal", m.Principal.String(), "policies", m.Policies, "token", t.ID,
		"requestId", server.RequestID(ctx))
	writeJSON(w, http.StatusOK, exchangeResponse{AccessToken: t.Raw, IssuedTokenType: typeAccessToken, TokenType: "Bearer",
		ExpiresIn: int(t.ExpiresAt.Sub(t.IssuedAt) / time.Second), Principal: m.Principal.String(), Policies: m.Policies})
}

// fail answers 500 for a failure of the index or of the token key, logged with the principal, when
// there is one, and the request id.
func (e *Exchange) fail(w http.ResponseWriter, r *http.Request, p auth.Principal, err error) {
	ctx := r.Context()
	e.log().ErrorContext(ctx, "ci: token exchange failed", "principal", p.String(), "error", err.Error(), "requestId", server.RequestID(ctx))
	writeOAuthError(w, http.StatusInternalServerError, "server_error", "Lajstrom could not mint the token: try again")
}

// record writes the token.exchange row of the CI token t of m, in a transaction of its own,
// before the token is answered: a CI token is never out without its row.
func (e *Exchange) record(ctx context.Context, r *http.Request, m *Match, t *token.Token) error {
	return e.appendRow(ctx, r, m.Principal, map[string]any{"outcome": outcomeMinted, "issuer": m.Issuer, "policies": m.Policies,
		"token": t.ID, "expiresAt": t.ExpiresAt.UTC().Format(time.RFC3339), "claims": m.Claims})
}

// appendRow appends a token.exchange row of p, details with the source and the request id, from
// the client of r, in a transaction of its own.
func (e *Exchange) appendRow(ctx context.Context, r *http.Request, p auth.Principal, details map[string]any) error {
	details["source"] = SourceCI
	if id := server.RequestID(ctx); id != "" {
		details["requestId"] = id
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("ci: audit details: %w", err)
	}
	ev := index.AuditEvent{At: e.now(), Principal: p.String(), Action: ActionExchange, Subject: p.String(), IP: clientOf(r),
		Details: raw}
	if err := e.Index.Tx(ctx, func(w index.Writer) error { return w.AppendAudit(ctx, &ev) }); err != nil {
		return fmt.Errorf("ci: audit row of the exchange: %w", err)
	}
	return nil
}

// clientOf is the client address of r: the server's (behind a trusted proxy, the forwarded one),
// or the peer's.
func clientOf(r *http.Request) string {
	if ip := server.ClientAddr(r.Context()); ip != "" {
		return ip
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func (e *Exchange) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// readForm reads the form of an exchange and returns its subject token, or the refusal of
// docs/spec/http-routing.md §7.
func readForm(w http.ResponseWriter, r *http.Request) (string, *oauthError) {
	if oe := parseBody(w, r); oe != nil {
		return "", oe
	}
	invalid := func(d string) (string, *oauthError) {
		return "", &oauthError{http.StatusBadRequest, "invalid_request", d}
	}
	f := r.PostForm
	one := func(k string) (string, bool) {
		v := f[k]
		return strings.Join(v, ""), len(v) == 1
	}
	if g, _ := one("grant_type"); g != GrantTokenExchange {
		return "", &oauthError{http.StatusBadRequest, "unsupported_grant_type", "the grant type is " + GrantTokenExchange}
	}
	for _, k := range []string{"grant_type", "subject_token", "subject_token_type"} {
		if v, ok := one(k); !ok || v == "" {
			return invalid(k + " is needed, once")
		}
	}
	if t, _ := one("subject_token_type"); t != typeJWT && t != typeIDToken {
		return invalid("subject_token_type is " + typeJWT + " or " + typeIDToken)
	}
	if v, ok := f["requested_token_type"]; ok && (len(v) != 1 || v[0] != typeAccessToken) {
		return invalid("requested_token_type is " + typeAccessToken)
	}
	if _, ok := f["actor_token"]; ok {
		return invalid("actor_token is not taken: a CI token acts for no one else")
	}
	if strings.TrimSpace(strings.Join(f["scope"], "")) != "" {
		return "", &oauthError{http.StatusBadRequest, "invalid_scope", "a CI token is not narrowed: its trust policies grant what it may do"}
	}
	subject, _ := one("subject_token")
	return subject, nil
}

// parseBody parses the body of r into r.PostForm, unless the caller did: a form of at most
// MaxBody bytes.
func parseBody(w http.ResponseWriter, r *http.Request) *oauthError {
	if r.PostForm != nil {
		return nil
	}
	tooBig := &oauthError{http.StatusRequestEntityTooLarge, "invalid_request", fmt.Sprintf("the body is at most %d bytes", MaxBody)}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return &oauthError{http.StatusBadRequest, "invalid_request", "the body is application/x-www-form-urlencoded"}
	}
	if r.ContentLength > MaxBody {
		return tooBig
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return tooBig
		}
		return &oauthError{http.StatusBadRequest, "invalid_request", "the body is not a form"}
	}
	return nil
}

// unverifiedIssuer returns the iss of the payload of the JWT raw, read before it is verified, or
// "" when raw is not a JWT.
func unverifiedIssuer(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Issuer
}

// refuse answers oe and logs it at info, with what is known of the token (who), never the token.
func (e *Exchange) refuse(w http.ResponseWriter, r *http.Request, oe *oauthError, who string) {
	ctx := r.Context()
	e.log().InfoContext(ctx, "ci: token exchange refused", "error", oe.code, "reason", oe.description, "token", who,
		"address", server.ClientAddr(ctx), "requestId", server.RequestID(ctx))
	writeOAuthError(w, oe.status, oe.code, oe.description)
}

func (e *Exchange) log() *slog.Logger {
	if e.Log == nil {
		return slog.Default()
	}
	return e.Log
}

// writeJSON writes v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client went away
}

// writeOAuthError writes an OAuth error (RFC 6749 §5.2).
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
