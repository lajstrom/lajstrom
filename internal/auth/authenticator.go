package auth

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// ErrBadCredentials means the credentials of a request were refused: an unknown user, a
// disabled one, one without a password, a wrong secret, or a token that is unknown, revoked,
// expired or not the user's. It does not say which: the caller answers 401 with a Basic
// challenge.
var ErrBadCredentials = errors.New("auth: bad credentials")

// Defaults of the verification cache.
const (
	// DefaultCacheTTL is how long a verified password is remembered.
	DefaultCacheTTL = 5 * time.Minute
	// DefaultCacheSize bounds the number of remembered verifications and of remembered token
	// uses, each.
	DefaultCacheSize = 10000
)

// touchInterval is how often at most the authenticator records the use of a token
// (tokens.last_used_at), per token and per replica.
const touchInterval = time.Minute

// Options configures an Authenticator.
type Options struct {
	// Index is where the users and the tokens are read, and the use of a token is recorded
	// (TouchToken); required.
	Index index.Store
	// Log receives the refusals, at debug level, with the user name or the token id and the
	// reason; nil means slog.Default().
	Log *slog.Logger
	// Now is the clock of the cache, of token expiry and of token use; nil means time.Now.
	Now func() time.Time
	// CacheTTL is how long a verified password is remembered; zero means DefaultCacheTTL.
	CacheTTL time.Duration
	// CacheSize bounds the remembered verifications, the oldest evicted first; zero means
	// DefaultCacheSize.
	CacheSize int
}

// Authenticator turns the credentials of a request into a principal (docs/spec/rbac.md §1).
// Create it with NewAuthenticator; its methods may be called concurrently.
//
// It reads the user from the index on every call, so that a user disabled on one replica is
// refused at once on every replica. The cost of argon2id stays off most requests through a
// verification cache: a user name and secret verified against a password hash are remembered
// for CacheTTL, by a sha256 of the pair (never the secret), and count only while the user's
// current hash is the one they were verified against. A new password, or a hash made with new
// parameters, is verified again at the next request.
//
// A token (docs/auth.md, "Tokens") is read from the index on every call too, by the sha256 of
// its secret, so that a revocation counts at the next request on every replica. It needs no
// cache and no argon2id: the secret is 256 random bits, and the lookup of its hash is the
// check. Its use is recorded at most once a minute.
type Authenticator struct {
	index index.Store
	log   *slog.Logger
	now   func() time.Time
	ttl   time.Duration
	size  int

	// verify is VerifyPassword; the tests count the calls.
	verify func(hash, password string) (bool, error)

	mu      sync.Mutex
	entries map[cacheKey]*list.Element // of *cacheEntry
	order   *list.List                 // oldest first

	// touches remembers when the use of each token was last recorded.
	touches *bounded[int64, time.Time]

	// logins counts the failed password verifications per account (accountKey).
	logins *throttle
}

// cacheKey is sha256(username ‖ 0x00 ‖ secret).
type cacheKey [sha256.Size]byte

// cacheEntry is a verification: the password hash the secret matched and when that stops
// counting.
type cacheEntry struct {
	key     cacheKey
	hash    string
	expires time.Time
}

// NewAuthenticator returns an Authenticator over o.Index. The first call of the process makes
// the dummy hash, one argon2id hash. It panics when o.Index is nil.
func NewAuthenticator(o Options) *Authenticator {
	if o.Index == nil {
		panic("auth: NewAuthenticator: nil Options.Index")
	}
	a := &Authenticator{
		index: o.Index, log: o.Log, now: o.Now, ttl: o.CacheTTL, size: o.CacheSize,
		verify:  VerifyPassword,
		entries: map[cacheKey]*list.Element{},
		order:   list.New(),
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	if a.ttl <= 0 {
		a.ttl = DefaultCacheTTL
	}
	if a.size <= 0 {
		a.size = DefaultCacheSize
	}
	a.touches = newBounded[int64, time.Time](a.size)
	a.logins = newThrottle(LoginFailures, LoginWindow, a.now)
	dummyHash() // now, so that the first unknown user does not pay for it
	return a
}

// dummyHash is the hash an unknown user's secret is verified against, so that "no such user"
// takes as long as "wrong password". It has the parameters of new hashes, and is made once per
// process, by the first NewAuthenticator.
var dummyHash = sync.OnceValue(func() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read does not return errors; it crashes the program instead
	return hashWith(hashParams, string(b))
})

// Authenticate returns the principal of HTTP Basic credentials, username and secret, and the
// credential it authenticated with. The prefix of the secret alone chooses the path:
//
//   - PATPrefix: a personal access token of the user username (authenticatePAT); the
//     principal is user:<username>, the credential a CredPAT.
//   - DeployPrefix: a deploy token, whatever username is (deploy, or anything for the clients
//     that insist on a user name: authenticateDeploy); the principal is token:<id>, the
//     credential a CredDeploy.
//   - Anything else: the user's password (authenticatePassword); the principal is
//     user:<username>, the credential a CredPassword.
//
// Every refusal is an error wrapping ErrBadCredentials, its reason logged at debug level with
// the user name or the token id, never anything about the secret. Any other error is a failure
// to decide (the index is down, or the stored hash is malformed), which the caller answers 500.
func (a *Authenticator) Authenticate(ctx context.Context, username, secret string) (Principal, Credential, error) {
	switch {
	case strings.HasPrefix(secret, PATPrefix):
		return a.authenticatePAT(ctx, username, secret)
	case strings.HasPrefix(secret, DeployPrefix):
		return a.authenticateDeploy(ctx, secret)
	}
	p, err := a.authenticatePassword(ctx, username, secret)
	if err != nil {
		return Principal{}, Credential{}, err
	}
	return p, Credential{Kind: CredPassword}, nil
}

// authenticatePassword returns the principal user:<uid> of the user called username when secret is its
// password. An unknown user, a disabled one, one without a password, and a wrong secret are
// each an error wrapping ErrBadCredentials, after the same argon2id work, and each counts as a
// failed login of the account. An account with LoginFailures of them in its window is refused
// with a *ThrottledError before its secret is looked at, the right password too: the answer
// tells nothing about the secret. A name that no user has is throttled by the name, alike, so
// that the throttle does not tell whether a user exists either.
func (a *Authenticator) authenticatePassword(ctx context.Context, username, secret string) (Principal, error) {
	if CheckName(username) != nil {
		// No user has such a name: nothing about a user is told by refusing it at once. It is
		// not logged, in case it is a password typed into the wrong field.
		return Principal{}, a.refuse(ctx, "", "malformed user name")
	}
	if secret == "" || len(secret) > MaxPasswordLen {
		return Principal{}, a.refuse(ctx, username, "malformed password")
	}
	u, err := a.index.GetUser(ctx, username)
	switch {
	case errors.Is(err, index.ErrNotFound):
		u = nil
	case err != nil:
		return Principal{}, fmt.Errorf("auth: authenticate %s: %w", username, err)
	}
	account := accountKey(u, username)
	if wait := a.logins.wait(account); wait > 0 {
		te := &ThrottledError{Throttle: "account", Account: "unknown", RetryAfter: wait}
		if u != nil {
			te.Account = account
		}
		return Principal{}, te
	}
	reason := ""
	switch {
	case u == nil:
		reason = "unknown user"
	case u.Disabled:
		reason = "disabled"
	case u.PasswordHash == "":
		reason = "no password"
	}
	if reason != "" {
		a.dummyVerify(secret)
		a.logins.add(account)
		return Principal{}, a.refuse(ctx, username, reason)
	}
	key := keyOf(username, secret)
	if a.cached(key, u.PasswordHash) {
		return UserPrincipal(u), nil
	}
	ok, err := a.verify(u.PasswordHash, secret)
	switch {
	case err != nil:
		return Principal{}, fmt.Errorf("auth: authenticate %s: the stored password hash: %w", username, err)
	case !ok:
		a.logins.add(account)
		return Principal{}, a.refuse(ctx, username, "wrong password")
	}
	a.remember(key, u.PasswordHash)
	return UserPrincipal(u), nil
}

// accountKey is the key of the throttle of failed logins of the user u, called username:
// user:<uid>, which a rename keeps; for a name that no user has (u nil), name:<username>.
func accountKey(u *index.User, username string) string {
	if u == nil {
		return "name:" + username
	}
	return UserPrincipal(u).String()
}

// refuse logs a refusal at debug level, with the user name unless it is empty, and returns
// ErrBadCredentials.
func (a *Authenticator) refuse(ctx context.Context, username, reason string) error {
	if username == "" {
		a.log.DebugContext(ctx, "authentication refused", "reason", reason)
	} else {
		a.log.DebugContext(ctx, "authentication refused", "user", username, "reason", reason)
	}
	return ErrBadCredentials
}

// dummyVerify spends the time of one verification on a hash that no secret matches.
func (a *Authenticator) dummyVerify(secret string) {
	_, _ = a.verify(dummyHash(), secret)
}

// keyOf returns the cache key of a user name and a secret.
func keyOf(username, secret string) cacheKey {
	b := make([]byte, 0, len(username)+1+len(secret))
	b = append(append(append(b, username...), 0), secret...)
	return sha256.Sum256(b)
}

// cached reports whether key was verified against hash and has not expired. An entry that has
// expired or was verified against another hash is dropped.
func (a *Authenticator) cached(key cacheKey, hash string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	el, ok := a.entries[key]
	if !ok {
		return false
	}
	e := el.Value.(*cacheEntry)
	if e.hash != hash || !a.now().Before(e.expires) {
		a.order.Remove(el)
		delete(a.entries, key)
		return false
	}
	return true
}

// remember records that key was verified against hash, evicting the oldest entries beyond
// the size bound.
func (a *Authenticator) remember(key cacheKey, hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.entries[key]; ok {
		a.order.Remove(el)
	}
	a.entries[key] = a.order.PushBack(&cacheEntry{key: key, hash: hash, expires: a.now().Add(a.ttl)})
	for a.order.Len() > a.size {
		oldest := a.order.Front()
		a.order.Remove(oldest)
		delete(a.entries, oldest.Value.(*cacheEntry).key)
	}
}

// cacheLen returns the number of remembered verifications.
func (a *Authenticator) cacheLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.order.Len()
}
