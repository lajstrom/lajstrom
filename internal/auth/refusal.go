package auth

import (
	"context"
	"log/slog"
)

// The kinds of a refused credential that are not a CredentialKind (docs/spec/rbac.md §1).
const (
	// RefusedBootstrap is the bootstrap token of POST /api/v1/bootstrap.
	RefusedBootstrap = "bootstrap"
	// RefusedBearer is a bearer token of /v2 (docs/spec/rbac.md §4).
	RefusedBearer = "bearer"
	// AccountUnknown is the account of a refusal whose credential names no principal that
	// Lajstrom knows.
	AccountUnknown = "unknown"
)

// requestKey is the context key of the Request of the line of a refusal.
type requestKey struct{}

// Request is what the line of a refused credential says of its request: its id and the client
// address of its access line (ADR 0011).
type Request struct {
	ID      string
	Address string
}

// WithRequest returns a copy of ctx that carries r. The server stores it for every request, so
// that whoever refuses a credential under ctx, the authenticator or a verifier, writes its line
// with the request's id and address (LogRefusal), and so that the audit row of a change that the
// authentication makes itself, the disabling of an inactive user (docs/spec/rbac.md §5), has them.
func WithRequest(ctx context.Context, r Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

// RequestFrom returns the Request of ctx, the zero Request without one.
func RequestFrom(ctx context.Context) Request {
	r, _ := ctx.Value(requestKey{}).(Request)
	return r
}

// LogRefusal writes the line at info of a refused credential (docs/spec/rbac.md §1, #1284):
// "authentication refused" with the kind of credential, the account it named (user:<uid>,
// token:<id>, ci:…, or AccountUnknown), the id of a token that was found when token is not 0, the
// reason, fixed text and never a value of the request, then attrs, and the client address and the
// request id of ctx (WithRequest). Never the user name as it was typed, which may be a password
// typed into the wrong field or a token pasted there (threat model L-3), nor the secret.
func LogRefusal(ctx context.Context, log *slog.Logger, credential, account string, token int64, reason string, attrs ...any) {
	if log == nil {
		log = slog.Default()
	}
	r := RequestFrom(ctx)
	all := make([]any, 0, 10+len(attrs))
	all = append(all, "credential", credential, "account", account)
	if token != 0 {
		all = append(all, "token", token)
	}
	all = append(all, "reason", reason)
	all = append(all, attrs...)
	log.InfoContext(ctx, "authentication refused", append(all, "address", r.Address, "requestId", r.ID)...)
}
