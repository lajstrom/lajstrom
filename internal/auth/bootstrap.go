package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The bootstrap token (docs/auth.md, Bootstrap): the token of auth.bootstrap.tokenFile, which
// the operator makes, valid on every replica while no enabled user has an admin binding and
// exchanged once for the first admin. serve never makes one and never logs one.
const (
	// MinBootstrapTokenLen and MaxBootstrapTokenLen bound a token of auth.bootstrap.tokenFile,
	// in bytes, so that it is not a word chosen by hand: openssl rand -hex 32 makes 64.
	MinBootstrapTokenLen = 32
	MaxBootstrapTokenLen = 256
	// BootstrapFailures is how many exchanges with a wrong token a client address may make
	// within LoginWindow. Beyond it, its exchanges are refused with a *ThrottledError of the
	// throttle "bootstrap", the right token too, until the window has passed. A context
	// without a client address (WithClientAddress) counts in no throttle, as for failed logins:
	// one budget for every client would let anyone's wrong tokens refuse the right one (#761),
	// and a token of MinBootstrapTokenLen bytes or more is not guessed one request at a time.
	BootstrapFailures = 10
)

// Errors of an exchange of the bootstrap token.
var (
	// ErrBootstrapClosed means that an enabled user has an admin binding: the token is dead
	// whatever it is, and the API answers 404.
	ErrBootstrapClosed = errors.New("auth: no bootstrap: an admin exists")
	// ErrBootstrapToken means that the token was refused: wrong, used or expired. Errors that
	// wrap it say which, and how to get a new one, only to the holder of the right token.
	ErrBootstrapToken = errors.New("auth: the bootstrap token is refused")
)

// Bootstrap makes the token of auth.bootstrap.tokenFile valid and exchanges it for the first
// admin. serve calls Arm at
// start, and the API's POST /api/v1/bootstrap calls Exchange. Create it as a struct literal;
// its methods may be called concurrently.
type Bootstrap struct {
	// Index is the index database; required.
	Index index.Store
	// Users creates the admin; required.
	Users *Users
	// TTL is how long a token is valid once Arm made it valid (auth.bootstrap.ttl); required.
	TTL time.Duration
	// Replica names this process in the row it makes, for the log of the others; empty means
	// the host name and the process id.
	Replica string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger

	once     sync.Once
	failures *throttle
}

// Arm makes the token of tokenFile the bootstrap token at start, when no enabled user has an
// admin binding. Without a token, tokenFile "" or a file that does not exist, nothing is made
// and a warning says how to make the first admin. A token that is valid already stays; the
// token of the file, once it is used or expired, is not made valid again: the file needs a new
// one. Nothing of a token is ever logged. When an admin exists Arm does nothing but warn while
// the file of tokenFile exists, without reading it. Errors are failures of the index, a file that cannot be
// read, and a token that ReadBootstrapToken refuses.
func (b *Bootstrap) Arm(ctx context.Context, tokenFile string) error {
	admin, err := anyAdmin(ctx, b.Index)
	if err != nil {
		return fmt.Errorf("auth: bootstrap: %w", err)
	}
	if admin {
		if _, err := os.Stat(tokenFile); tokenFile != "" && err == nil {
			b.log().WarnContext(ctx, "the bootstrap token is set but no longer valid, an admin exists: remove auth.bootstrap.tokenFile and its file",
				"auth.bootstrap.tokenFile", tokenFile)
		}
		return nil
	}
	if tokenFile == "" {
		b.noToken(ctx, "auth.bootstrap.tokenFile is not set")
		return nil
	}
	secret, err := ReadBootstrapToken(tokenFile)
	if errors.Is(err, fs.ErrNotExist) {
		b.noToken(ctx, "auth.bootstrap.tokenFile "+tokenFile+" does not exist")
		return nil
	}
	if err != nil {
		return err
	}
	for range 3 {
		if done, err := b.arm(ctx, tokenFile, secret); done || err != nil {
			return err
		}
	}
	return errors.New("auth: bootstrap: the token changed under every attempt to make it")
}

// noToken warns that no admin exists and no token makes one, why, and the two ways to make the
// first admin. It names no secret: the operator makes the token.
func (b *Bootstrap) noToken(ctx context.Context, why string) {
	b.log().WarnContext(ctx, "no admin yet, and no bootstrap token: "+why+". Write a token of your own (openssl rand -hex 32) "+
		"to a file, name it in auth.bootstrap.tokenFile, restart and run lajstrom admin bootstrap; "+
		"or, with the database, run lajstrom admin user create --admin")
}

// arm makes secret, the token of tokenFile, the bootstrap token, unless it is the stored one
// (Arm), and logs what it did, never the token. done is false when another replica made or
// replaced the row in between: the caller reads it again.
func (b *Bootstrap) arm(ctx context.Context, tokenFile, secret string) (done bool, err error) {
	now, hash := b.now(), HashSecret(secret)
	cur, err := b.Index.GetBootstrapToken(ctx)
	old := ""
	switch {
	case errors.Is(err, index.ErrNotFound):
	case err != nil:
		return false, fmt.Errorf("auth: bootstrap: %w", err)
	case cur.Hash == hash && cur.UsedAt == nil && now.Before(cur.ExpiresAt):
		b.log().InfoContext(ctx, "no admin yet: the bootstrap token of auth.bootstrap.tokenFile is valid; exchange it with lajstrom admin bootstrap",
			"auth.bootstrap.tokenFile", tokenFile, "madeBy", cur.Replica, "madeAt", cur.CreatedAt, "expiresAt", cur.ExpiresAt)
		return true, nil
	case cur.Hash == hash:
		b.log().WarnContext(ctx, "no admin yet, and the bootstrap token of auth.bootstrap.tokenFile is used or expired: "+
			"write a new token to the file and restart, or create the admin with lajstrom admin user create --admin",
			"auth.bootstrap.tokenFile", tokenFile, "expiresAt", cur.ExpiresAt, "usedAt", cur.UsedAt)
		return true, nil
	default:
		old = cur.Hash // another token: the file's replaces it
	}
	t := index.BootstrapToken{Hash: hash, Replica: b.replica(), CreatedAt: now, ExpiresAt: now.Add(b.TTL)}
	switch err := b.Index.PutBootstrapToken(ctx, &t, old); {
	case errors.Is(err, index.ErrConflict):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("auth: bootstrap: %w", err)
	}
	b.log().InfoContext(ctx, "no admin yet: the bootstrap token of auth.bootstrap.tokenFile is valid; exchange it with lajstrom admin bootstrap",
		"auth.bootstrap.tokenFile", tokenFile, "madeBy", t.Replica, "madeAt", t.CreatedAt, "expiresAt", t.ExpiresAt)
	return true, nil
}

// Open reports whether the exchange is open: no enabled user has an admin binding.
func (b *Bootstrap) Open(ctx context.Context) (bool, error) {
	admin, err := anyAdmin(ctx, b.Index)
	if err != nil {
		return false, fmt.Errorf("auth: bootstrap: %w", err)
	}
	return !admin, nil
}

// ReadBootstrapToken reads the token of auth.bootstrap.tokenFile: the file without the
// whitespace around it, MinBootstrapTokenLen to MaxBootstrapTokenLen bytes without whitespace.
// Errors name the file and never contain the token.
func ReadBootstrapToken(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator chooses the file
	if err != nil {
		return "", fmt.Errorf("auth: auth.bootstrap.tokenFile: %w", err)
	}
	s := strings.TrimSpace(string(b))
	switch {
	case len(s) < MinBootstrapTokenLen || len(s) > MaxBootstrapTokenLen:
		return "", fmt.Errorf("auth: auth.bootstrap.tokenFile %s: the token must be %d to %d bytes, it has %d",
			path, MinBootstrapTokenLen, MaxBootstrapTokenLen, len(s))
	case strings.ContainsAny(s, " \t\r\n"):
		return "", fmt.Errorf("auth: auth.bootstrap.tokenFile %s: the token must be one word, without whitespace", path)
	}
	return s, nil
}

// Exchange creates the first admin, name with password and the admin binding on */*, for the
// bootstrap token secret, and marks the token used, in one transaction: the token makes one
// admin, on any replica. The audit event is user.create with o's source. In this order:
//
//   - a client address with BootstrapFailures wrong tokens in its window: a *ThrottledError;
//     a context without an address is never throttled;
//   - an enabled user with an admin binding: ErrBootstrapClosed, whatever the token;
//   - a token that is not the one stored, compared in constant time: an error wrapping
//     ErrBootstrapToken, and it counts as a failure of the address, when there is one;
//   - the right token, used or expired: an error wrapping ErrBootstrapToken that says so;
//   - then the name and the password are checked (ErrInvalid), and a name that exists is
//     index.ErrConflict, the token left valid.
func (b *Bootstrap) Exchange(ctx context.Context, secret, name, password string, o Origin) (*User, error) {
	addr := throttleAddress(ctx)
	if err := b.throttled(addr); err != nil {
		return nil, err
	}
	admin, err := anyAdmin(ctx, b.Index)
	switch {
	case err != nil:
		return nil, fmt.Errorf("auth: bootstrap: %w", err)
	case admin:
		return nil, ErrBootstrapClosed
	}
	hash := HashSecret(secret)
	cur, err := b.Index.GetBootstrapToken(ctx)
	switch {
	case errors.Is(err, index.ErrNotFound):
	case err != nil:
		return nil, fmt.Errorf("auth: bootstrap: %w", err)
	}
	if cur == nil || subtle.ConstantTimeCompare([]byte(hash), []byte(cur.Hash)) != 1 {
		b.failed(addr)
		b.log().InfoContext(ctx, "bootstrap token refused: not the token", "address", addr)
		return nil, &authError{msg: "the bootstrap token is refused", err: ErrBootstrapToken}
	}
	now := b.now()
	if err := tokenValid(cur, now); err != nil {
		return nil, err
	}
	u, err := b.Users.create(ctx, name, password, true, o, func(w index.Writer) error {
		switch admin, err := anyAdmin(ctx, w); {
		case err != nil:
			return err
		case admin:
			return ErrBootstrapClosed
		}
		if err := w.UseBootstrapToken(ctx, hash, now); errors.Is(err, index.ErrNotFound) {
			return &authError{msg: "the bootstrap token was used in between", err: ErrBootstrapToken}
		} else if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.log().InfoContext(ctx, "the first admin is made with the bootstrap token", "user", u.Name, "uid", u.UID, "address", addr)
	return u, nil
}

// tokenValid returns nil when the right token t is valid at now, or the error that says when it
// was used or expired and how to get another.
func tokenValid(t *index.BootstrapToken, now time.Time) error {
	switch {
	case t.UsedAt != nil:
		return &authError{msg: "the bootstrap token was used at " + t.UsedAt.UTC().Format(time.RFC3339), err: ErrBootstrapToken}
	case !now.Before(t.ExpiresAt):
		return &authError{msg: "the bootstrap token expired at " + t.ExpiresAt.UTC().Format(time.RFC3339) +
			": write a new token to auth.bootstrap.tokenFile and restart a replica, or create the admin with lajstrom admin user create --admin",
			err: ErrBootstrapToken}
	}
	return nil
}

// throttled returns the *ThrottledError of the client address addr, or nil when it has fewer
// than BootstrapFailures wrong tokens in its window or is "", which no throttle counts (#761).
func (b *Bootstrap) throttled(addr string) error {
	if wait := b.throttle().wait(addr); addr != "" && wait > 0 {
		return &ThrottledError{Throttle: "bootstrap", RetryAfter: wait}
	}
	return nil
}

// failed counts a wrong token against the client address addr, unless it is "".
func (b *Bootstrap) failed(addr string) {
	if addr != "" {
		b.throttle().add(addr)
	}
}

func (b *Bootstrap) throttle() *throttle {
	b.once.Do(func() { b.failures = newThrottle(BootstrapFailures, LoginWindow, b.now) })
	return b.failures
}

func (b *Bootstrap) replica() string {
	if b.Replica != "" {
		return b.Replica
	}
	host, _ := os.Hostname()
	return host + ":" + strconv.Itoa(os.Getpid())
}

func (b *Bootstrap) now() time.Time {
	if b.Now == nil {
		return time.Now().UTC()
	}
	return b.Now().UTC()
}

func (b *Bootstrap) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}
