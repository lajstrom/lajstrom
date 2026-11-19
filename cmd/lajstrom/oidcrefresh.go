package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/auth/oidc"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
	"github.com/lajstrom/lajstrom/internal/metrics"
)

// Asking the IdP again about each OIDC login (docs/spec/rbac.md §8, #1282; ADR 0037):
// auth.oidc.refresh.interval, its master key, its warning at start, and the refresh itself.

// errRefreshWithoutKey is serve's refusal of auth.oidc.refresh.interval without a master key.
var errRefreshWithoutKey = errors.New("auth.oidc.refresh.interval is set and no master key is: asking the IdP again keeps each " +
	"login's refresh token sealed with the master key. Configure one (keys.masterKeyEnv, keys.masterKeyFile, keys.masterKeyDir " +
	"or keys.masterKeyKMS), or set auth.oidc.refresh.interval to 0")

// checkRefreshKey is serve's check of auth.oidc.refresh.interval at start: with auth.oidc on, an
// interval takes a master key, m; without one the configuration is refused (exit status 2).
func checkRefreshKey(o config.OIDC, m *keys.Master) error {
	if o.Issuer == "" || o.Refresh.Interval == 0 || m != nil {
		return nil
	}
	return configError{errRefreshWithoutKey}
}

// warnRefreshGrant logs at warn, at start with auth.oidc.refresh.interval, when the IdP's
// discovery document lists grant_types_supported without refresh_token: its logins may keep no
// refresh token, and last auth.oidc.tokenLifetime each. A document that lists none, or that does
// not load, says nothing: the callback warns of each login without a refresh token. Nothing
// without the setting or without auth.oidc (p nil).
func warnRefreshGrant(ctx context.Context, o config.OIDC, p *oidc.Client, log *slog.Logger) {
	if p == nil || o.Refresh.Interval == 0 {
		return
	}
	grants, err := p.GrantTypes(ctx)
	if err != nil {
		log.DebugContext(ctx, "the IdP's grant types not read at start", "error", err.Error())
		return
	}
	if grants != nil && !slices.Contains(grants, "refresh_token") {
		log.WarnContext(ctx, "the IdP's discovery document lists no refresh_token grant: its logins may keep no refresh token, "+
			"and are then not asked about again (auth.oidc.refresh.interval)", "grantTypesSupported", grants)
	}
}

// The refresh's schedule and its bounds (docs/spec/rbac.md §8, Asking the IdP again; ADR 0037).
const (
	// refreshTick is how often a replica claims the due rows.
	refreshTick = 30 * time.Second
	// refreshClaim is how long a claim holds a row: a call takes refreshCallTimeout at most.
	refreshClaim = 2 * time.Minute
	// refreshCalls is how many calls to the IdP a replica makes at once.
	refreshCalls = 4
	// refreshCallTimeout bounds a call to the IdP's token endpoint.
	refreshCallTimeout = 10 * time.Second
	// refreshRetry is the wait between two tries to write an outcome, while the claim lasts.
	refreshRetry = time.Second
)

// The outcomes of a refresh, the label of lajstrom_oidc_refreshes_total.
const (
	refreshOK      = "ok"      // the IdP confirmed the login
	refreshRefused = "refused" // the IdP answered invalid_grant: the login's token is revoked
	refreshError   = "error"   // anything else: tried again later, nothing ends
	refreshDropped = "dropped" // the login's token is revoked, expired or never minted: the row is deleted without a call
)

// refreshIDP is the IdP as the refresh asks it (oidc.Client).
type refreshIDP interface {
	Refresh(ctx context.Context, refreshToken string) (*oidc.Refreshed, error)
}

// refreshKeys opens and seals the refresh tokens (keys.Master).
type refreshKeys interface {
	Open(aad string, sealed []byte) ([]byte, error)
	Seal(aad string, plain []byte) ([]byte, error)
	Sealing() (id string, held bool)
}

// refresher asks the IdP again about each OIDC login whose row of oidc_refresh is due, on one
// replica (docs/spec/rbac.md §8, Asking the IdP again, #1282): it claims the due rows with the
// replica's id, refreshCalls at a time, and writes each outcome under its claim. The replicas
// share the rows through the claims, without a leader. A refresh token is never logged, audited
// or put in an error.
type refresher struct {
	st       index.Store
	idp      refreshIDP
	keys     refreshKeys
	tokens   *auth.Tokens
	users    *auth.Users
	replica  string
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger
}

// startRefresh starts the refresh of the OIDC logins on wg while auth.oidc.refresh.interval is
// set, with the relying party idp and the master keys m, under the replica's id. Off, or without
// auth.oidc (idp nil), it deletes every row of oidc_refresh before serve listens: no refresh
// token stays in the index for nothing.
func startRefresh(ctx context.Context, c *config.Config, st index.Store, idp *oidc.Client, m *keys.Master, replica string,
	wg *sync.WaitGroup, log *slog.Logger) {
	interval := c.Auth.OIDC.Refresh.Interval
	if idp == nil || interval == 0 || m == nil {
		switch n, err := st.ClearOIDCRefresh(ctx); {
		case err != nil:
			log.WarnContext(ctx, "the refresh tokens of the OIDC logins not deleted (auth.oidc.refresh.interval is 0)", "error", err.Error())
		case n > 0:
			log.InfoContext(ctx, "the refresh tokens of the OIDC logins deleted: auth.oidc.refresh.interval is 0", "count", n)
		}
		return
	}
	r := &refresher{st: st, idp: idp, keys: m, tokens: &auth.Tokens{Index: st, Log: log}, users: usersOf(c, st, log),
		replica: replica, interval: interval, now: time.Now, log: log}
	wg.Go(func() { r.run(ctx) })
	log.InfoContext(ctx, "OIDC logins asked about again with their refresh tokens", "auth.oidc.refresh.interval", interval.String())
}

// run refreshes the due rows every refreshTick until ctx ends.
func (r *refresher) run(ctx context.Context) {
	t := time.NewTicker(refreshTick)
	defer t.Stop()
	for {
		r.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// round claims the due rows, refreshCalls at a time, and refreshes each batch at once, until none
// is due or ctx ends. A replica that seals nothing claims none: an answer may hold a new refresh
// token, which it could not keep.
func (r *refresher) round(ctx context.Context) {
	for ctx.Err() == nil {
		if _, held := r.keys.Sealing(); !held {
			return
		}
		now := r.now()
		rows, err := r.st.ClaimOIDCRefresh(ctx, r.replica, now, now.Add(refreshClaim), refreshCalls)
		if err != nil {
			if ctx.Err() == nil {
				r.log.WarnContext(ctx, "oidc: the due logins not claimed; trying again", "error", err.Error())
			}
			return
		}
		if len(rows) == 0 {
			return
		}
		var wg sync.WaitGroup
		for _, row := range rows {
			wg.Go(func() {
				if outcome := r.refresh(ctx, row); outcome != "" {
					metrics.OIDCRefreshes.WithLabelValues(outcome).Inc()
				}
			})
		}
		wg.Wait()
	}
}

// refresh refreshes the claimed row and returns its outcome; "" when the end of serve cut the
// call short, which records nothing: the claim ends, and a replica asks again. An answer that
// came is recorded even then.
func (r *refresher) refresh(ctx context.Context, row index.OIDCRefresh) string {
	live, err := r.live(ctx, row)
	switch {
	case err != nil:
		r.log.WarnContext(ctx, "oidc: the token of a login not read; its refresh is tried again", "row", row.ID, "error", err.Error())
		return refreshError
	case !live:
		return r.drop(ctx, row)
	}
	plain, err := r.keys.Open(auth.RefreshAAD(row.ID), row.Sealed)
	if err != nil {
		return r.failed(ctx, row, "seal", err)
	}
	call, cancel := context.WithTimeout(ctx, refreshCallTimeout)
	got, err := r.idp.Refresh(call, string(plain))
	cancel()
	clear(plain)
	var re *oidc.RefreshError
	switch {
	case err == nil:
		return r.confirmed(ctx, row, got)
	case errors.Is(err, oidc.ErrRefused):
		return r.refused(ctx, row)
	case ctx.Err() != nil:
		return ""
	case errors.As(err, &re):
		return r.failed(ctx, row, re.Kind, err)
	}
	return r.failed(ctx, row, oidc.RefreshUnavailable, err)
}

// live reports whether the row's login has a token neither revoked nor expired: a row that has
// none, its code never redeemed, is due LoginTTL after its callback, and so not live.
func (r *refresher) live(ctx context.Context, row index.OIDCRefresh) (bool, error) {
	if row.TokenID == nil {
		return false, nil
	}
	page, err := r.st.ListTokenPage(ctx, index.TokenFilter{After: *row.TokenID - 1, Limit: 1, LiveAt: r.now()})
	if err != nil {
		return false, err
	}
	return len(page) == 1 && page[0].ID == *row.TokenID, nil
}

// drop deletes the row of a login whose token is revoked, expired or was never minted, without a
// call, under its claim.
func (r *refresher) drop(ctx context.Context, row index.OIDCRefresh) string {
	err := r.write(ctx, row, func(ctx context.Context, w index.Writer) error {
		fence := row
		fence.Sealed = nil
		if err := w.FinishOIDCRefresh(ctx, &fence, r.replica); err != nil {
			return err
		}
		return w.DeleteOIDCRefresh(ctx, row.ID)
	})
	if err != nil {
		return refreshError
	}
	return refreshDropped
}

// refused revokes the login's token, once on any replica, with its token.revoke row, in the
// transaction that deletes the row under its claim. The user and its other tokens are as they
// were. The refusal is logged at info with the principal and the token's id.
func (r *refresher) refused(ctx context.Context, row index.OIDCRefresh) string {
	var ev *index.AuditEvent
	err := r.write(ctx, row, func(ctx context.Context, w index.Writer) (err error) {
		fence := row
		fence.Sealed = nil
		if err := w.FinishOIDCRefresh(ctx, &fence, r.replica); err != nil {
			return err
		}
		if ev, err = r.tokens.RevokeRefused(ctx, w, *row.TokenID, row.RefreshedAt); err != nil {
			return err
		}
		return w.DeleteOIDCRefresh(ctx, row.ID)
	})
	if err != nil {
		return refreshError
	}
	r.tokens.WriteAudit(ctx, ev)
	principal := ""
	if ev != nil {
		principal = ev.Subject
	}
	r.log.InfoContext(ctx, "oidc: the IdP refused a login's refresh: its token is revoked", "principal", principal,
		"token", *row.TokenID, "refreshedAt", row.RefreshedAt.UTC().Format(time.RFC3339))
	return refreshRefused
}

// failed records an error of kind under the row's claim: nothing ends, the row is due again
// after a backoff, and the error is logged at warn by its kind, never in the IdP's words.
func (r *refresher) failed(ctx context.Context, row index.OIDCRefresh, kind string, cause error) string {
	next := row
	next.Sealed, next.Failures = nil, row.Failures+1
	next.NextAt = auth.RefreshDue(r.now(), refreshBackoff(next.Failures, r.interval))
	err := r.write(ctx, row, func(ctx context.Context, w index.Writer) error { return w.FinishOIDCRefresh(ctx, &next, r.replica) })
	if err == nil {
		r.log.WarnContext(ctx, "oidc: a login's refresh failed, and ends nothing; it is tried again", "kind", kind, "error", cause.Error(),
			"token", tokenOf(row), "failures", next.Failures, "next", next.NextAt.UTC().Format(time.RFC3339))
	}
	return refreshError
}

// refreshBackoff is the wait after the nth error in a row: 1, 2, 4 … minutes, at most interval.
func refreshBackoff(n int, interval time.Duration) time.Duration {
	if n > 16 {
		return interval
	}
	return min(time.Minute<<(n-1), interval)
}

// confirmed records a success under the row's claim: the new refresh token, sealed, when the IdP
// rotated it, the IdP's confirmation and the next due time; and, when the answer has an ID token,
// which must name the row's identity, the identity's groups replaced when they differ, in the same
// transaction. The token's expiry, its sign-in and the user's last activity do not move.
func (r *refresher) confirmed(ctx context.Context, row index.OIDCRefresh, got *oidc.Refreshed) string {
	if id := got.Identity; id != nil {
		ident, err := r.st.GetIdentity(context.WithoutCancel(ctx), id.Issuer, id.Subject)
		switch {
		case errors.Is(err, index.ErrNotFound) || (err == nil && ident.ID != row.IdentityID):
			return r.failed(ctx, row, "identity", errors.New("the refreshed ID token names another identity than the login's"))
		case err != nil:
			return r.failed(ctx, row, "index", err)
		}
	}
	next := row
	next.Sealed = nil
	if got.RefreshToken != "" {
		plain := []byte(got.RefreshToken)
		sealed, err := r.keys.Seal(auth.RefreshAAD(row.ID), plain)
		clear(plain)
		if err != nil {
			return r.failed(ctx, row, "seal", err)
		}
		next.Sealed = sealed
	}
	now := r.now()
	next.RefreshedAt, next.NextAt, next.Failures = now, auth.RefreshDue(now, r.interval), 0
	var ev *index.AuditEvent
	err := r.write(ctx, row, func(ctx context.Context, w index.Writer) (err error) {
		ev = nil
		if err := w.FinishOIDCRefresh(ctx, &next, r.replica); err != nil || got.Identity == nil {
			return err
		}
		ev, err = r.users.RefreshGroups(ctx, w, got.Identity.Issuer, got.Identity.Subject, got.Identity.Groups, *row.TokenID)
		return err
	})
	if err != nil {
		return refreshError
	}
	r.users.WriteAudit(ctx, ev)
	return refreshOK
}

// write runs fn in a transaction, on a context that the end of serve does not cancel, and again
// after refreshRetry while it fails and the row's claim lasts, but not after serve began to end:
// a rotated refresh token lost makes the next refresh send the old one. ErrConflict ends it: the
// claim is not the replica's any more. A failure is logged.
func (r *refresher) write(ctx context.Context, row index.OIDCRefresh, fn func(context.Context, index.Writer) error) error {
	wctx := context.WithoutCancel(ctx)
	for {
		err := r.st.Tx(wctx, func(w index.Writer) error { return fn(wctx, w) })
		switch {
		case err == nil:
			return nil
		case errors.Is(err, index.ErrConflict):
			r.log.InfoContext(ctx, "oidc: the claim of a login's refresh ended; another replica refreshes it", "row", row.ID)
			return err
		case ctx.Err() != nil || !r.now().Add(refreshRetry).Before(*row.ClaimedUntil):
			r.log.WarnContext(ctx, "oidc: the outcome of a login's refresh not written", "row", row.ID, "error", err.Error())
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(refreshRetry):
		}
	}
}

// tokenOf is the id of the row's token, 0 before its mint.
func tokenOf(row index.OIDCRefresh) int64 {
	if row.TokenID == nil {
		return 0
	}
	return *row.TokenID
}
