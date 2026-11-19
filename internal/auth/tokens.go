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
	"github.com/lajstrom/lajstrom/internal/auth/dpop"
	"github.com/lajstrom/lajstrom/internal/index"
)

// Kinds of tokens (tokens.kind).
const (
	// TokenPAT is a personal access token: it acts as its user.
	TokenPAT = "pat"
	// TokenDeploy is a deploy token: a principal of its own, token:<id>, bound to one repo or to
	// a mask (docs/spec/rbac.md §1).
	TokenDeploy = "deploy"
)

// Scopes of tokens: the actions a token may get (docs/spec/rbac.md §1).
const (
	ScopeRead   = string(ActionRead)
	ScopeWrite  = string(ActionWrite)
	ScopeAttach = string(ActionAttach) // write holds it too (#89)
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

// SourceIdle is the source, and the reason, of the token.revoke row of a sign-in that the server
// revoked as idle (RevokeIdle, docs/spec/rbac.md §10).
const SourceIdle = "idle"

// Token is a personal access token or a deploy token as the tokens API and lajstrom admin
// token answer it (docs/api.md). It never carries the secret or its hash, only the first
// HashPrefixLen characters of the hash, to tell tokens apart.
type Token struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"` // pat or deploy
	// User is the owner of a personal access token; Repo the <format>/<name> of a deploy
	// token of one repo; RepoMask the mask of a deploy token over a mask, oci/*.
	User     string `json:"user,omitempty"`
	Repo     string `json:"repo,omitempty"`
	RepoMask string `json:"repoMask,omitempty"`
	Name     string `json:"name"`
	// Scopes are read and/or write; empty on a personal access token, which then has its user's
	// full rights.
	Scopes     []string   `json:"scopes"`
	ExpiresAt  *time.Time `json:"expiresAt"` // null for a deploy token without an expiry
	LastUsedAt *time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	HashPrefix string     `json:"hashPrefix"`
	// DPoPJKT is the JWK SHA-256 thumbprint of the DPoP key that the token of a web UI's sign-in
	// is bound to (docs/spec/rbac.md §10); absent for every other token.
	DPoPJKT string `json:"dpopJkt,omitempty"`
	// SignIn is the sign-in at the IdP of the token of a web UI's sign-in through OIDC
	// (docs/spec/rbac.md §10): when and how the person authenticated, so that a UI knows what it
	// meets before it asks; absent for every other token.
	SignIn *TokenSignIn `json:"signIn,omitempty"`
	// BeyondMaxTTL marks a live token beyond the bound of the configuration of its kind when it
	// is answered (Tokens.beyond, docs/spec/rbac.md §1): created before the bound was set or
	// lowered, it is to be replaced. Worked out from the configuration of the replica that
	// answers, stored nowhere; absent otherwise.
	BeyondMaxTTL bool `json:"beyondMaxTTL,omitempty"`
}

// TokenSignIn is a token's sign-in as the views of a token answer it: the auth_time, the amr and
// the acr of the ID token of its OIDC login, each when the token kept it.
type TokenSignIn struct {
	AuthTime *time.Time `json:"authTime,omitempty"`
	AMR      []string   `json:"amr,omitempty"`
	ACR      string     `json:"acr,omitempty"`
}

// CreatedToken is the answer of a creation, the one time the secret is shown: the token and its
// secret.
type CreatedToken struct {
	Token
	Secret string `json:"secret"`
}

// Expiry is when a new token expires: At, or In from now, or neither for the default (90 days
// for a person's personal access token, Tokens.ServiceMaxLifetime for a service account's,
// Tokens.DeployMaxLifetime for a deploy token, never when that is zero). Both is an error.
type Expiry struct {
	At time.Time     // zero when not given
	In time.Duration // zero when not given
	// Field names the expiry in the message of a refusal: --expires on the command line. Empty
	// names the field of the API that was given, expiresAt or expiresIn.
	Field string
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
	// Formats reports whether a format of that name is served (a compiled-in plugin, or oci):
	// the format of a deploy token's mask is one of them or "*", as a binding's. nil accepts
	// every name.
	Formats func(name string) bool
	// Audit records the events; nil means each event goes to the index in the transaction of
	// its change.
	Audit audit.Writer
	// Now is the clock of createdAt, of the expiry and of the audit events; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
	// ServiceMaxLifetime bounds the lifetime of a service account's personal access token
	// instead of MaxPATLifetime: auth.serviceAccounts.maxTokenTTL (docs/spec/rbac.md §1). A token
	// created without an expiry gets it. Zero bounds nothing, and a token created without an
	// expiry then never expires.
	ServiceMaxLifetime time.Duration
	// DeployMaxLifetime bounds the lifetime of a deploy token, of a repo and over a mask:
	// auth.deployTokens.maxTTL (docs/spec/rbac.md §1, #1286). A token created without an expiry
	// gets it. Zero bounds nothing, and a token created without an expiry then never expires.
	DeployMaxLifetime time.Duration
}

// CreatePAT creates a personal access token of the user called user, and returns it with its
// secret, PATPrefix and 43 characters, with one token.create audit event. name is 1 to
// MaxTokenNameLen bytes of printable UTF-8, unique among the user's live tokens (neither
// revoked nor expired). scopes are empty, for the user's full rights, or read, write and attach,
// one or more.
// The expiry is mandatory: DefaultPATLifetime when exp gives none, at most MaxPATLifetime
// ahead; a service account's is bounded by ServiceMaxLifetime instead (lifetime). The user need
// not be enabled. The credential of ctx must be one that may create it (MayCreatePAT): a
// service account has no password, so its tokens are created by an admin, with a password or a
// web UI's fresh sign-in, or on the command line.
func (s *Tokens) CreatePAT(ctx context.Context, user, name string, scopes []string, exp Expiry, o Origin) (*Token, string, error) {
	return s.createPAT(ctx, user, name, scopes, binding{}, exp, o)
}

// CreateBoundPAT creates a personal access token of the user called user as CreatePAT does,
// without scopes, bound to the DPoP key whose JWK SHA-256 thumbprint is jkt (docs/spec/rbac.md
// §10, RFC 9449): the Authenticator takes it only as Authorization: DPoP with a proof of that key
// (AuthenticateDPoP), and refuses it as a bearer and in Basic credentials (ErrBound). Its
// token.create event has dpopJkt. jkt must have the shape of a thumbprint.
func (s *Tokens) CreateBoundPAT(ctx context.Context, user, name, jkt string, exp Expiry, o Origin) (*Token, string, error) {
	if !dpop.ValidThumbprint(jkt) {
		return nil, "", invalidf("dpopJkt: must be a JWK SHA-256 thumbprint, 43 characters of base64url")
	}
	return s.createPAT(ctx, user, name, nil, binding{jkt: jkt}, exp, o)
}

// binding is what a web UI's token keeps beside the token (docs/spec/rbac.md §10): the thumbprint
// of the DPoP key it is bound to, "" for none, and the sign-in at the IdP of an OIDC login, nil
// for none.
type binding struct {
	jkt    string
	signIn *SignIn
	// refresh joins the token of an OIDC login to its refresh token (#1282).
	refresh LoginRefresh
}

// createPAT is CreatePAT, the token bound to the DPoP key of b unless its thumbprint is "", with
// the sign-in of b.
func (s *Tokens) createPAT(ctx context.Context, user, name string, scopes []string, b binding, exp Expiry, o Origin) (*Token, string, error) {
	if err := s.MayCreatePAT(ctx); err != nil {
		return nil, "", err
	}
	scopes, err := checkScopes(scopes, TokenPAT)
	if err != nil {
		return nil, "", err
	}
	return s.create(ctx, TokenPAT, "user "+user, name, scopes, exp, o, b.refresh.join(ctx), func(w index.Writer) (*index.Token, tokenOwner, error) {
		u, err := w.GetUser(ctx, user)
		if errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, userError("create a token of", user, err)
		}
		if err != nil {
			return nil, tokenOwner{}, err
		}
		row := &index.Token{UserID: &u.ID, DPoPJKT: b.jkt}
		if in := b.signIn; in != nil {
			row.AMR, row.ACR = slices.Clone(in.AMR), in.ACR
			if !in.At.IsZero() {
				at := in.At.UTC()
				row.AuthTime = &at
			}
		}
		return row, tokenOwner{user: u.Name, uid: u.UID, service: u.Kind == KindService}, nil
	})
}

// MayCreatePAT returns nil when the credential of ctx may create a personal access token: a
// password, an OIDC login (docs/spec/rbac.md §8), none, as from the command line, which the
// operator runs, or, in place of a password, the token of a web UI's sign-in through OIDC whose
// sign-in is fresh at s's now (inPlaceOfPassword, §12, #1323). Any other personal access token,
// with scopes or without, and a deploy token may not, an error wrapping ErrScope: a stolen token
// would otherwise mint tokens that outlive its revocation (decided on #249, docs/auth.md, Rules).
// Nor may a client certificate or a Kubernetes service-account token, which authenticate
// /v2/token only (docs/spec/rbac.md §9), nor a CI token, whose minutes must not become a secret
// that lasts (§11).
func (s *Tokens) MayCreatePAT(ctx context.Context) error {
	const rule = "a personal access token is created with a password or a web UI's fresh sign-in through OIDC, not with a token"
	switch cred := CredentialFrom(ctx); cred.Kind {
	case CredPAT:
		return s.inPlaceOfPassword(cred, rule)
	case CredDeploy, CredCertificate, CredKubernetes, CredCI:
		return &authError{msg: rule, err: ErrScope}
	case CredPassword, CredOIDC:
	}
	return nil
}

// MayCreateMaskDeploy returns nil when the credential of ctx may create a deploy token over a
// mask: a password, none, as from the command line, which writes the database and is the
// operator's (docs/spec/rbac.md §1, §3), or, in place of the password, the token of a web UI's
// sign-in through OIDC whose sign-in is fresh at s's now (inPlaceOfPassword, §12, #1323). Every
// other credential may not, an error wrapping ErrScope that names the rule: any other personal
// access token, with scopes or without, the admin's too, a deploy token, and an OIDC login,
// which is no credential of a request and has no password. A stolen token would otherwise mint a
// token that needs no expiry, covers every repo of its mask and outlives its revocation (decided
// on #911, 2026-11-14).
func (s *Tokens) MayCreateMaskDeploy(ctx context.Context) error {
	return s.passwordOrSignIn(ctx, "a deploy token over a mask is created with the admin's password or a web UI's fresh sign-in "+
		"through OIDC, not with a token")
}

// passwordOrSignIn is the rule of MayCreateMaskDeploy, which the approval of a request for a
// second admin takes too (Approvals.MayApprove, docs/spec/rbac.md §13): a password, none, as from
// the command line, or the token of a web UI's sign-in through OIDC whose sign-in is fresh
// (inPlaceOfPassword); any other credential is refused with rule, ErrScope.
func (s *Tokens) passwordOrSignIn(ctx context.Context, rule string) error {
	switch cred := CredentialFrom(ctx); cred.Kind {
	case CredPAT:
		return s.inPlaceOfPassword(cred, rule)
	case CredDeploy, CredOIDC, CredCertificate, CredKubernetes, CredCI:
		return &authError{msg: rule, err: ErrScope}
	case CredPassword:
	}
	return nil
}

// inPlaceOfPassword is the rule of a creation that takes a password, for cred, a personal access
// token (docs/spec/rbac.md §12, A fresh sign-in in place of a password; #1323): the token of a
// web UI's sign-in through OIDC stands for the password when its sign-in is fresh, its auth_time
// within FreshSignInWindow at s's now, as for a step of §11; one that is not is asked for more, a
// *StepUpError with max_age. A second factor is the admin role's to ask, under the admin mode,
// not the rule's: a password has none either. Any other personal access token is refused with
// rule, ErrScope.
func (s *Tokens) inPlaceOfPassword(cred Credential, rule string) error {
	if cred.SignIn == nil {
		return &authError{msg: rule, err: ErrScope}
	}
	err := CheckSignIn(cred, s.now(), Need{Fresh: true})
	var stepUp *StepUpError
	if errors.As(err, &stepUp) {
		return stepUp.withPrefix("a token in place of a password: ")
	}
	return err
}

// MayCreateDeploy returns nil when the credential of ctx may create a deploy token of a repo,
// which its rights on the repo then decide: every credential but a CI token (docs/spec/rbac.md
// §11), whatever its grants, an error wrapping ErrScope: a token of minutes must not mint a
// secret that lasts. A client certificate and a Kubernetes service-account token, which
// authenticate /v2/token only (§9), may not either.
func MayCreateDeploy(ctx context.Context) error {
	switch CredentialFrom(ctx).Kind {
	case CredCI, CredCertificate, CredKubernetes:
		return &authError{msg: "a CI token creates no token: a deploy token is created with a password or a personal access token", err: ErrScope}
	case CredPassword, CredPAT, CredDeploy, CredOIDC:
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
	case CredCertificate, CredKubernetes, CredCI: // create no token
	}
	return ""
}

// CreateDeploy creates a deploy token of the repo <format>/<name> and returns it with its
// secret, DeployPrefix and 43 characters, with one token.create audit event whose repo is the
// repo. name is as for CreatePAT, unique among the repo's live tokens. scopes are read, write
// and attach, one or more, required: delete and manage are never a deploy token's. A group's
// deploy token reads only: a write or an attachment through a group is its hosted member's, which
// the token does not reach (docs/spec/rbac.md §7). The expiry is optional, and bounded by
// DeployMaxLifetime when it is not zero (lifetime).
func (s *Tokens) CreateDeploy(ctx context.Context, repo, name string, scopes []string, exp Expiry, o Origin) (*Token, string, error) {
	scopes, err := checkScopes(scopes, TokenDeploy)
	if err != nil {
		return nil, "", err
	}
	formatName, repoName, err := splitRepo(repo)
	if err != nil {
		return nil, "", err
	}
	return s.create(ctx, TokenDeploy, "repo "+repo, name, scopes, exp, o, nil, func(w index.Writer) (*index.Token, tokenOwner, error) {
		r, err := w.GetRepo(ctx, formatName, repoName)
		if errors.Is(err, index.ErrNotFound) {
			return nil, tokenOwner{}, repoError(repo, err)
		}
		if err != nil {
			return nil, tokenOwner{}, err
		}
		if r.Kind == index.KindGroup && (slices.Contains(scopes, ScopeWrite) || slices.Contains(scopes, ScopeAttach)) {
			return nil, tokenOwner{}, invalidf("scopes: %s is a group, and its deploy token reads only: a write or an attachment "+
				"through it is its hosted member's, which the token would not reach", repo)
		}
		return &index.Token{RepoID: &r.ID}, tokenOwner{repo: repo}, nil
	})
}

// CreateMaskDeploy creates a deploy token over mask, <format>/<repo> with "*" for a whole
// segment as a binding's mask (docs/spec/rbac.md §1, §3), and returns it with its secret as
// CreateDeploy does, with one token.create audit event whose subject is the mask and which has
// no repo. The mask names no repo: it need not match one now, it matches the repos created later
// too, and no repo deleted takes the token with it. name is unique among the live tokens over
// the same mask; scopes and the expiry are those of CreateDeploy. Only the admin role may create
// one: the caller decides that (the API asks it before the body is read; the command line is
// the operator's). The credential of ctx must be one that may create it (MayCreateMaskDeploy):
// a password, a web UI's fresh sign-in through OIDC, or none from the command line.
func (s *Tokens) CreateMaskDeploy(ctx context.Context, mask, name string, scopes []string, exp Expiry, o Origin) (*Token, string, error) {
	if err := s.MayCreateMaskDeploy(ctx); err != nil {
		return nil, "", err
	}
	if err := checkMask(mask, s.Formats); err != nil {
		return nil, "", err
	}
	scopes, err := checkScopes(scopes, TokenDeploy)
	if err != nil {
		return nil, "", err
	}
	return s.create(ctx, TokenDeploy, "mask "+mask, name, scopes, exp, o, nil, func(index.Writer) (*index.Token, tokenOwner, error) {
		return &index.Token{RepoMask: mask}, tokenOwner{mask: mask}, nil
	})
}

// tokenOwner is the owner of a token: the name and the uid of the user of a personal access
// token, and whether it is a service account; the repo <format>/<name> of a deploy token of one
// repo; or the mask of one over a mask.
type tokenOwner struct {
	user, uid, repo, mask string
	service               bool
}

// create checks name and the expiry and creates a token of kind with scopes in one
// transaction with its token.create event, whose details name the credential of ctx
// (createdBy). owner names the owner for the messages, user <name>, repo <format>/<name> or
// mask <mask>; read reads it in the transaction and returns the row with its owner set, and
// the owner, whose kind the rule of the expiry depends on (lifetime). The name is unique among
// the live tokens of the same kind and owner: a token over a mask is listed with every token
// (ListTokens without a filter) and compared by its mask. after, when not nil, runs in the
// transaction once the row is inserted: the join of an OIDC login's refresh token (#1282).
func (s *Tokens) create(ctx context.Context, kind, owner, name string, scopes []string, exp Expiry, o Origin,
	after func(index.Writer, *index.Token) error, read func(index.Writer) (*index.Token, tokenOwner, error)) (*Token, string, error) {
	if err := checkTokenName(name); err != nil {
		return nil, "", err
	}
	now := s.now()
	prefix := PATPrefix
	if kind == TokenDeploy {
		prefix = DeployPrefix
	}
	secret := newSecret(prefix)
	var (
		row       *index.Token
		own       tokenOwner
		ev        index.AuditEvent
		expiresAt *time.Time
	)
	err := s.Index.Tx(ctx, func(w index.Writer) (err error) {
		if row, own, err = read(w); err != nil {
			return err
		}
		l := s.lifetime(kind, own)
		if expiresAt, err = expiry(l, exp, now); err != nil {
			return err
		}
		if err := nameTaken(ctx, w, kind, row, name, owner, now); err != nil {
			return err
		}
		row.Kind, row.Name, row.Hash, row.Scopes, row.ExpiresAt, row.CreatedAt = kind, name, HashSecret(secret), scopes, expiresAt, now
		if err = w.CreateToken(ctx, row); err != nil {
			return err
		}
		if after != nil {
			if err := after(w, row); err != nil {
				return err
			}
		}
		ev, err = s.record(ctx, w, ActionTokenCreate, own, o, createDetails(ctx, row, l))
		return err
	})
	if err != nil {
		return nil, "", tokenError("create a token of "+owner, err)
	}
	s.writeAudit(ctx, ev)
	t := s.resource(row, own, now)
	return &t, secret, nil
}

// nameTaken returns an error wrapping index.ErrConflict, naming owner, when a live token of kind
// and of the owner of row, its user, its repo or its mask, has name at now (Tokens.create): a
// token over a mask is listed with every token (ListTokens without a filter) and compared by its
// mask.
func nameTaken(ctx context.Context, w index.Writer, kind string, row *index.Token, name, owner string, now time.Time) error {
	live, err := w.ListTokens(ctx, row.UserID, row.RepoID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(live, func(t index.Token) bool {
		return t.Kind == kind && t.RepoMask == row.RepoMask && t.Name == name && alive(&t, now)
	}) {
		return &authError{msg: fmt.Sprintf("name: %q is taken by a live token of %s", name, owner), err: index.ErrConflict}
	}
	return nil
}

// createDetails returns the details of the token.create event of row, created by the credential
// of ctx under the rule l (docs/spec/rbac.md §5): createdBy, and maxTTL, the seconds of the
// bound of the configuration it was created under, when there is one.
func createDetails(ctx context.Context, row *index.Token, l lifetime) map[string]any {
	details := map[string]any{"id": row.ID, "kind": row.Kind, "name": row.Name, "scopes": row.Scopes, "expiresAt": row.ExpiresAt}
	if by := createdBy(CredentialFrom(ctx)); by != "" {
		details["createdBy"] = by
	}
	if l.key != "" && l.max > 0 {
		details["maxTTL"] = int64(l.max / time.Second)
	}
	if row.DPoPJKT != "" {
		details["dpopJkt"] = row.DPoPJKT
	}
	if row.AuthTime != nil {
		details["authTime"] = row.AuthTime.Format(time.RFC3339)
	}
	if len(row.AMR) > 0 {
		details["amr"] = row.AMR
	}
	if row.ACR != "" {
		details["acr"] = row.ACR
	}
	return details
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
	return s.resources(rows, TokenPAT, tokenOwner{user: u.Name, uid: u.UID, service: u.Kind == KindService}), nil
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
	return s.resources(rows, TokenDeploy, tokenOwner{repo: repo}), nil
}

// ListMaskDeploy returns the deploy tokens over mask, or over any mask when mask is "", in the
// order of their creation, the revoked and expired ones included. A mask is compared as it was
// written, not matched: oci/* lists the tokens over oci/*, not those over */*.
func (s *Tokens) ListMaskDeploy(ctx context.Context, mask string) ([]Token, error) {
	if mask != "" {
		if err := checkMask(mask, nil); err != nil {
			return nil, err
		}
	}
	rows, err := s.Index.ListTokens(ctx, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: list the deploy tokens over a mask: %w", err)
	}
	out, now := []Token{}, s.now()
	for i := range rows {
		if t := &rows[i]; t.Kind == TokenDeploy && t.RepoMask != "" && (mask == "" || t.RepoMask == mask) {
			out = append(out, s.resource(t, tokenOwner{mask: t.RepoMask}, now))
		}
	}
	return out, nil
}

// Get returns the token with id, of any kind and owner.
func (s *Tokens) Get(ctx context.Context, id int64) (*Token, error) {
	row, own, err := find(ctx, s.Index, id)
	if err != nil {
		return nil, tokenError(fmt.Sprintf("get token %d", id), err)
	}
	t := s.resource(row, own, s.now())
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
		if err := w.RevokeToken(ctx, id, now); errors.Is(err, index.ErrConflict) {
			return nil // revoked meanwhile, with its row
		} else if err != nil {
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

// RevokeIdle revokes the token of a web UI's sign-in that the authenticator refused as idle, e
// (docs/spec/rbac.md §10, #1277), with one token.revoke audit event by anonymous: the server
// revokes it, not a person. Its details add reason idle, lastUsedAt and idleTimeout (seconds) to
// those of Revoke, and o's source is idle. A token revoked already, by a request on another
// replica too, stays as it is, without an event.
func (s *Tokens) RevokeIdle(ctx context.Context, e *IdleError, o Origin) error {
	ctx = WithPrincipal(ctx, Anonymous)
	o.Source = SourceIdle
	var ev *index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		row, own, err := find(ctx, w, e.TokenID)
		if err != nil || row.RevokedAt != nil {
			return err
		}
		if err := w.RevokeToken(ctx, row.ID, s.now()); errors.Is(err, index.ErrConflict) {
			return nil // revoked meanwhile, with its row
		} else if err != nil {
			return err
		}
		e2, err := s.record(ctx, w, ActionTokenRevoke, own, o, map[string]any{
			"id": row.ID, "kind": row.Kind, "name": row.Name, "reason": SourceIdle,
			"lastUsedAt": e.LastUse.UTC().Format(time.RFC3339), "idleTimeout": int64(e.Timeout / time.Second),
		})
		ev = &e2
		return err
	})
	if err != nil {
		return tokenError(fmt.Sprintf("revoke idle token %d", e.TokenID), err)
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
	own, err := tokenOwnerOf(ctx, r, &rows[i])
	if err != nil {
		return nil, tokenOwner{}, err
	}
	return &rows[i], own, nil
}

// tokenOwnerOf returns the owner of the token row: its mask, its user, by the name it had when it
// was deleted for a deleted user, and whether it is a service account, or its repo.
func tokenOwnerOf(ctx context.Context, r index.Reader, row *index.Token) (tokenOwner, error) {
	switch {
	case row.RepoMask != "":
		return tokenOwner{mask: row.RepoMask}, nil
	case row.UserID != nil:
		u, err := r.GetUserByID(ctx, *row.UserID)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return tokenOwner{}, err
		}
		if err == nil {
			name := u.Name
			if u.DeletedAt != nil {
				name = u.DeletedName
			}
			return tokenOwner{user: name, uid: u.UID, service: u.Kind == KindService}, nil
		}
	case row.RepoID != nil:
		repo, err := r.GetRepoByID(ctx, *row.RepoID)
		if err != nil && !errors.Is(err, index.ErrNotFound) {
			return tokenOwner{}, err
		}
		if err == nil {
			return tokenOwner{repo: repo.Format + "/" + repo.Name}, nil
		}
	}
	return tokenOwner{}, nil // an owner gone takes its tokens with it; no row is left like this
}

// TokenQuery selects the tokens of Tokens.Page, every token of every owner when only Limit is
// set: each other field narrows.
type TokenQuery struct {
	Kind     string // TokenPAT or TokenDeploy
	User     string // a live user's name or uid: its personal access tokens
	Repo     string // <format>/<name>: the deploy tokens of that repo, not those over a mask
	RepoMask string // the deploy tokens over that mask, as it was written
	Live     bool   // those neither revoked nor expired now
	After    int64  // the id of the last token of the page before
	Limit    int
}

// Page returns the tokens q keeps whose id is above q.After, by id, which is the order of their
// creation, at most q.Limit, the revoked and expired ones included unless q.Live: personal
// access tokens of every user, a deleted user's under the name it had, and deploy tokens of
// every repo and over every mask (docs/api.md, Every token). A kind that is neither, a repo
// that is not <format>/<name> and a mask that breaks the grammar of rbac.md §3 are errors
// wrapping ErrInvalid; a user or a repo that does not exist has no token.
func (s *Tokens) Page(ctx context.Context, q TokenQuery) ([]Token, error) {
	f, err := s.filter(ctx, q)
	if errors.Is(err, index.ErrNotFound) {
		return []Token{}, nil
	} else if err != nil {
		return nil, err
	}
	rows, err := s.Index.ListTokenPage(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("auth: list the tokens: %w", err)
	}
	out, now := make([]Token, 0, len(rows)), s.now()
	owners := map[string]tokenOwner{} // a page reads each owner once
	for i := range rows {
		row := &rows[i]
		key := "mask " + row.RepoMask
		if row.UserID != nil {
			key = fmt.Sprint("user ", *row.UserID)
		} else if row.RepoID != nil {
			key = fmt.Sprint("repo ", *row.RepoID)
		}
		own, ok := owners[key]
		if !ok {
			if own, err = tokenOwnerOf(ctx, s.Index, row); err != nil {
				return nil, fmt.Errorf("auth: list the tokens: %w", err)
			}
			owners[key] = own
		}
		out = append(out, s.resource(row, own, now))
	}
	return out, nil
}

// filter returns the index.TokenFilter of q, an error wrapping ErrInvalid for a field that
// breaks its rule, or index.ErrNotFound when q names a user or a repo that does not exist.
func (s *Tokens) filter(ctx context.Context, q TokenQuery) (index.TokenFilter, error) {
	f := index.TokenFilter{Kind: q.Kind, RepoMask: q.RepoMask, After: q.After, Limit: q.Limit}
	if q.Kind != "" && q.Kind != TokenPAT && q.Kind != TokenDeploy {
		return f, invalidf("kind: %q is neither %s nor %s", q.Kind, TokenPAT, TokenDeploy)
	}
	if q.RepoMask != "" {
		if err := checkMask(q.RepoMask, nil); err != nil {
			return f, err
		}
	}
	if q.Repo != "" {
		formatName, name, err := splitRepo(q.Repo)
		if err != nil {
			return f, err
		}
		r, err := s.Index.GetRepo(ctx, formatName, name)
		if err != nil {
			return f, fmt.Errorf("auth: list the tokens of repo %s: %w", q.Repo, err)
		}
		f.RepoID = &r.ID
	}
	if q.User != "" {
		u, err := liveUser(ctx, s.Index, q.User)
		if err != nil {
			return f, fmt.Errorf("auth: list the tokens of user %s: %w", q.User, err)
		}
		f.UserID = &u.ID
	}
	if q.Live {
		f.LiveAt = s.now()
	}
	return f, nil
}

// liveUser returns the live user that s names, a name or a uid; a uid of a deleted user is
// index.ErrNotFound, as a name that no live user has.
func liveUser(ctx context.Context, r index.Reader, s string) (*index.User, error) {
	if !index.IsUID(s) {
		return r.GetUser(ctx, s)
	}
	u, err := r.GetUserByUID(ctx, s)
	if err == nil && u.DeletedAt != nil {
		return nil, fmt.Errorf("%w: user %s was deleted", index.ErrNotFound, s)
	}
	return u, err
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

// checkScopes checks the scopes of a token of kind and returns them without repeats, in the
// order read, write, attach. A personal access token may have none; a deploy token needs one.
func checkScopes(scopes []string, kind string) ([]string, error) {
	var read, write, attach bool
	for _, sc := range scopes {
		switch sc {
		case ScopeRead:
			read = true
		case ScopeWrite:
			write = true
		case ScopeAttach:
			attach = true
		default:
			if kind == TokenDeploy {
				return nil, invalidf("scopes: %q is not a scope of a deploy token: it gets read, write and attach, one or more, never delete or manage", sc)
			}
			return nil, invalidf("scopes: %q is not a scope: a personal access token narrows its user to read, write and attach, one or more", sc)
		}
	}
	out := []string{}
	for _, sc := range []struct {
		on    bool
		scope string
	}{{read, ScopeRead}, {write, ScopeWrite}, {attach, ScopeAttach}} {
		if sc.on {
			out = append(out, sc.scope)
		}
	}
	if kind == TokenDeploy && len(out) == 0 {
		return nil, invalidf("scopes: a deploy token needs read, write or attach")
	}
	return out, nil
}

// lifetime is the rule of the expiry of a new token: def is the lifetime of a token created
// without an expiry, and max bounds its lifetime, each none when zero; bound says max in the
// message of a refusal; key is the key of the configuration that sets max, "" for a constant.
type lifetime struct {
	def, max   time.Duration
	bound, key string
}

// lifetime returns the rule of the expiry of a token of kind whose owner is own: for a person's
// personal access token DefaultPATLifetime and MaxPATLifetime, for a service account's
// ServiceMaxLifetime as both, for a deploy token DeployMaxLifetime as both (docs/spec/rbac.md §1).
func (s *Tokens) lifetime(kind string, own tokenOwner) lifetime {
	switch {
	case kind == TokenDeploy:
		return configured(s.DeployMaxLifetime, "a deploy token", "auth.deployTokens.maxTTL")
	case own.service:
		return configured(s.ServiceMaxLifetime, "a service account's token", "auth.serviceAccounts.maxTokenTTL")
	}
	return lifetime{def: DefaultPATLifetime, max: MaxPATLifetime,
		bound: fmt.Sprintf("a personal access token expires at most %d days ahead", MaxPATLifetime/(24*time.Hour))}
}

// configured is the rule of d, the bound of the configuration key, for the tokens what names: a
// token created without an expiry gets d, and none when d is zero.
func configured(d time.Duration, what, key string) lifetime {
	return lifetime{def: d, max: d, key: key, bound: fmt.Sprintf("%s expires at most %s ahead (%s)", what, d, key)}
}

// expiry returns the expiry of a new token at now by the rule l: exp.At, or now plus exp.In, or
// l.def from now, or none when l.def is zero. It is in the future, and at most l.max ahead when
// l.max is not zero. A refusal names exp.Field, or else the field given.
func expiry(l lifetime, exp Expiry, now time.Time) (*time.Time, error) {
	field := "expiresAt"
	if exp.In != 0 {
		field = "expiresIn"
	}
	if exp.Field != "" {
		field = exp.Field
	}
	var at time.Time
	switch {
	case !exp.At.IsZero() && exp.In != 0:
		return nil, invalidf("expiresAt and expiresIn: give one of them, not both")
	case exp.In < 0:
		return nil, invalidf("%s: must be positive", field)
	case exp.In > 0:
		at = now.Add(exp.In)
	case !exp.At.IsZero():
		at = exp.At.UTC()
	case l.def > 0:
		at = now.Add(l.def)
	default:
		return nil, nil // a deploy token or a service account's token without a bound: no expiry
	}
	switch {
	case !at.After(now):
		return nil, invalidf("%s: must be in the future", field)
	case l.max > 0 && at.After(now.Add(l.max)):
		return nil, invalidf("%s: %s, at %s at the latest", field, l.bound, now.Add(l.max).Format(time.RFC3339))
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
// token, <format>/<name> for a deploy token of one repo, the mask for one over a mask
// (docs/spec/rbac.md §5).
func subjectOf(own tokenOwner) string {
	switch {
	case own.uid != "":
		return Principal{Kind: KindUser, Name: own.uid}.String()
	case own.mask != "":
		return own.mask
	}
	return own.repo
}

// resources returns the Tokens of rows of kind, whose owner is own.
func (s *Tokens) resources(rows []index.Token, kind string, own tokenOwner) []Token {
	out, now := make([]Token, 0, len(rows)), s.now()
	for i := range rows {
		if rows[i].Kind == kind {
			out = append(out, s.resource(&rows[i], own, now))
		}
	}
	return out
}

// resource returns the Token of the tokens row t whose owner is own, with UTC times, marked
// BeyondMaxTTL when it is a live token beyond the bound of its kind at now.
func (s *Tokens) resource(t *index.Token, own tokenOwner, now time.Time) Token {
	r := tokenResource(t, own)
	r.BeyondMaxTTL = s.beyond(t, own, now)
	return r
}

// ViewAt returns the Token of the tokens row t at now as every view of a token answers it
// (resource), without a read: its owner the user u of a personal access token, repo the
// <format>/<name> of a deploy token of one repo, and the mask of one over a mask its own (the
// access report, docs/spec/rbac.md §14).
func (s *Tokens) ViewAt(t *index.Token, u *index.User, repo string, now time.Time) Token {
	own := tokenOwner{repo: repo, mask: t.RepoMask}
	if u != nil {
		own = tokenOwner{user: u.Name, uid: u.UID, service: u.Kind == KindService}
	}
	return s.resource(t, own, now)
}

// storedPrecision is the precision of the times of the index, a microsecond on PostgreSQL: the
// expiry a bound gives a token is at most that much after createdAt plus the bound once both
// are stored.
const storedPrecision = time.Microsecond

// beyond reports whether t, whose owner is own, is a live token at now beyond the bound of the
// configuration of its kind (docs/spec/rbac.md §1, #1286): a deploy token beyond
// DeployMaxLifetime or a service account's token beyond ServiceMaxLifetime, without an expiry or
// expiring more than the bound after its creation. A token created under the bound never is;
// one created before the bound was set or lowered may be. A person's token never is: its bound
// is a constant.
func (s *Tokens) beyond(t *index.Token, own tokenOwner, now time.Time) bool {
	l := s.lifetime(t.Kind, own)
	if l.key == "" || l.max <= 0 || !alive(t, now) {
		return false
	}
	return t.ExpiresAt == nil || t.ExpiresAt.Sub(t.CreatedAt) > l.max+storedPrecision
}

// countPage is the page of CountBeyondMaxTTL.
const countPage = 500

// CountBeyondMaxTTL counts the live tokens beyond the bound of their kind now (beyond): the deploy
// tokens beyond DeployMaxLifetime and the tokens of live service accounts beyond
// ServiceMaxLifetime, which GET /api/v1/tokens?live=true marks beyondMaxTTL. serve logs them at
// start (docs/spec/rbac.md §1). A bound that is zero counts none.
func (s *Tokens) CountBeyondMaxTTL(ctx context.Context) (deploy, service int, err error) {
	now := s.now()
	if s.DeployMaxLifetime > 0 {
		if deploy, err = s.countDeployBeyond(ctx, now); err != nil {
			return 0, 0, fmt.Errorf("auth: count the deploy tokens beyond auth.deployTokens.maxTTL: %w", err)
		}
	}
	if s.ServiceMaxLifetime > 0 {
		if service, err = s.countServiceBeyond(ctx, now); err != nil {
			return 0, 0, fmt.Errorf("auth: count the service accounts' tokens beyond auth.serviceAccounts.maxTokenTTL: %w", err)
		}
	}
	return deploy, service, nil
}

// countDeployBeyond counts the live deploy tokens beyond DeployMaxLifetime at now, a page at a
// time.
func (s *Tokens) countDeployBeyond(ctx context.Context, now time.Time) (int, error) {
	n, f := 0, index.TokenFilter{Kind: TokenDeploy, LiveAt: now, Limit: countPage}
	for {
		rows, err := s.Index.ListTokenPage(ctx, f)
		if err != nil {
			return 0, err
		}
		for i := range rows {
			if s.beyond(&rows[i], tokenOwner{}, now) {
				n++
			}
		}
		if len(rows) < f.Limit {
			return n, nil
		}
		f.After = rows[len(rows)-1].ID
	}
}

// countServiceBeyond counts the live tokens of the live service accounts beyond
// ServiceMaxLifetime at now; a deleted account's tokens were revoked with it.
func (s *Tokens) countServiceBeyond(ctx context.Context, now time.Time) (int, error) {
	users, err := s.Index.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range users {
		if users[i].Kind != KindService {
			continue
		}
		rows, err := s.Index.ListTokens(ctx, &users[i].ID, nil)
		if err != nil {
			return 0, err
		}
		for j := range rows {
			if rows[j].Kind == TokenPAT && s.beyond(&rows[j], tokenOwner{service: true}, now) {
				n++
			}
		}
	}
	return n, nil
}

// tokenResource returns the Token of a tokens row whose owner is own, with UTC times.
func tokenResource(t *index.Token, own tokenOwner) Token {
	scopes := slices.Clone(t.Scopes)
	if scopes == nil {
		scopes = []string{}
	}
	return Token{
		ID: t.ID, Kind: t.Kind, User: own.user, Repo: own.repo, RepoMask: own.mask, Name: t.Name, Scopes: scopes,
		ExpiresAt: utc(t.ExpiresAt), LastUsedAt: utc(t.LastUsedAt), CreatedAt: t.CreatedAt.UTC(), RevokedAt: utc(t.RevokedAt),
		HashPrefix: t.Hash[:min(HashPrefixLen, len(t.Hash))], DPoPJKT: t.DPoPJKT, SignIn: tokenSignIn(t),
	}
}

// keptSignIn reports whether t kept a sign-in at the IdP: any of auth_time, amr and acr
// (docs/spec/rbac.md §10).
func keptSignIn(t *index.Token) bool { return t.AuthTime != nil || len(t.AMR) > 0 || t.ACR != "" }

// tokenSignIn is the sign-in that t kept, as its views answer it; nil when it kept none.
func tokenSignIn(t *index.Token) *TokenSignIn {
	if !keptSignIn(t) {
		return nil
	}
	return &TokenSignIn{AuthTime: utc(t.AuthTime), AMR: slices.Clone(t.AMR), ACR: t.ACR}
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
