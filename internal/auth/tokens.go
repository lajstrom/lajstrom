package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Kinds of tokens (tokens.kind).
const (
	// TokenPAT is a personal access token: it acts as its user.
	TokenPAT = "pat"
	// TokenDeploy is a deploy token: a principal of its own, token:<id>, bound to one repo.
	TokenDeploy = "deploy"
)

// Scopes of tokens: the actions a token may get (docs/spec/rbac.md §1).
const (
	ScopeRead  = string(ActionRead)
	ScopeWrite = string(ActionWrite)
)

// Bounds of tokens (docs/spec/rbac.md §1). They are constants, not configuration.
const (
	// DefaultPATLifetime is the lifetime of a personal access token created without an expiry.
	DefaultPATLifetime = 90 * 24 * time.Hour
	// MaxPATLifetime bounds the lifetime of a personal access token.
	MaxPATLifetime = 365 * 24 * time.Hour
	// MaxTokenNameLen bounds the name of a token, in bytes.
	MaxTokenNameLen = 64
)

// Audit actions of the token changes (docs/spec/rbac.md §5).
const (
	ActionTokenCreate = "token.create"
	ActionTokenRevoke = "token.revoke"
)

// Token is a personal access token or a deploy token as the tokens API and lajstrom admin
// token answer it (docs/api.md). It never carries the secret or its hash, only the first
// HashPrefixLen characters of the hash, to tell tokens apart.
type Token struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"` // pat or deploy
	// User is the owner of a personal access token; Repo the <format>/<name> of a deploy
	// token.
	User string `json:"user,omitempty"`
	Repo string `json:"repo,omitempty"`
	Name string `json:"name"`
	// Scopes are read and/or write; empty on a personal access token, which then has its user's
	// full rights.
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expiresAt"` // null for a deploy token without an expiry
	LastUsedAt *time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	HashPrefix string     `json:"hashPrefix"`
}

// CreatedToken is the answer of a creation, the one time the secret is shown: the token and its
// secret.
type CreatedToken struct {
	Token
	Secret string `json:"secret"`
}

// Expiry is when a new token expires: At, or In from now, or neither for the default (90 days
// for a personal access token, never for a deploy token). Both is an error.
type Expiry struct {
	At time.Time     // zero when not given
	In time.Duration // zero when not given
}

// Tokens manages the personal access tokens and the deploy tokens of the control plane
// (docs/spec/index-schema.md, tokens). lajstrom admin token and the tokens API go through it,
// so that the validation and the audit events exist once, in the shape of Users. Every change
// is one audit event by PrincipalFrom(ctx). Create it as a struct literal; its methods may be
// called concurrently.
//
// A secret is made by the creation, returned by it once, and never stored, logged, audited or
// returned again: the index has its sha256, every later view the first HashPrefixLen
// characters of that.
//
// Errors for the caller wrap ErrInvalid (a field that breaks its rule; the message starts with
// the field), index.ErrNotFound (no such user, repo or token) or index.ErrConflict (the name
// is taken by a live token of the same owner), with a message for the person; any other error
// is a failure of the index.
type Tokens struct {
	// Index is the index database; required.
	Index index.Store
	// Audit records the events; nil means each event goes to the index in the transaction of
	// its change.
	Audit audit.Writer
	// Now is the clock of createdAt, of the expiry and of the audit events; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// CreatePAT creates a personal access token of the user called user, and returns it with its
// secret, PATPrefix and 43 characters, with one token.create audit event. name is 1 to
// MaxTokenNameLen bytes of printable UTF-8, unique among the user's live tokens (neither
// revoked nor expired). scopes are empty, for the user's full rights, or read and/or write.
// The expiry is mandatory: DefaultPATLifetime when exp gives none, at most MaxPATLifetime
// ahead. The user need not be enabled. The credential of ctx must be one that may create it
// (MayCreatePAT).
func (s *Tokens) CreatePAT(ctx context.Context, user, name string, scopes []string, exp Expiry, o Origin) (*Token, string, error) {
	if err := MayCreatePAT(ctx); err != nil {
		return nil, "", err
	}
	scopes, err := checkScopes(scopes, TokenPAT)
	if err != nil {
		return nil, "", err
	}
	return s.create(ctx, TokenPAT, "user "+user, name, scopes, exp, o, func(w index.Writer) (*index.Token, tokenOwner, error) {
		u, err := w.GetUser(ctx, user)
		if errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, userError("create a token of", user, err)
		}
		if err != nil {
			return nil, tokenOwner{}, err
		}
		return &index.Token{UserID: &u.ID}, tokenOwner{user: u.Name, uid: u.UID}, nil
	})
}

// MayCreatePAT returns nil when the credential of ctx may create a personal access token: a
// password, an OIDC login (docs/spec/rbac.md §8), or none, as from the command line, which the
// operator runs. A personal access
// token, with scopes or without, and a deploy token may not, an error wrapping ErrScope: a
// stolen token would otherwise mint tokens that outlive its revocation (decided on #249,
// docs/auth.md, Rules).
func MayCreatePAT(ctx context.Context) error {
	switch CredentialFrom(ctx).Kind {
	case CredPAT, CredDeploy:
		return &authError{msg: "a personal access token is created with a password, not with a token", err: ErrScope}
	case CredPassword, CredOIDC:
	}
	return nil
}

// createdBy is the credential c as the createdBy of a token.create event: password, oidc, or
// token:<id> for a token; "" for none.
func createdBy(c Credential) string {
	switch c.Kind {
	case CredPassword, CredOIDC:
		return string(c.Kind)
	case CredPAT, CredDeploy:
		return "token:" + strconv.FormatInt(c.TokenID, 10)
	}
	return ""
}

// CreateDeploy creates a deploy token of the repo <format>/<name> and returns it with its
// secret, DeployPrefix and 43 characters, with one token.create audit event whose repo is the
// repo. name is as for CreatePAT, unique among the repo's live tokens. scopes are read, write
// or both, required: delete and manage are never a deploy token's. A group's deploy token reads
// only: a write through a group is its hosted member's, which the token does not reach
// (docs/spec/rbac.md §7). The expiry is optional, and not bounded.
func (s *Tokens) CreateDeploy(ctx context.Context, repo, name string, scopes []string, exp Expiry, o Origin) (*Token, string, error) {
	scopes, err := checkScopes(scopes, TokenDeploy)
	if err != nil {
		return nil, "", err
	}
	formatName, repoName, err := splitRepo(repo)
	if err != nil {
		return nil, "", err
	}
	return s.create(ctx, TokenDeploy, "repo "+repo, name, scopes, exp, o, func(w index.Writer) (*index.Token, tokenOwner, error) {
		r, err := w.GetRepo(ctx, formatName, repoName)
		if errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, repoError(repo, err)
		}
		if err != nil {
			return nil, tokenOwner{}, err
		}
		if r.Kind == index.KindGroup && slices.Contains(scopes, ScopeWrite) {
			return nil, tokenOwner{}, invalidf("scopes: %s is a group, and its deploy token reads only: a write through it is "+
				"its hosted member's, which the token would not reach", repo)
		}
		return &index.Token{RepoID: &r.ID}, tokenOwner{repo: repo}, nil
	})
}

// tokenOwner is the owner of a token: the name and the uid of the user of a personal access
// token, or the repo <format>/<name> of a deploy token.
type tokenOwner struct {
	user, uid, repo string
}

// create checks name and the expiry and creates a token of kind with scopes in one
// transaction with its token.create event, whose details name the credential of ctx
// (createdBy). owner names the owner for the messages, user
// <name> or repo <format>/<name>; read reads it in the transaction and returns the row with
// its owner set, and the owner.
func (s *Tokens) create(ctx context.Context, kind, owner, name string, scopes []string, exp Expiry, o Origin,
	read func(index.Writer) (*index.Token, tokenOwner, error)) (*Token, string, error) {
	if err := checkTokenName(name); err != nil {
		return nil, "", err
	}
	now := s.now()
	expiresAt, err := expiry(kind, exp, now)
	if err != nil {
		return nil, "", err
	}
	prefix := PATPrefix
	if kind == TokenDeploy {
		prefix = DeployPrefix
	}
	secret := newSecret(prefix)
	var (
		row *index.Token
		own tokenOwner
		ev  index.AuditEvent
	)
	err = s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if row, own, err = read(w); err != nil {
			return err
		}
		live, err := w.ListTokens(ctx, row.UserID, row.RepoID)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(live, func(t index.Token) bool { return t.Kind == kind && t.Name == name && alive(&t, now) }) {
			return &authError{msg: fmt.Sprintf("name: %q is taken by a live token of %s", name, owner), err: index.ErrConflict}
		}
		row.Kind, row.Name, row.Hash, row.Scopes, row.ExpiresAt, row.CreatedAt = kind, name, HashSecret(secret), scopes, expiresAt, now
		if err = w.CreateToken(ctx, row); err != nil {
			return err
		}
		details := map[string]any{"id": row.ID, "kind": kind, "name": name, "scopes": scopes, "expiresAt": expiresAt}
		if by := createdBy(CredentialFrom(ctx)); by != "" {
			details["createdBy"] = by
		}
		ev, err = s.record(ctx, w, ActionTokenCreate, own, o, details)
		return err
	})
	if err != nil {
		return nil, "", tokenError("create a token of "+owner, err)
	}
	s.writeAudit(ctx, ev)
	t := tokenResource(row, own.user, own.repo)
	return &t, secret, nil
}

// List returns the personal access tokens of the user called user, in the order of their
// creation, the revoked and expired ones included.
func (s *Tokens) List(ctx context.Context, user string) ([]Token, error) {
	u, err := s.Index.GetUser(ctx, user)
	if err != nil {
		return nil, userError("list the tokens of", user, err)
	}
	rows, err := s.Index.ListTokens(ctx, &u.ID, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: list the tokens of user %s: %w", user, err)
	}
	return resources(rows, TokenPAT, u.Name, ""), nil
}

// ListDeploy returns the deploy tokens of the repo <format>/<name>, in the order of their
// creation, the revoked and expired ones included.
func (s *Tokens) ListDeploy(ctx context.Context, repo string) ([]Token, error) {
	formatName, repoName, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	r, err := s.Index.GetRepo(ctx, formatName, repoName)
	if err != nil {
		return nil, repoError(repo, err)
	}
	rows, err := s.Index.ListTokens(ctx, nil, &r.ID)
	if err != nil {
		return nil, fmt.Errorf("auth: list the deploy tokens of %s: %w", repo, err)
	}
	return resources(rows, TokenDeploy, "", repo), nil
}

// Get returns the token with id, of any kind and owner.
func (s *Tokens) Get(ctx context.Context, id int64) (*Token, error) {
	row, own, err := find(ctx, s.Index, id)
	if err != nil {
		return nil, tokenError(fmt.Sprintf("get token %d", id), err)
	}
	t := tokenResource(row, own.user, own.repo)
	return &t, nil
}

// Revoke revokes the token with id, of any kind and owner, with one token.revoke audit event:
// the authenticator refuses it from the next request on, on every replica. A token revoked
// already stays as it is, without an event. An unknown id is an error wrapping
// index.ErrNotFound.
func (s *Tokens) Revoke(ctx context.Context, id int64, o Origin) error {
	var ev *index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		row, own, err := find(ctx, w, id)
		if err != nil || row.RevokedAt != nil {
			return err
		}
		now := s.now()
		if err := w.RevokeToken(ctx, id, now); err != nil {
			return err
		}
		e, err := s.record(ctx, w, ActionTokenRevoke, own, o, map[string]any{"id": row.ID, "kind": row.Kind, "name": row.Name})
		ev = &e
		return err
	})
	if err != nil {
		return tokenError(fmt.Sprintf("revoke token %d", id), err)
	}
	if ev != nil {
		s.writeAudit(ctx, *ev)
	}
	return nil
}

// find returns the token with id and its owner. The index has no GetTokenByID: it lists the
// tokens; control-plane tables are small, and these are rare operations.
func find(ctx context.Context, r index.Reader, id int64) (*index.Token, tokenOwner, error) {
	rows, err := r.ListTokens(ctx, nil, nil)
	if err != nil {
		return nil, tokenOwner{}, err
	}
	i := slices.IndexFunc(rows, func(t index.Token) bool { return t.ID == id })
	if i < 0 {
		return nil, tokenOwner{}, &authError{msg: fmt.Sprintf("token %d does not exist", id), err: index.ErrNotFound}
	}
	row := &rows[i]
	switch {
	case row.UserID != nil:
		u, err := r.GetUserByID(ctx, *row.UserID)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, err
		}
		if err == nil {
			name := u.Name
			if u.DeletedAt != nil {
				name = u.DeletedName
			}
			return row, tokenOwner{user: name, uid: u.UID}, nil
		}
	case row.RepoID != nil:
		repo, err := r.GetRepoByID(ctx, *row.RepoID)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, err
		}
		if err == nil {
			return row, tokenOwner{repo: repo.Format + "/" + repo.Name}, nil
		}
	}
	return row, tokenOwner{}, nil // an owner gone takes its tokens with it; no row is left like this
}

// alive reports whether t is neither revoked nor expired at now.
func alive(t *index.Token, now time.Time) bool {
	return t.RevokedAt == nil && (t.ExpiresAt == nil || !t.ExpiresAt.Before(now))
}

// checkTokenName checks the name of a token: 1 to MaxTokenNameLen bytes of printable UTF-8.
func checkTokenName(name string) error {
	switch {
	case name == "":
		return invalidf("name: must not be empty")
	case len(name) > MaxTokenNameLen:
		return invalidf("name: must be at most %d bytes", MaxTokenNameLen)
	case !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
		return invalidf("name: %q must be printable UTF-8", name)
	}
	return nil
}

// checkScopes checks the scopes of a token of kind and returns them without repeats, read
// before write. A personal access token may have none; a deploy token needs one.
func checkScopes(scopes []string, kind string) ([]string, error) {
	var read, write bool
	for _, sc := range scopes {
		switch sc {
		case ScopeRead:
			read = true
		case ScopeWrite:
			write = true
		default:
			if kind == TokenDeploy {
				return nil, invalidf("scopes: %q is not a scope of a deploy token: it gets read, write or both, never delete or manage", sc)
			}
			return nil, invalidf("scopes: %q is not a scope: a personal access token narrows its user to read, write or both", sc)
		}
	}
	out := []string{}
	if read {
		out = append(out, ScopeRead)
	}
	if write {
		out = append(out, ScopeWrite)
	}
	if kind == TokenDeploy && len(out) == 0 {
		return nil, invalidf("scopes: a deploy token needs read, write or both")
	}
	return out, nil
}

// expiry returns the expiry of a new token of kind at now: exp.At, or now plus exp.In, or the
// default of the kind. It is in the future, and for a personal access token at most
// MaxPATLifetime ahead.
func expiry(kind string, exp Expiry, now time.Time) (*time.Time, error) {
	field := "expiresAt"
	var at time.Time
	switch {
	case !exp.At.IsZero() && exp.In != 0:
		return nil, invalidf("expiresAt and expiresIn: give one of them, not both")
	case exp.In < 0:
		return nil, invalidf("expiresIn: must be positive")
	case exp.In > 0:
		field, at = "expiresIn", now.Add(exp.In)
	case !exp.At.IsZero():
		at = exp.At.UTC()
	case kind == TokenPAT:
		at = now.Add(DefaultPATLifetime)
	default:
		return nil, nil // a deploy token without an expiry
	}
	switch {
	case !at.After(now):
		return nil, invalidf("%s: must be in the future", field)
	case kind == TokenPAT && at.After(now.Add(MaxPATLifetime)):
		return nil, invalidf("%s: a personal access token expires at most %d days ahead, at %s at the latest",
			field, MaxPATLifetime/(24*time.Hour), now.Add(MaxPATLifetime).Format(time.RFC3339))
	}
	return &at, nil
}

// maxLifetimeDays bounds the number of days ParseLifetime reads, far beyond any token's
// lifetime and far below an overflow of time.Duration.
const maxLifetimeDays = 100000

// ParseLifetime parses the lifetime of a token, expiresIn of the API and --expires of the CLI:
// a positive whole number of days, 30d, or a positive duration of time.ParseDuration, 12h or
// 90m. Errors wrap ErrInvalid; the caller names the field.
func ParseLifetime(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseUint(days, 10, 32)
		if err != nil || n == 0 || n > maxLifetimeDays {
			return 0, invalidf("%q is not a positive number of days, like 30d, or a duration, like 12h", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, invalidf("%q is not a positive number of days, like 30d, or a duration, like 12h", s)
	}
	return d, nil
}

// splitRepo splits <format>/<name>.
func splitRepo(repo string) (formatName, name string, err error) {
	formatName, name, ok := strings.Cut(repo, "/")
	if !ok || formatName == "" || name == "" || strings.Contains(name, "/") {
		return "", "", invalidf("repo: %q is not <format>/<name>", repo)
	}
	return formatName, name, nil
}

// repoError turns an error about the repo into the one the callers answer.
func repoError(repo string, err error) error {
	if errors.Is(err, index.ErrNotFound) {
		return &authError{msg: fmt.Sprintf("repo %s does not exist", repo), err: err}
	}
	return fmt.Errorf("auth: repo %s: %w", repo, err)
}

// tokenError turns an error of a token operation into the one the callers answer: an
// *authError stays as it is, anything else is wrapped with op, for the log.
func tokenError(op string, err error) error {
	var ae *authError
	if errors.As(err, &ae) {
		return err
	}
	return fmt.Errorf("auth: %s: %w", op, err)
}

// subjectOf is the subject of the audit event of a token: user:<uid> for a personal access
// token, <format>/<name> for a deploy token.
func subjectOf(own tokenOwner) string {
	if own.uid != "" {
		return Principal{Kind: KindUser, Name: own.uid}.String()
	}
	return own.repo
}

// resources returns the Tokens of rows of kind, whose owner is user or repo.
func resources(rows []index.Token, kind, user, repo string) []Token {
	out := make([]Token, 0, len(rows))
	for i := range rows {
		if rows[i].Kind == kind {
			out = append(out, tokenResource(&rows[i], user, repo))
		}
	}
	return out
}

// tokenResource returns the Token of a tokens row whose owner is user or repo, with UTC times.
func tokenResource(t *index.Token, user, repo string) Token {
	scopes := slices.Clone(t.Scopes)
	if scopes == nil {
		scopes = []string{}
	}
	return Token{
		ID: t.ID, Kind: t.Kind, User: user, Repo: repo, Name: t.Name, Scopes: scopes,
		ExpiresAt: utc(t.ExpiresAt), LastUsedAt: utc(t.LastUsedAt), CreatedAt: t.CreatedAt.UTC(), RevokedAt: utc(t.RevokedAt),
		HashPrefix: t.Hash[:min(HashPrefixLen, len(t.Hash))],
	}
}

// utc returns a copy of t in UTC, or nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// record builds the audit event of a change of a token of own, and appends it on w when there is
// no audit.Writer (recordAudit).
func (s *Tokens) record(ctx context.Context, w index.Writer, action string, own tokenOwner, o Origin, details map[string]any) (index.AuditEvent, error) {
	ev := index.AuditEvent{At: s.now(), Action: action, Repo: own.repo, Subject: subjectOf(own)}
	return recordAudit(ctx, w, s.Audit, ev, o, details)
}

// writeAudit passes ev to the audit.Writer after the commit of its change (writeAuditEvent).
func (s *Tokens) writeAudit(ctx context.Context, ev index.AuditEvent) {
	writeAuditEvent(ctx, s.Audit, s.log(), ev, "auth: audit event of a token change not written", "subject")
}

func (s *Tokens) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Tokens) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
