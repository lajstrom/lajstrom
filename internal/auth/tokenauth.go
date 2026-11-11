package auth

import (
	"container/list"
	"context"
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
// without leeway), whose owner is the user called username and is not disabled. A malformed
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
	}
	if reason != "" {
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, reason)
	}
	a.touch(ctx, t.ID)
	return UserPrincipal(u), patCredential(t), nil
}

// authenticateDeploy returns the principal token:<id> when secret is a deploy token: a token
// of kind deploy that is not revoked and not expired. The user name of the Basic credentials
// does not count (deploy by convention, anything for a client that insists on one) and is not
// logged. The credential carries the token's repo, by id and by name: one GetTokenByHash and
// one GetRepoByID. A repo deleted takes its deploy tokens with it, so a token found has a repo.
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
	repo, err := a.index.GetRepoByID(ctx, *t.RepoID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return Principal{}, Credential{}, a.refuseToken(ctx, nil, t, "the repo is gone")
	case err != nil:
		return Principal{}, Credential{}, fmt.Errorf("auth: authenticate token %d: its repo: %w", t.ID, err)
	}
	a.touch(ctx, t.ID)
	return Principal{Kind: KindToken, Name: strconv.FormatInt(t.ID, 10)}, Credential{
		Kind: CredDeploy, TokenID: t.ID, RepoID: repo.ID, Repo: repo.Format + "/" + repo.Name, Scopes: slices.Clone(t.Scopes),
	}, nil
}

// AuthenticateBearer returns the principal of Authorization: Bearer <secret> outside /v2 (the
// management API and the frontends; the bearer of /v2 is the JWT of #26, which the facade
// verifies): a personal access token, as authenticatePAT checks it, without a user name, whose
// principal is user:<owner>; or a deploy token, as authenticateDeploy checks it, token:<id>,
// which npm's _authToken and yarn's npmAuthToken send so (ADR 0012, docs/spec/rbac.md §1).
// Anything else is refused.
//
// The token names its owner by id, which GetUserByID reads on every call, so that a disabled or
// deleted owner is refused at once and a renamed one keeps its tokens.
func (a *Authenticator) AuthenticateBearer(ctx context.Context, secret string) (Principal, Credential, error) {
	switch {
	case strings.HasPrefix(secret, DeployPrefix):
		return a.authenticateDeploy(ctx, secret)
	case !tokenShaped(secret, PATPrefix):
		return Principal{}, Credential{}, a.refuse(ctx, nil, "malformed token")
	}
	t, err := a.token(ctx, secret)
	if err != nil {
		return Principal{}, Credential{}, err
	}
	if reason := a.dead(t, TokenPAT); reason != "" {
		return Principal{}, Credential{}, a.refuseToken(ctx, nil, t, reason)
	}
	u, err := a.index.GetUserByID(ctx, *t.UserID)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return Principal{}, Credential{}, a.refuseToken(ctx, nil, t, "unknown user")
	case err != nil:
		return Principal{}, Credential{}, fmt.Errorf("auth: authenticate token %d: its owner: %w", t.ID, err)
	case u.DeletedAt != nil:
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, "deleted")
	case u.Disabled:
		return Principal{}, Credential{}, a.refuseToken(ctx, u, t, "disabled")
	}
	a.touch(ctx, t.ID)
	return UserPrincipal(u), patCredential(t), nil
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
// kind, revoked, expired, a personal access token without an expiry or an owner, a deploy token
// without a repo. The last three are rows this package never writes.
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
	case kind == TokenPAT && (t.ExpiresAt == nil || t.UserID == nil):
		return "a personal access token without an expiry or an owner"
	case kind == TokenDeploy && t.RepoID == nil:
		return "a deploy token without a repo"
	}
	return ""
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

// touch records the use of the token id now (TouchToken) when the last record of this replica
// is a minute old or more. A record that fails is logged at debug and does not refuse the
// request: last_used_at is for the person reading a token list, not for a decision.
func (a *Authenticator) touch(ctx context.Context, id int64) {
	now := a.now()
	if !a.touches.putUnless(id, now, func(last time.Time) bool { return now.Sub(last) < touchInterval }) {
		return
	}
	if err := a.index.TouchToken(ctx, id, now); err != nil {
		a.log.DebugContext(ctx, "the use of a token is not recorded", "token", id, "error", err.Error())
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

// len returns the number of entries.
func (b *bounded[K, V]) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.order.Len()
}
