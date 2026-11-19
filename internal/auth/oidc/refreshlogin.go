package oidc

import (
	"context"
	"fmt"
	"net/http"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/server"
)

// The login's part of asking the IdP again (docs/spec/rbac.md §8, #1282): with
// auth.oidc.refresh.interval the callback seals the refresh token of the code exchange into a
// row of oidc_refresh, in the transaction of the login, and the mint of the login's token, at the
// callback, at the device's poll or at POST /api/v1/login, joins the row to the token.

// Sealer seals the refresh tokens of the logins: the master keys (keys.Master).
type Sealer interface {
	// Seal seals plain with the key the index names, aad its additional data; without it, an
	// error.
	Seal(aad string, plain []byte) ([]byte, error)
	// Sealing reports whether the process holds the key the index names to seal.
	Sealing() (id string, held bool)
}

// unsealed is the value of a new row of oidc_refresh until the refresh token, sealed with the
// row's id, replaces it in the same transaction.
var unsealed = []byte{0}

// sealError is a refresh token that the replica could not seal: the callback answers 503.
type sealError struct{ err error }

func (e *sealError) Error() string { return "oidc: the refresh token not sealed: " + e.err.Error() }
func (e *sealError) Unwrap() error { return e.err }

// seals reports whether this replica holds the master key the index names to seal.
func (h *handler) seals() bool {
	_, held := h.o.Sealer.Sealing()
	return held
}

// sealsNothing answers 503 with Retry-After while the replica seals nothing (index-schema.md, The
// master key and its replacement): a login would keep a refresh token, which it cannot seal.
func (h *handler) sealsNothing(w http.ResponseWriter, r *http.Request, err error) {
	args := []any{"requestId", server.RequestID(r.Context())}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	h.o.Log.WarnContext(r.Context(), "oidc: a login refused for now: this replica does not hold the master key that seals, "+
		"and a login keeps a refresh token (auth.oidc.refresh.interval)", args...)
	w.Header().Set("Retry-After", "30")
	writePage(w, http.StatusServiceUnavailable, page{Title: "Login unavailable",
		Message: "Lajstrom cannot finish logins for a moment: try again in a minute.", Again: true})
}

// loginOf is the id of the oidc_logins row whose token a login mints: the device request that a
// code login approves, else the code login itself.
func loginOf(row *index.OIDCLogin) int64 {
	if row.DeviceID != nil {
		return *row.DeviceID
	}
	return row.ID
}

// loginRefresh is the join of the token minted for the login request id.
func (h *handler) loginRefresh(id int64) auth.LoginRefresh {
	return auth.LoginRefresh{Login: id, Interval: h.o.Refresh}
}

// keepRefresh returns the step of the login's transaction that keeps token, the refresh token of
// the code exchange, for the login request login: a row of oidc_refresh of the identity, due
// LoginTTL after the callback (a token not minted by then never will be), its value sealed with
// the row's id. It reports false for a login whose IdP issued no refresh token. A seal that fails
// is a *sealError. The token is never logged nor put in an error.
func (h *handler) keepRefresh(login int64, token string) func(context.Context, index.Writer, int64) (bool, error) {
	at := h.o.Now()
	return func(ctx context.Context, w index.Writer, identityID int64) (bool, error) {
		if token == "" {
			return false, nil
		}
		row := index.OIDCRefresh{LoginID: &login, IdentityID: identityID, Sealed: unsealed, CreatedAt: at, NextAt: at.Add(LoginTTL)}
		if err := w.CreateOIDCRefresh(ctx, &row); err != nil {
			return false, err
		}
		plain := []byte(token)
		sealed, err := h.o.Sealer.Seal(auth.RefreshAAD(row.ID), plain)
		clear(plain)
		if err != nil {
			return false, &sealError{err: err}
		}
		switch ok, err := w.Reseal(ctx, index.SealedRefresh, row.ID, unsealed, sealed); {
		case err != nil:
			return false, err
		case !ok:
			return false, fmt.Errorf("oidc: the refresh token of oidc_refresh %d not sealed in place", row.ID)
		}
		return true, nil
	}
}
