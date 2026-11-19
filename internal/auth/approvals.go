package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/audit"
	"github.com/lajstrom/lajstrom/internal/index"
)

// The approval of privileged changes (docs/spec/rbac.md §13, ADR 0030, decided on #1285): with
// auth.approval second-admin, an admin binding, a binding on */* and a deploy token over a mask
// whose scopes hold write, made through the API, wait for a second admin. The route checks what
// it checks for the change, and makes a request (index.Approval) in its place, which grants
// nothing: nothing that reads bindings or tokens reads it. An admin other than the requester
// approves it, and the approval makes the change, after checking again what the request checked;
// any admin rejects it, the requester so withdrawing it; one still pending ApprovalLifetime after
// it was made expires. The command line makes these changes at once whatever the setting.

// ActionApprovalRequest is the audit action of a request for a second admin (docs/spec/rbac.md
// §5, §13); its decisions' are with them (approve.go).
const ActionApprovalRequest = "approval.request"

// ApprovalLifetime is how long a request waits for a decision: one still pending this long after
// it was made expires (ADR 0030, a constant).
const ApprovalLifetime = 7 * 24 * time.Hour

// Approval is a request for a second admin as the approvals API and lajstrom admin approval list
// answer it (docs/spec/http-routing.md §4): its change as it is answered, never a deploy token's
// hash, and the decision once it is decided.
type Approval struct {
	ID int64 `json:"id"`
	// Kind is binding or deploy-token (index.ApprovalBinding, index.ApprovalDeployToken).
	Kind string `json:"kind"`
	// Change is the binding, {principal, role, repoMask}, or the deploy token, {name, repoMask,
	// scopes, expiresAt}, that the approval makes.
	Change       json.RawMessage `json:"change"`
	ChangeReason string          `json:"changeReason,omitempty"`
	// State is pending, approved, rejected or expired.
	State       string    `json:"state"`
	RequestedBy string    `json:"requestedBy"`
	RequestedAt time.Time `json:"requestedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	// DecidedBy, DecidedAt and DecisionReason are the decision, anonymous for an expiry; Result is
	// what an approval made, binding:<id> or token:<id>. Absent while pending.
	DecidedBy      string     `json:"decidedBy,omitempty"`
	DecidedAt      *time.Time `json:"decidedAt,omitempty"`
	DecisionReason string     `json:"decisionReason,omitempty"`
	Result         string     `json:"result,omitempty"`
}

// bindingChange is the change of a request of a binding, as it is kept and answered: the
// principal as it is stored, user:<uid> for a user.
type bindingChange struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	RepoMask  string `json:"repoMask"`
}

// tokenChange is the change of a request of a deploy token over a mask as it is answered.
type tokenChange struct {
	Name      string     `json:"name"`
	RepoMask  string     `json:"repoMask"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// keptToken is the change of a request of a deploy token as it is kept (docs/spec/index-schema.md,
// approvals): with the sha256 of its secret, which the approval makes the token with, the
// credential of the request as the token's createdBy, and the request's sign-in (AddSignIn).
type keptToken struct {
	tokenChange
	Hash      string         `json:"hash"`
	CreatedBy string         `json:"createdBy,omitempty"`
	SignIn    map[string]any `json:"signIn,omitempty"`
}

// BindingWaits reports whether a binding of role on mask waits for a second admin under
// auth.approval second-admin (docs/spec/rbac.md §13): the role admin, whatever its principal, and
// the mask */*, whatever its role.
func BindingWaits(role, mask string) bool { return role == RoleAdmin || mask == MaskAll }

// MaskDeployWaits reports whether a deploy token over a mask with scopes waits for a second admin
// under auth.approval second-admin (docs/spec/rbac.md §13): scopes that hold write.
func MaskDeployWaits(scopes []string) bool { return slices.Contains(scopes, ScopeWrite) }

// Approvals manages the requests for a second admin (docs/spec/rbac.md §13): it makes them in
// place of the changes that wait, and approves, rejects and expires them. Every request and
// decision is an audit row by PrincipalFrom(ctx), with the subject approval:<id>; an approval
// writes the change's own row beside it. Create it as a struct literal; its methods may be called
// concurrently.
//
// Errors for the caller wrap ErrInvalid (a field that breaks its rule), ErrScope (a credential
// or a principal that may not), index.ErrNotFound (no such request) or index.ErrConflict (the
// change exists, the same change waits, a request decided or expired, an approval whose change no
// longer holds), with a message for the person; any other error is a failure of the index.
type Approvals struct {
	// Index is the index database; required.
	Index index.Store
	// Bindings checks and makes the bindings, with its Formats; required.
	Bindings *Bindings
	// Tokens checks and makes the deploy tokens, with its Formats and DeployMaxLifetime; required.
	Tokens *Tokens
	// Audit records the events; nil means each event goes to the index in the transaction of its
	// change.
	Audit audit.Writer
	// Now is the clock of the requests, their expiry and the decisions; nil means time.Now.
	Now func() time.Time
	// Log is the logger; nil means slog.Default().
	Log *slog.Logger
}

// MayRequest returns nil when the credential of ctx may make a request for a second admin
// (docs/spec/rbac.md §13, Who requests; ADR 0035, #1393): what its approval takes (MayApprove),
// the admin's password, or a web UI's fresh sign-in through OIDC in its place, never a token,
// whoever made it, nor none: an error wrapping ErrScope naming the rule, or a *StepUpError for a
// sign-in that is not fresh. A service account, which has neither a password nor a sign-in, so
// requests nothing, and a token that one admin made for another gives that admin no second
// voice. The API asks it once the body shows that a binding waits; a deploy token over a mask
// takes the same credential already (Tokens.MayCreateMaskDeploy).
func (s *Approvals) MayRequest(ctx context.Context) error {
	return s.personal(ctx, "a request for a second admin is made with the admin's password or a web UI's fresh sign-in through OIDC, "+
		"not with a token")
}

// RequestBinding checks the binding (principal, role, mask) as Bindings.Create does, that it does
// not exist, and makes the request of it in its place, pending, with its approval.request row,
// by the user of ctx. principal is kept as it would be bound, user:<uid> for a user. The same
// binding pending already is index.ErrConflict. The caller checks the credential first
// (MayRequest).
func (s *Approvals) RequestBinding(ctx context.Context, principal, role, mask string, o Origin) (*Approval, error) {
	p, err := s.Bindings.check(principal, role, mask)
	if err != nil {
		return nil, err
	}
	by, err := requester(ctx)
	if err != nil {
		return nil, err
	}
	return s.request(ctx, index.ApprovalBinding, o, by, func(w index.Writer) (string, any, any, error) {
		bound, _, err := boundPrincipal(ctx, w, p)
		if err != nil {
			return "", nil, nil, err
		}
		have, err := w.ListBindings(ctx, bound)
		if err != nil {
			return "", nil, nil, err
		}
		if slices.ContainsFunc(have, func(b index.Binding) bool { return b.Role == role && b.RepoMask == mask }) {
			return "", nil, nil, &authError{msg: fmt.Sprintf("binding %s %s on %s exists already", principal, role, mask),
				err: index.ErrConflict}
		}
		c := bindingChange{Principal: bound, Role: role, RepoMask: mask}
		return bound + " " + role + " " + mask, c, c, nil
	})
}

// RequestMaskDeploy checks the deploy token over mask as Tokens.CreateMaskDeploy does, its
// credential, mask, scopes, name and expiry, and that no live token over the mask has its name,
// and makes the request of it in its place, pending, with its approval.request row, by the user of
// ctx. It returns the request and the token's secret, the one time it is shown: the request keeps
// its sha256, and its approval makes the token with it, which until then is no credential. The
// expiry is the one asked now, kept for the token. The same name over the mask pending already is
// index.ErrConflict.
func (s *Approvals) RequestMaskDeploy(ctx context.Context, mask, name string, scopes []string, exp Expiry, o Origin) (*Approval, string,
	error) {
	if err := s.Tokens.MayCreateMaskDeploy(ctx); err != nil {
		return nil, "", err
	}
	if err := checkMask(mask, s.Tokens.Formats); err != nil {
		return nil, "", err
	}
	scopes, err := checkScopes(scopes, TokenDeploy)
	if err != nil {
		return nil, "", err
	}
	if err := checkTokenName(name); err != nil {
		return nil, "", err
	}
	by, err := requester(ctx)
	if err != nil {
		return nil, "", err
	}
	now := s.now()
	expiresAt, err := expiry(s.Tokens.lifetime(TokenDeploy, tokenOwner{mask: mask}), exp, now)
	if err != nil {
		return nil, "", err
	}
	secret := newSecret(DeployPrefix)
	c := tokenChange{Name: name, RepoMask: mask, Scopes: scopes, ExpiresAt: expiresAt}
	kept := keptToken{tokenChange: c, Hash: HashSecret(secret), CreatedBy: createdBy(CredentialFrom(ctx))}
	if in, ok := AddSignIn(ctx, map[string]any{})["signIn"].(map[string]any); ok {
		kept.SignIn = in
	}
	a, err := s.request(ctx, index.ApprovalDeployToken, o, by, func(w index.Writer) (string, any, any, error) {
		if err := nameTaken(ctx, w, TokenDeploy, &index.Token{RepoMask: mask}, name, "mask "+mask, now); err != nil {
			return "", nil, nil, err
		}
		return mask + " " + name, kept, c, nil
	})
	if err != nil {
		return nil, "", err
	}
	return a, secret, nil
}

// request makes, in one transaction, the request of kind by the user by with the origin o: read
// checks the change on w and returns its subject, what the request keeps of it and what its
// approval.request row says of it, which has the request's credential too. The same subject
// pending is index.ErrConflict.
func (s *Approvals) request(ctx context.Context, kind string, o Origin, by string,
	read func(index.Writer) (subject string, kept, shown any, err error)) (*Approval, error) {
	now := s.now()
	var (
		row index.Approval
		ev  index.AuditEvent
	)
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		subject, kept, shown, err := read(w)
		if err != nil {
			return err
		}
		change, err := json.Marshal(kept)
		if err != nil {
			return fmt.Errorf("the change of the request: %w", err)
		}
		row = index.Approval{Kind: kind, Subject: subject, Change: change, ChangeReason: o.ChangeReason, RequestedBy: by,
			RequestedAt: now, ExpiresAt: now.Add(ApprovalLifetime)}
		if err := w.CreateApproval(ctx, &row); err != nil {
			if errors.Is(err, index.ErrConflict) {
				return &authError{msg: fmt.Sprintf("the same change waits for a second admin already: %s (the requests of state "+
					"pending)", subject), err: err}
			}
			return err
		}
		ev, err = s.record(ctx, w, ActionApprovalRequest, &row, o, addCredential(ctx, map[string]any{"kind": kind, "change": shown,
			"expiresAt": row.ExpiresAt}))
		return err
	})
	if err != nil {
		return nil, approvalError("request", kind, err)
	}
	s.writeAudit(ctx, ev)
	a, err := approvalOf(&row)
	return &a, err
}

// Get returns the request id; index.ErrNotFound when there is none.
func (s *Approvals) Get(ctx context.Context, id int64) (*Approval, error) {
	row, err := s.Index.GetApproval(ctx, id)
	if err != nil {
		return nil, approvalError("read", strconv.FormatInt(id, 10), err)
	}
	a, err := approvalOf(row)
	return &a, err
}

// List returns the requests of state, pending, approved, rejected or expired, or of every state
// when state is "" (another is ErrInvalid), by id, newest first: those below the id before (0:
// from the newest), at most limit.
func (s *Approvals) List(ctx context.Context, state string, before int64, limit int) ([]Approval, error) {
	switch state {
	case "", index.ApprovalPending, index.ApprovalApproved, index.ApprovalRejected, index.ApprovalExpired:
	default:
		return nil, invalidf("state: %q is not pending, approved, rejected or expired", state)
	}
	rows, err := s.Index.ListApprovals(ctx, state, before, limit)
	if err != nil {
		return nil, fmt.Errorf("auth: list the requests for a second admin: %w", err)
	}
	out := make([]Approval, len(rows))
	for i := range rows {
		if out[i], err = approvalOf(&rows[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// requester returns the principal of ctx as the requester or the decider of a request: a user,
// user:<uid>; any other principal, which the admin role never is, is ErrScope.
func requester(ctx context.Context) (string, error) {
	p := PrincipalFrom(ctx)
	if p.Kind != KindUser {
		return "", &authError{msg: "a request for a second admin is a user's, an admin's: " + p.String() + " is not one", err: ErrScope}
	}
	return p.String(), nil
}

// addCredential sets credential in the details d of a request or an approval (docs/spec/rbac.md
// §5, #1393): the credential of ctx as a token's createdBy names it, password or the sign-in's
// token:<id>, the proof that the side was a person's. It returns d, unchanged for none.
func addCredential(ctx context.Context, d map[string]any) map[string]any {
	if c := createdBy(CredentialFrom(ctx)); c != "" {
		d["credential"] = c
	}
	return d
}

// record builds the audit event of action about the request a, its subject approval:<id>, and
// appends it on w when there is no audit.Writer (recordAudit).
func (s *Approvals) record(ctx context.Context, w index.Writer, action string, a *index.Approval, o Origin,
	details map[string]any) (index.AuditEvent, error) {
	ev := index.AuditEvent{At: s.now(), Action: action, Subject: "approval:" + strconv.FormatInt(a.ID, 10)}
	return recordAudit(ctx, w, s.Audit, ev, o, details)
}

// writeAudit passes ev to the audit.Writer, after the commit of its change (writeAuditEvent).
func (s *Approvals) writeAudit(ctx context.Context, ev index.AuditEvent) {
	writeAuditEvent(ctx, s.Audit, s.log(), ev, "auth: audit event of a request for a second admin not written", "subject")
}

// approvalOf returns the Approval of a row of approvals, with UTC times.
func approvalOf(a *index.Approval) (Approval, error) {
	shown, err := shownChange(a)
	if err != nil {
		return Approval{}, err
	}
	out := Approval{ID: a.ID, Kind: a.Kind, Change: shown, ChangeReason: a.ChangeReason, State: a.State, RequestedBy: a.RequestedBy,
		RequestedAt: a.RequestedAt.UTC(), ExpiresAt: a.ExpiresAt.UTC(), DecidedBy: a.DecidedBy, DecisionReason: a.DecisionReason,
		Result: a.Result}
	if a.DecidedAt != nil {
		at := a.DecidedAt.UTC()
		out.DecidedAt = &at
	}
	return out, nil
}

// shownChange returns the change of a as it is answered: a deploy token's without its hash, its
// createdBy and its sign-in.
func shownChange(a *index.Approval) (json.RawMessage, error) {
	if a.Kind != index.ApprovalDeployToken {
		return a.Change, nil
	}
	var k keptToken
	if err := json.Unmarshal(a.Change, &k); err != nil {
		return nil, fmt.Errorf("auth: the change of request %d: %w", a.ID, err)
	}
	return json.Marshal(k.tokenChange)
}

// approvalError turns an error about the request what into the one the callers answer: an
// *authError as it is, index.ErrNotFound with a message for the person, anything else wrapped
// with op, for the log.
func approvalError(op, what string, err error) error {
	var ae *authError
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, index.ErrNotFound):
		return &authError{msg: fmt.Sprintf("request %s does not exist", what), err: err}
	}
	return fmt.Errorf("auth: %s the request %s: %w", op, what, err)
}

func (s *Approvals) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func (s *Approvals) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}
