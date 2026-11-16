package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/token"
)

// Tokens authenticates CI tokens, the secrets with auth.CIPrefix (docs/spec/rbac.md §11), on
// every route. Create it as a struct literal; its methods may be called concurrently.
type Tokens struct {
	// Policies give the trust policies that count now (Live), whose grants a CI token's policy
	// names get; required.
	Policies Source
	// Verifier checks the token with the token key; required.
	Verifier *token.Verifier
	// Log receives the refusals at info; nil means slog.Default().
	Log *slog.Logger
}

// Authenticate returns the ci: principal of the CI token secret and a CredCI with its id and the
// grants of its policies, as the configuration and the index have them now. A token that does not
// verify, and one none of whose policies is left, are auth.ErrBadCredentials, the reason logged at
// info (auth.LogRefusal), never the token; a failure to load the keys or to read the policies is another error.
func (t *Tokens) Authenticate(ctx context.Context, secret string) (auth.Principal, auth.Credential, error) {
	g, err := t.Verifier.VerifyCI(ctx, secret)
	switch {
	case errors.Is(err, token.ErrInvalidToken):
		return auth.Principal{}, auth.Credential{}, auth.ErrBadCredentials
	case err != nil:
		return auth.Principal{}, auth.Credential{}, fmt.Errorf("ci: a CI token: %w", err)
	}
	ps, err := t.Policies.Current(ctx)
	if err != nil {
		return auth.Principal{}, auth.Credential{}, err
	}
	grants, ok := ps.Grants(g.Policies)
	if !ok {
		auth.LogRefusal(ctx, t.log(), string(auth.CredCI), g.Principal.String(), 0, "none of its policies is left",
			"ciToken", g.ID, "policies", g.Policies)
		return auth.Principal{}, auth.Credential{}, auth.ErrBadCredentials
	}
	return g.Principal, auth.Credential{Kind: auth.CredCI, Grants: grants, CIToken: g.ID}, nil
}

func (t *Tokens) log() *slog.Logger {
	if t.Log == nil {
		return slog.Default()
	}
	return t.Log
}
