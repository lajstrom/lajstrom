package auth

import (
	"container/list"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// authenticatePAT returns the principal user:<uid> of the user called username when secret is a personal access token
// of that user: a token of kind pat that is not revoked and not expired (ExpiresAt before now,
// without leeway), whose owner is the user called username and is not disabled, and which has an
// expiry unless its owner is a service account (docs/spec/rbac.md §1). A malformed
// user name, the reserved name deploy among them, and a secret that is not of the token's
// shape are refused without the index. Otherwise it costs one GetTokenByHash and one GetUser,
// a miss as much as a hit, and no argon2id.
func (a *Authenticator) authenticatePAT(ctx context.Context, username, secret string) (Principal, Credential, error) {
	if CheckName(username) != nil {
		return Principal{}, Credential{}, a.refuse(ctx, nil, "malformed user name")
	}
	if !tokenShaped(secret, PATPrefix) {
		return Principal{}, Credential{}, a.refuse(ctx, nil, "malformed token")
	}
	t, err := a.token(ctx, secret)
	if err != nil {
		return Principal{}, Credential{}, err
	}
	u, err := a.index.GetUser(ctx, username)
	switch {
	case errors.Is(err, index.ErrNotFound):
		u = nil
	case err != nil:
		return Principal{}, Credential{}, fmt.Errorf("auth: authenticate a token: read its user: %w", err)
	}
	reason := a.dead(t, TokenPAT)
	switch {
	case reason != "":
	case u == nil:
		reason = "unknown user"
	case u.ID != *t.UserID:
		reason = "the token of another user"
	case u.Disabled:
		reason = "disabled"
	default:
		reason = unbounded(t, u)
	}
	if reason != "" {
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, reason)
	}
	if t.DPoPJKT != "" {
		return Principal{}, Credential{}, a.refuseBound(ctx, u, t)
	}
	a.touch(ctx, t)
	return UserPrincipal(u), patCredential(t), nil
}

// authenticateDeploy returns the principal token:<id> when secret is a deploy token: a token
// of kind deploy that is not revoked and not expired. The user name of the Basic credentials
// does not count (deploy by convention, anything for a client that insists on one) and is not
// logged. The credential of a token of one repo carries the repo, by id and by name: one
// GetTokenByHash and one GetRepoByID. A repo deleted takes its deploy tokens with it, so a
// token found has a repo. The credential of a token over a mask carries the mask, which names no
// repo: one GetTokenByHash (docs/spec/rbac.md §1).
func (a *Authenticator) authenticateDeploy(ctx context.Context, secret string) (Principal, Credential, error) {
	if !tokenShaped(secret, DeployPrefix) {
		return Principal{}, Credential{}, a.refuse(ctx, nil, "malformed token")
	}
	t, err := a.token(ctx, secret)
	if err != nil {
		return Principal{}, Credential{}, err
	}
	if reason := a.dead(t, TokenDeploy); reason != "" {
		return Principal{}, Credential{}, a.refuseToken(ctx, nil, t, reason)
	}
	if t.RepoMask != "" {
		a.touch(ctx, t)
		return Principal{Kind: KindToken, Name: strconv.FormatInt(t.ID, 10)}, Credential{
			Kind: CredDeploy, TokenID: t.ID, Mask: t.RepoMask, Scopes: slices.Clone(t.Scopes),
		}, nil
	}
	repo, err := a.index.GetRepoByID(ctx, *t.RepoID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return Principal{}, Credential{}, a.refuseToken(ctx, nil, t, "the repo is gone")
	case err != nil:
		return Principal{}, Credential{}, fmt.Errorf("auth: authenticate token %d: its repo: %w", t.ID, err)
	}
	a.touch(ctx, t)
	return Principal{Kind: KindToken, Name: strconv.FormatInt(t.ID, 10)}, Credential{
		Kind: CredDeploy, TokenID: t.ID, RepoID: repo.ID, Repo: repo.Format + "/" + repo.Name, Scopes: slices.Clone(t.Scopes),
	}, nil
}

// AuthenticateBearer returns the principal of Authorization: Bearer <secret> outside /v2 (the
// management API and the frontends; the bearer of /v2 is the JWT of #26, which the facade
// verifies): a personal access token, as authenticatePAT checks it, without a user name, whose
// principal is user:<owner>; or a deploy token, as authenticateDeploy checks it, token:<id>,
// which npm's _authToken and yarn's npmAuthToken send so (ADR 0012, docs/spec/rbac.md §1).
// A personal access token bound to a DPoP key is refused with ErrBound: it is taken only with
// its proof (AuthenticateDPoP). Anything else is refused.
//
// The token names its owner by id, which GetUserByID reads on every call, so that a disabled or
// deleted owner is refused at once and a renamed one keeps its tokens.
func (a *Authenticator) AuthenticateBearer(ctx context.Context, secret string) (Principal, Credential, error) {
	if strings.HasPrefix(secret, DeployPrefix) {
		return a.authenticateDeploy(ctx, secret)
	}
	t, u, err := a.bearerPAT(ctx, secret)
	if err != nil {
		return Principal{}, Credential{}, err
	}
	if t.DPoPJKT != "" {
		return Principal{}, Credential{}, a.refuseBound(ctx, u, t)
	}
	a.touch(ctx, t)
	return UserPrincipal(u), patCredential(t), nil
}

// AuthenticateDPoP returns the principal of Authorization: DPoP <secret> on /api/ (RFC 9449
// §7.1), whose proof the caller verified: a personal access token, as AuthenticateBearer checks
// it, bound to the DPoP key whose thumbprint is jkt, the proof's (docs/spec/rbac.md §10). A token
// bound to another key or to none, and a deploy token, are refused as ErrBadCredentials. With
// Options.IdleTimeout, a token that passed all that but whose last use is longer ago is refused
// with an *IdleError, which only the holder of the key learns; its use is not recorded.
func (a *Authenticator) AuthenticateDPoP(ctx context.Context, secret, jkt string) (Principal, Credential, error) {
	t, u, err := a.bearerPAT(ctx, secret)
	switch {
	case err != nil:
		return Principal{}, Credential{}, err
	case t.DPoPJKT == "":
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, "a token bound to no DPoP key, sent with a proof")
	case subtle.ConstantTimeCompare([]byte(t.DPoPJKT), []byte(jkt)) != 1:
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, "a token bound to another DPoP key")
	}
	if last, idle := a.idleSince(t); idle {
		_ = a.refuseToken(ctx, u, t, "a sign-in idle for longer than auth.login.idleTimeout")
		return Principal{}, Credential{}, &IdleError{TokenID: t.ID, LastUse: last, Timeout: a.idle}
	}
	a.touch(ctx, t)
	cred := patCredential(t)
	if t.AuthTime != nil {
		cred.SignIn = &SignIn{At: *t.AuthTime, AMR: slices.Clone(t.AMR)}
	}
	return UserPrincipal(u), cred, nil
}

// bearerPAT returns the personal access token whose secret is secret, of the token's shape, and
// its owner: a token of kind pat that is not revoked or expired, whose owner exists and is
// neither deleted nor disabled. A refusal wraps ErrBadCredentials; any other error is a failure
// of the index.
func (a *Authenticator) bearerPAT(ctx context.Context, secret string) (*index.Token, *index.User, error) {
	if !tokenShaped(secret, PATPrefix) {
		return nil, nil, a.refuse(ctx, nil, "malformed token")
	}
	t, err := a.token(ctx, secret)
	if err != nil {
		return nil, nil, err
	}
	if reason := a.dead(t, TokenPAT); reason != "" {
		return nil, nil, a.refuseToken(ctx, nil, t, reason)
	}
	u, err := a.index.GetUserByID(ctx, *t.UserID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil, a.refuseToken(ctx, nil, t, "unknown user")
	case err != nil:
		return nil, nil, fmt.Errorf("auth: authenticate token %d: its owner: %w", t.ID, err)
	case u.DeletedAt != nil:
		return nil, nil, a.refuseToken(ctx, u, t, "deleted")
	case u.Disabled:
		return nil, nil, a.refuseToken(ctx, u, t, "disabled")
	}
	if reason := unbounded(t, u); reason != "" {
		return nil, nil, a.refuseToken(ctx, u, t, reason)
	}
	return t, u, nil
}

// refuseBound logs the refusal of a token bound to a DPoP key that came without its proof, as
// refuseToken does, and returns ErrBound.
func (a *Authenticator) refuseBound(ctx context.Context, u *index.User, t *index.Token) error {
	_ = a.refuseToken(ctx, u, t, "a token bound to a DPoP key, without its proof")
	return ErrBound
}

// unbounded returns why the personal access token t of the user u cannot authenticate for its
// expiry, or "" when it can: only a service account's may have none (docs/spec/rbac.md §1). A
// replica of a release before service accounts refuses every one without an expiry.
func unbounded(t *index.Token, u *index.User) string {
	if t.ExpiresAt == nil && u.Kind != KindService {
		return "a personal access token without an expiry, of a user that is not a service account"
	}
	return ""
}

// patCredential is the credential of a personal access token.
func patCredential(t *index.Token) Credential {
	return Credential{Kind: CredPAT, TokenID: t.ID, UserID: *t.UserID, Scopes: slices.Clone(t.Scopes)}
}

// token returns the token whose secret is secret, or nil when there is none.
func (a *Authenticator) token(ctx context.Context, secret string) (*index.Token, error) {
	t, err := a.index.GetTokenByHash(ctx, HashSecret(secret))
	switch {
	case errors.Is(err, index.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("auth: authenticate a token: %w", err)
	}
	return t, nil
}

// dead returns why t cannot authenticate as a token of kind, or "" when it can: none, another
// kind, revoked, expired, a personal access token without an owner, a deploy token with neither
// a repo nor a mask, or with both, or with a mask that is not <format>/<repo>. The last four are
// rows this package never writes; a replica of a release before the masks (#911) refuses a token
// over a mask as one without a repo. A personal access token without an expiry is its owner's
// to decide (unbounded).
func (a *Authenticator) dead(t *index.Token, kind string) string {
	switch {
	case t == nil:
		return "unknown token"
	case t.Kind != kind:
		return "a token of kind " + t.Kind
	case t.RevokedAt != nil:
		return "revoked"
	case t.ExpiresAt != nil && t.ExpiresAt.Before(a.now()):
		return "expired"
	case kind == TokenPAT && t.UserID == nil:
		return "a personal access token without an owner"
	case kind == TokenDeploy && (t.RepoID == nil) == (t.RepoMask == ""):
		return "a deploy token with neither a repo nor a mask, or with both"
	case kind == TokenDeploy && t.RepoMask != "" && !validMask(t.RepoMask):
		return "a deploy token whose mask is not <format>/<repo>"
	}
	return ""
}

// validMask reports whether mask is <format>/<repo>, each segment a name or "*" (ParseMask).
func validMask(mask string) bool {
	_, _, err := ParseMask(mask)
	return err == nil
}

// refuseToken logs the refusal of a token at debug level, with the principal user:<uid> of
// the user u unless it is nil and the token's id when there is a token, never a user name as
// typed (refuse), the secret or its hash, and returns ErrBadCredentials.
func (a *Authenticator) refuseToken(ctx context.Context, u *index.User, t *index.Token, reason string) error {
	attrs := make([]any, 0, 6)
	if u != nil {
		attrs = append(attrs, "user", UserPrincipal(u).String())
	}
	if t != nil {
		attrs = append(attrs, "token", t.ID)
	}
	a.log.DebugContext(ctx, "authentication refused", append(attrs, "reason", reason)...)
	return ErrBadCredentials
}

// idleSince returns the last use of the token t, its last_used_at or else its created_at, and
// whether that is more than Options.IdleTimeout before now; never with IdleTimeout zero
// (docs/spec/rbac.md §10). The last use read is less than a minute behind (touch), so a token is
// refused after between the bound less a minute and the bound without use, never later.
func (a *Authenticator) idleSince(t *index.Token) (time.Time, bool) {
	last := t.CreatedAt
	if t.LastUsedAt != nil && t.LastUsedAt.After(last) {
		last = *t.LastUsedAt
	}
	return last, a.idle > 0 && a.now().Sub(last) > a.idle
}

// touch records the use of the token t now (TouchToken) when its last use, as read with it on
// this request, is a minute old or more, or none (docs/spec/rbac.md §10, #1277): every replica
// reads the same row, so the use is recorded at most once a minute per token on all replicas
// together, and less than a minute behind the last use. TouchToken only moves forward, so
// replicas that record at once move it back on none. This replica's last record, by token, keeps
// requests that read the same old value at once from writing it each: a record of the last
// minute leaves the row at least as late. A record that fails is logged at debug, refuses
// nothing, and is forgotten, so that the next request records it again.
func (a *Authenticator) touch(ctx context.Context, t *index.Token) {
	now := a.now()
	if t.LastUsedAt != nil && now.Sub(*t.LastUsedAt) < touchInterval {
		return
	}
	if !a.touches.putUnless(t.ID, now, func(last time.Time) bool { return now.Sub(last) < touchInterval }) {
		return
	}
	if err := a.index.TouchToken(ctx, t.ID, now); err != nil {
		a.touches.removeIf(t.ID, now.Equal) // a record made since stays
		a.log.DebugContext(ctx, "the use of a token is not recorded", "token", t.ID, "error", err.Error())
	}
}

// bounded is a map of at most size entries that evicts the one written longest ago; its
// methods may be called concurrently.
type bounded[K comparable, V any] struct {
	mu      sync.Mutex
	size    int
	entries map[K]*list.Element // of *boundedEntry[K, V]
	order   *list.List          // written longest ago first
}

type boundedEntry[K comparable, V any] struct {
	key   K
	value V
}

func newBounded[K comparable, V any](size int) *bounded[K, V] {
	return &bounded[K, V]{size: size, entries: map[K]*list.Element{}, order: list.New()}
}

// putUnless stores v as the value of k and reports true, unless k has a value for which keep
// reports true; then it changes nothing and reports false. A nil keep keeps nothing.
func (b *bounded[K, V]) putUnless(k K, v V, keep func(V) bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, ok := b.entries[k]; ok {
		if keep != nil && keep(el.Value.(*boundedEntry[K, V]).value) {
			return false
		}
		b.order.Remove(el)
	}
	b.push(k, v)
	return true
}

// push stores v as the value of k, written last, and evicts beyond the size; b.mu is held and
// k has no entry.
func (b *bounded[K, V]) push(k K, v V) {
	b.entries[k] = b.order.PushBack(&boundedEntry[K, V]{key: k, value: v})
	for b.order.Len() > b.size {
		oldest := b.order.Front()
		b.order.Remove(oldest)
		delete(b.entries, oldest.Value.(*boundedEntry[K, V]).key)
	}
}

// removeIf deletes the entry of k when match reports true for its value.
func (b *bounded[K, V]) removeIf(k K, match func(V) bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, ok := b.entries[k]; ok && match(el.Value.(*boundedEntry[K, V]).value) {
		b.order.Remove(el)
		delete(b.entries, k)
	}
}

// len returns the number of entries.
func (b *bounded[K, V]) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.order.Len()
}
