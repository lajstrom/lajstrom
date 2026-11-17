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

	"github.com/lajstrom/lajstrom/internal/index"
)

// The decisions of a request for a second admin (docs/spec/rbac.md §13, ADR 0030): an admin other
// than the requester approves it with the admin's password or a fresh sign-in, and the approval
// makes the change after checking again what the request checked and the requester; any admin
// rejects it, the requester so withdrawing it; each replica marks expired, once a minute, the
// requests still pending ApprovalLifetime after they were made. Each decision changes the state
// once, from pending, so that of decisions made at once one is.

// Audit actions of the decisions of a request for a second admin (docs/spec/rbac.md §5, §13).
const (
	ActionApprovalApprove = "approval.approve"
	ActionApprovalReject  = "approval.reject"
	ActionApprovalExpire  = "approval.expire"
)

// ApprovalExpiryInterval is how often each replica looks for the requests that expired
// (Approvals.RunExpiry).
const ApprovalExpiryInterval = time.Minute

// MayApprove returns nil when the credential of ctx may approve a request (docs/spec/rbac.md §13):
// what creating a deploy token over a mask takes, the admin's password, or a web UI's fresh
// sign-in through OIDC in its place, never another token (Tokens.MayCreateMaskDeploy's rule), nor
// none: an error wrapping ErrScope, or a *StepUpError for a sign-in that is not fresh. The API
// asks it before it looks the request up.
func (s *Approvals) MayApprove(ctx context.Context) error {
	const rule = "a request is approved with the admin's password or a web UI's fresh sign-in through OIDC, not with a token"
	if CredentialFrom(ctx).Kind == "" {
		return &authError{msg: rule, err: ErrScope} // none: the command line approves nothing
	}
	return s.Tokens.passwordOrSignIn(ctx, rule)
}

// Approve approves the request id by the user of ctx, who is not its requester (ErrScope), with
// the origin o, whose reason is the decision's. In one transaction, while the request is pending
// and has not expired (else index.ErrConflict): it checks again what the request checked, and
// that its requester is a live, enabled user with the admin role by its bindings (else
// index.ErrConflict naming what fails, and the request stays pending), makes the change with its
// binding.create or token.create row, which has approval and the request's changeReason, marks the
// request approved while it is pending, and writes its approval.approve row. The caller checks
// the role and the credential first (MayApprove).
func (s *Approvals) Approve(ctx context.Context, id int64, o Origin) (*Approval, error) {
	by, err := requester(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var (
		row index.Approval
		evs []index.AuditEvent
	)
	err = s.Index.Tx(ctx, func(w index.Writer) error {
		cur, err := s.pending(ctx, w, id, now)
		if err != nil {
			return err
		}
		if cur.RequestedBy == by {
			return &authError{msg: fmt.Sprintf("request %d: the admin who made it does not approve it: another admin does", id),
				err: ErrScope}
		}
		if err := s.requesterHolds(ctx, w, cur, now); err != nil {
			return err
		}
		result, ev, err := s.apply(ctx, w, cur, o, now)
		if err != nil {
			return err
		}
		row = index.Approval{ID: id, State: index.ApprovalApproved, DecidedBy: by, DecidedAt: &now, DecisionReason: o.ChangeReason,
			Result: result}
		if err := s.decide(ctx, w, &row); err != nil {
			return err
		}
		done, err := s.record(ctx, w, ActionApprovalApprove, &row, o, decisionDetails(&row))
		evs = []index.AuditEvent{ev, done}
		return err
	})
	if err != nil {
		return nil, approvalError("approve", strconv.FormatInt(id, 10), err)
	}
	for _, ev := range evs {
		s.writeAudit(ctx, ev)
	}
	a, err := approvalOf(&row)
	return &a, err
}

// Reject rejects the request id by the user of ctx, its requester's being a withdrawal, with the
// origin o, whose reason is the decision's: while it is pending and has not expired (else
// index.ErrConflict), with its approval.reject row. The caller checks the role first.
func (s *Approvals) Reject(ctx context.Context, id int64, o Origin) (*Approval, error) {
	by, err := requester(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var (
		row index.Approval
		ev  index.AuditEvent
	)
	err = s.Index.Tx(ctx, func(w index.Writer) error {
		if _, err := s.pending(ctx, w, id, now); err != nil {
			return err
		}
		row = index.Approval{ID: id, State: index.ApprovalRejected, DecidedBy: by, DecidedAt: &now, DecisionReason: o.ChangeReason}
		if err := s.decide(ctx, w, &row); err != nil {
			return err
		}
		ev, err = s.record(ctx, w, ActionApprovalReject, &row, o, decisionDetails(&row))
		return err
	})
	if err != nil {
		return nil, approvalError("reject", strconv.FormatInt(id, 10), err)
	}
	s.writeAudit(ctx, ev)
	a, err := approvalOf(&row)
	return &a, err
}

// Expire marks expired the requests still pending at their expiry, in one transaction with an
// approval.expire row each by the principal of ctx (anonymous for a replica's own pass), and
// returns how many it marked. Of the replicas that find a request expired at once, one marks it
// (index.Writer.ExpireApprovals), so that its row is written once.
func (s *Approvals) Expire(ctx context.Context, o Origin) (int, error) {
	var evs []index.AuditEvent
	err := s.Index.Tx(ctx, func(w index.Writer) error {
		evs = nil
		rows, err := w.ExpireApprovals(ctx, s.now())
		if err != nil {
			return err
		}
		for i := range rows {
			ev, err := s.record(ctx, w, ActionApprovalExpire, &rows[i], o, decisionDetails(&rows[i]))
			if err != nil {
				return err
			}
			evs = append(evs, ev)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("auth: expire the requests for a second admin: %w", err)
	}
	for _, ev := range evs {
		s.writeAudit(ctx, ev)
	}
	return len(evs), nil
}

// RunExpiry runs Expire every ApprovalExpiryInterval until ctx ends, with the origin source expiry
// and the anonymous principal; a pass that fails is logged at warn, and the next one tries again.
func (s *Approvals) RunExpiry(ctx context.Context, log *slog.Logger) {
	t := time.NewTicker(ApprovalExpiryInterval)
	defer t.Stop()
	for {
		if n, err := s.Expire(ctx, Origin{Source: "expiry"}); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "auth: the expiry of the requests for a second admin", "error", err.Error())
		} else if n > 0 {
			log.InfoContext(ctx, "auth: requests for a second admin expired", "requests", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pending reads the request id on w, and returns it while it is pending and its expiry has not
// come at now; otherwise index.ErrConflict, and index.ErrNotFound for none.
func (s *Approvals) pending(ctx context.Context, w index.Writer, id int64, now time.Time) (*index.Approval, error) {
	row, err := w.GetApproval(ctx, id)
	switch {
	case err != nil:
		return nil, err
	case row.State != index.ApprovalPending:
		return nil, conflictf("request %d is %s: it was decided", id, row.State)
	case !now.Before(row.ExpiresAt):
		return nil, conflictf("request %d expired at %s: make the change again", id, row.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return row, nil
}

// decide stores the decision row on w while the request is pending (index.Writer.DecideApproval):
// one made meanwhile is index.ErrConflict.
func (s *Approvals) decide(ctx context.Context, w index.Writer, row *index.Approval) error {
	err := w.DecideApproval(ctx, row)
	if errors.Is(err, index.ErrConflict) {
		return conflictf("request %d was decided meanwhile", row.ID)
	}
	return err
}

// requesterHolds checks on w that the requester of a is still what made it (docs/spec/rbac.md
// §13): a live, enabled user with the admin role by its bindings, its own or a group's whose
// membership has not lapsed at now, whatever the admin mode, which is the credential's.
// Otherwise index.ErrConflict naming it: a request does not outlast its requester's revocation.
func (s *Approvals) requesterHolds(ctx context.Context, w index.Writer, a *index.Approval, now time.Time) error {
	p, err := ParsePrincipal(a.RequestedBy)
	if err != nil {
		return conflictf("request %d: its requester %s is not a user", a.ID, a.RequestedBy)
	}
	u, err := LookupUser(ctx, w, p)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return conflictf("request %d: its requester %s no longer exists", a.ID, a.RequestedBy)
	case err != nil:
		return err
	case u.Disabled:
		return conflictf("request %d: its requester %s is disabled", a.ID, u.Name)
	}
	admin, err := hasAdmin(ctx, w, u, now)
	switch {
	case err != nil:
		return err
	case !admin:
		return conflictf("request %d: its requester %s no longer has the admin role", a.ID, u.Name)
	}
	return nil
}

// hasAdmin reports whether the user u has an admin binding, its own or one of a group it is in
// whose membership has not lapsed at now (docs/spec/rbac.md §3, §8).
func hasAdmin(ctx context.Context, r index.Reader, u *index.User, now time.Time) (bool, error) {
	principals := []string{UserPrincipal(u).String()}
	groups, err := r.ListIdentityGroups(ctx)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if g.UID == u.UID && g.ExpiresAt != nil && now.Before(*g.ExpiresAt) {
			principals = append(principals, Principal{Kind: KindGroup, Name: g.Name}.String())
		}
	}
	for _, p := range principals {
		bindings, err := r.ListBindings(ctx, p)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(bindings, func(b index.Binding) bool { return b.Role == RoleAdmin }) {
			return true, nil
		}
	}
	return false, nil
}

// apply makes the change of the request a on w at now, after checking again what the request
// checked, and returns what it made, binding:<id> or token:<id>, and the change's own audit row,
// whose details have approval and the request's changeReason; a check that fails is
// index.ErrConflict naming it.
func (s *Approvals) apply(ctx context.Context, w index.Writer, a *index.Approval, o Origin, now time.Time) (string, index.AuditEvent,
	error) {
	o.ChangeReason = a.ChangeReason
	approval := map[string]any{"id": a.ID, "requestedBy": a.RequestedBy, "requestedAt": a.RequestedAt.UTC()}
	switch a.Kind {
	case index.ApprovalBinding:
		return s.applyBinding(ctx, w, a, o, approval, now)
	case index.ApprovalDeployToken:
		return s.applyToken(ctx, w, a, o, approval, now)
	}
	return "", index.AuditEvent{}, conflictf("request %d: a change of the kind %q is not one this server makes", a.ID, a.Kind)
}

// applyBinding makes the binding of the request a (apply).
func (s *Approvals) applyBinding(ctx context.Context, w index.Writer, a *index.Approval, o Origin, approval map[string]any,
	now time.Time) (string, index.AuditEvent, error) {
	var c bindingChange
	if err := json.Unmarshal(a.Change, &c); err != nil {
		return "", index.AuditEvent{}, fmt.Errorf("auth: the change of request %d: %w", a.ID, err)
	}
	p, err := s.Bindings.check(c.Principal, c.Role, c.RepoMask)
	if err != nil {
		return "", index.AuditEvent{}, conflictf("request %d: the binding no longer holds: %s", a.ID, err.Error())
	}
	bound, _, err := boundPrincipal(ctx, w, p)
	switch {
	case errors.Is(err, ErrInvalid):
		return "", index.AuditEvent{}, conflictf("request %d: its principal %s no longer exists", a.ID, c.Principal)
	case err != nil:
		return "", index.AuditEvent{}, err
	}
	b := index.Binding{Principal: bound, Role: c.Role, RepoMask: c.RepoMask, CreatedAt: now}
	if err := w.CreateBinding(ctx, &b); err != nil {
		if errors.Is(err, index.ErrConflict) {
			return "", index.AuditEvent{}, conflictf("request %d: the binding %s %s on %s exists already", a.ID, c.Principal, c.Role,
				c.RepoMask)
		}
		return "", index.AuditEvent{}, err
	}
	ev, err := recordAudit(ctx, w, s.Audit, index.AuditEvent{At: now, Action: ActionBindingCreate, Subject: b.Principal}, o,
		map[string]any{"id": b.ID, "role": b.Role, "repoMask": b.RepoMask, "approval": approval})
	return "binding:" + strconv.FormatInt(b.ID, 10), ev, err
}

// applyToken makes the deploy token over a mask of the request a (apply), with the hash, the
// expiry and the createdBy that the request kept.
func (s *Approvals) applyToken(ctx context.Context, w index.Writer, a *index.Approval, o Origin, approval map[string]any,
	now time.Time) (string, index.AuditEvent, error) {
	var k keptToken
	if err := json.Unmarshal(a.Change, &k); err != nil {
		return "", index.AuditEvent{}, fmt.Errorf("auth: the change of request %d: %w", a.ID, err)
	}
	held := func(err error) error {
		return conflictf("request %d: the deploy token no longer holds: %s", a.ID, err.Error())
	}
	if err := checkMask(k.RepoMask, s.Tokens.Formats); err != nil {
		return "", index.AuditEvent{}, held(err)
	}
	scopes, err := checkScopes(k.Scopes, TokenDeploy)
	if err != nil {
		return "", index.AuditEvent{}, held(err)
	}
	if err := checkTokenName(k.Name); err != nil {
		return "", index.AuditEvent{}, held(err)
	}
	own := tokenOwner{mask: k.RepoMask}
	l := s.Tokens.lifetime(TokenDeploy, own)
	switch {
	case k.ExpiresAt == nil && l.max > 0:
		return "", index.AuditEvent{}, held(fmt.Errorf("expiresAt: none, and %s now", l.bound))
	case k.ExpiresAt != nil:
		if _, err := expiry(l, Expiry{At: *k.ExpiresAt}, now); err != nil {
			return "", index.AuditEvent{}, held(err)
		}
	}
	row := &index.Token{Kind: TokenDeploy, RepoMask: k.RepoMask, Name: k.Name, Hash: k.Hash, Scopes: scopes, ExpiresAt: k.ExpiresAt,
		CreatedAt: now}
	if err := nameTaken(ctx, w, TokenDeploy, row, k.Name, "mask "+k.RepoMask, now); err != nil {
		return "", index.AuditEvent{}, held(err)
	}
	if err := w.CreateToken(ctx, row); err != nil {
		if errors.Is(err, index.ErrConflict) {
			return "", index.AuditEvent{}, conflictf("request %d: its token exists already", a.ID)
		}
		return "", index.AuditEvent{}, err
	}
	details := createDetails(ctx, row, l)
	delete(details, "createdBy")
	if k.CreatedBy != "" {
		details["createdBy"] = k.CreatedBy
	}
	details["approval"] = approval
	ev, err := recordAudit(ctx, w, s.Audit, index.AuditEvent{At: now, Action: ActionTokenCreate, Subject: subjectOf(own)}, o, details)
	return "token:" + strconv.FormatInt(row.ID, 10), ev, err
}

// decisionDetails are the details of the row of a decision of the request a as it is stored: its
// kind, its change as it is answered, and its requester (docs/spec/rbac.md §5).
func decisionDetails(a *index.Approval) map[string]any {
	d := map[string]any{"kind": a.Kind, "requestedBy": a.RequestedBy}
	if shown, err := shownChange(a); err == nil {
		d["change"] = shown
	}
	return d
}

// conflictf returns an *authError wrapping index.ErrConflict with the message fmt.Sprintf(msg,
// args...).
func conflictf(msg string, args ...any) error {
	return &authError{msg: fmt.Sprintf(msg, args...), err: index.ErrConflict}
}

// Approvers counts the users who could decide a request (docs/spec/rbac.md §13), for serve's
// warning at start under auth.approval second-admin: the live, enabled users, service accounts
// apart, which have no password and no sign-in, with an admin binding of their own or of a group
// of their last login, whose next login brings it again; under the admin mode (adminMFA, §12) the
// OIDC users only, whose sign-in alone reaches the role.
func Approvers(ctx context.Context, r index.Reader, adminMFA bool) (int, error) {
	bindings, err := r.ListBindings(ctx, "")
	if err != nil {
		return 0, err
	}
	admins := map[string]bool{}
	for _, b := range bindings {
		if b.Role == RoleAdmin {
			admins[b.Principal] = true
		}
	}
	groups, err := r.ListIdentityGroups(ctx)
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if admins[Principal{Kind: KindGroup, Name: g.Name}.String()] {
			admins[Principal{Kind: KindUser, Name: g.UID}.String()] = true
		}
	}
	n := 0
	for principal := range admins {
		p, perr := ParsePrincipal(principal)
		if perr != nil || p.Kind != KindUser {
			continue
		}
		u, err := LookupUser(ctx, r, p)
		switch {
		case errors.Is(err, index.ErrNotFound):
			continue
		case err != nil:
			return 0, err
		case u.Disabled, u.Kind == KindService, adminMFA && u.Kind != KindOIDC:
			continue
		}
		n++
	}
	return n, nil
}
