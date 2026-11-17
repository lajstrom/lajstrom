package ci

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/config"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/webhook"
)

// The steps of a CI trust policy of the API that grants write (docs/spec/rbac.md §11, decided on
// #937): its write grants within auth.ci.apiWriteRepos, an expiry at most WriteLifetime ahead, a
// fresh sign-in with a second factor of whoever makes, changes, renews or approves it, and, with
// auth.ci.apiWriteApproval second-admin, the approval of another admin, until which it waits
// pending and grants nothing.

// ExpiryInterval is how often each replica looks for the policies whose expiry has passed
// (RunExpiry).
const ExpiryInterval = time.Minute

// steps sets on r, a policy as a create or a change by the principal of ctx leaves it, what its
// grants take at now. One that grants write: the steps of writeSteps, its expiry, expiresAt or
// WriteLifetime from now, and pending until another admin approves it unless
// auth.ci.apiWriteApproval is none. One that reads only: active, and expiresAt refused.
func (s *Service) steps(ctx context.Context, r *index.CIPolicy, expiresAt *time.Time, now time.Time) error {
	r.RenewalExpiresAt, r.ApprovedBy, r.ApprovedAt = nil, "", nil
	if !writes(configPolicy(*r)) {
		if expiresAt != nil {
			return &serviceError{msg: "expiresAt: a policy that grants reader only does not expire", err: auth.ErrInvalid}
		}
		r.State, r.ExpiresAt = index.CIPolicyActive, nil
		return nil
	}
	exp, err := s.writeSteps(ctx, r.Grants, expiresAt, now)
	if err != nil {
		return err
	}
	r.ExpiresAt, r.State = &exp, s.waits()
	return nil
}

// writeSteps checks what a create, a change or a renewal of a policy with grants that write
// takes at now, and returns its expiry: every mask of a grant that writes within
// auth.ci.apiWriteRepos (auth.ErrScope), expiresAt after now and at most WriteLifetime ahead, by
// default WriteLifetime ahead (auth.ErrInvalid), and the fresh sign-in with a second factor of
// the credential of ctx (auth.FreshSignIn: auth.ErrScope, an *auth.StepUpError, the API's 401, for a
// web UI's sign-in through OIDC short of it).
func (s *Service) writeSteps(ctx context.Context, grants []index.CIGrant, expiresAt *time.Time, now time.Time) (time.Time, error) {
	if err := s.writeRepos(grants); err != nil {
		return time.Time{}, err
	}
	exp := now.Add(WriteLifetime)
	if expiresAt != nil {
		switch {
		case !expiresAt.After(now):
			return time.Time{}, &serviceError{msg: "expiresAt: must be in the future", err: auth.ErrInvalid}
		case expiresAt.After(exp):
			return time.Time{}, &serviceError{msg: fmt.Sprintf("expiresAt: must be at most %d days ahead, a policy that grants "+
				"write is renewed", int(WriteLifetime/(24*time.Hour))), err: auth.ErrInvalid}
		}
		exp = *expiresAt
	}
	if err := auth.FreshSignIn(auth.CredentialFrom(ctx), now); err != nil {
		return time.Time{}, &serviceError{msg: "a policy of the API that grants write: " + err.Error(), err: err}
	}
	return exp.UTC(), nil
}

// writeRepos returns auth.ErrScope naming the first grant that writes outside
// auth.ci.apiWriteRepos, or nil.
func (s *Service) writeRepos(grants []index.CIGrant) error {
	for i, g := range grants {
		if config.CIWrites(g.Role) && !s.Config.WriteReposAllow(g.Mask) {
			allowed := "none: write it in the configuration (auth.ci.policies)"
			if len(s.Config.APIWriteRepos) > 0 {
				allowed = strings.Join(s.Config.APIWriteRepos, ", ") + "; write to other repos in the configuration (auth.ci.policies)"
			}
			return &serviceError{msg: fmt.Sprintf("grants[%d]: %s on %s is outside auth.ci.apiWriteRepos, %s", i, g.Role, g.Mask, allowed),
				err: auth.ErrScope}
		}
	}
	return nil
}

// waits is the state of a policy that writes once its steps are taken: pending until a second
// admin approves it, or active with auth.ci.apiWriteApproval none.
func (s *Service) waits() string {
	if s.Config.APIWriteApproval == config.CIApprovalNone {
		return index.CIPolicyActive
	}
	return index.CIPolicyPending
}

// change runs one change of the policy of the API called name in a transaction: the row read,
// the condition of ctx checked on it (index.WithCondition), apply's row stored while the row is
// at the revision read (else index.ErrConflict, a change meanwhile), and its audit row of action
// and its event of typ. It returns the row as stored.
func (s *Service) change(ctx context.Context, name, action, typ string, o auth.Origin,
	apply func(old *index.CIPolicy) (index.CIPolicy, error)) (index.CIPolicy, error) {
	var r index.CIPolicy
	err := s.Index.Tx(index.ConditionContext(ctx), func(w index.Writer) error {
		old, err := s.row(ctx, w, name)
		if err != nil {
			return err
		}
		if err := index.CheckCondition(ctx, old.ID, old.Revision); err != nil {
			return err
		}
		if r, err = apply(old); err != nil {
			return err
		}
		r.Name, r.CreatedBy, r.CreatedAt = old.Name, old.CreatedBy, old.CreatedAt
		if err := w.UpdateCIPolicy(ctx, &r); err != nil {
			if errors.Is(err, index.ErrConflict) {
				return &serviceError{msg: fmt.Sprintf("policy %s changed meanwhile: read it again", name), err: err}
			}
			return err
		}
		return s.changed(ctx, w, action, typ, r, old, o)
	})
	return r, s.txError(strings.TrimPrefix(action, "ci.policy."), name, err)
}

// Renew renews the policy of the API called name, one that grants write, by the principal of ctx,
// with its ci.policy.renew row and event: a new expiry, expiresAt or WriteLifetime from now, under
// the steps of a change (writeSteps). With auth.ci.apiWriteApproval second-admin it waits for
// another admin's approval: a policy that counts keeps its expiry until then, the renewal beside
// it (Renewal); an expired one waits pending. A policy that reads only, one that waits pending,
// and one of the configuration are index.ErrConflict. The condition of ctx is checked on the row
// the transaction read.
func (s *Service) Renew(ctx context.Context, name string, expiresAt *time.Time, o auth.Origin) (*Policy, error) {
	if err := s.readOnly(name); err != nil {
		return nil, err
	}
	now, by := s.now(), auth.PrincipalFrom(ctx).String()
	r, err := s.change(ctx, name, ActionPolicyRenew, webhook.TypeCIPolicyRenewed, o, func(old *index.CIPolicy) (index.CIPolicy, error) {
		switch {
		case !writes(configPolicy(*old)):
			return index.CIPolicy{}, &serviceError{msg: fmt.Sprintf("policy %s grants reader only: it does not expire", name),
				err: index.ErrConflict}
		case old.State == index.CIPolicyPending:
			return index.CIPolicy{}, &serviceError{msg: fmt.Sprintf("policy %s waits for an admin's approval: approve it, or "+
				"change it with its new expiresAt", name), err: index.ErrConflict}
		}
		exp, err := s.writeSteps(ctx, old.Grants, expiresAt, now)
		if err != nil {
			return index.CIPolicy{}, err
		}
		r := *old
		r.UpdatedBy, r.RenewalExpiresAt = by, nil
		switch {
		case s.waits() == index.CIPolicyActive:
			r.ExpiresAt, r.State = &exp, index.CIPolicyActive
		case expired(*old, now):
			r.ExpiresAt, r.State, r.ApprovedBy, r.ApprovedAt = &exp, index.CIPolicyPending, "", nil
		default:
			r.RenewalExpiresAt = &exp
		}
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	pol := fromRow(r, now)
	return &pol, nil
}

// Approve approves what waits on the policy of the API called name: a policy pending, which then
// counts, or a renewal, whose expiry it then has; by the principal of ctx, with its
// ci.policy.approve row and event. The approver has a fresh sign-in with a second factor
// (auth.FreshSignIn) and is not the principal of the change that waits (UpdatedBy), else
// auth.ErrScope; the grants that write are still within auth.ci.apiWriteRepos. ctx must have a
// condition (index.WithCondition, the If-Match of the API), so that an approval names the revision
// it approves: none is auth.ErrInvalid. Nothing that waits, an expiry passed before the approval,
// and a policy of the configuration are index.ErrConflict.
func (s *Service) Approve(ctx context.Context, name string, o auth.Origin) (*Policy, error) {
	if err := s.readOnly(name); err != nil {
		return nil, err
	}
	if !index.HasCondition(ctx) {
		return nil, &serviceError{msg: "If-Match: an approval names the revision it approves: send the ETag of the policy as you " +
			"read it", err: auth.ErrInvalid}
	}
	now, by := s.now(), auth.PrincipalFrom(ctx).String()
	if err := auth.FreshSignIn(auth.CredentialFrom(ctx), now); err != nil {
		return nil, &serviceError{msg: "an approval: " + err.Error(), err: err}
	}
	r, err := s.change(ctx, name, ActionPolicyApprove, webhook.TypeCIPolicyApproved, o, func(old *index.CIPolicy) (index.CIPolicy, error) {
		r := *old
		switch {
		case old.State == index.CIPolicyPending:
			r.State = index.CIPolicyActive
		case old.RenewalExpiresAt != nil:
			r.ExpiresAt, r.RenewalExpiresAt, r.State = old.RenewalExpiresAt, nil, index.CIPolicyActive
		default:
			return index.CIPolicy{}, &serviceError{msg: fmt.Sprintf("policy %s waits for no approval", name), err: index.ErrConflict}
		}
		switch {
		case by == old.UpdatedBy:
			return index.CIPolicy{}, &serviceError{msg: fmt.Sprintf("policy %s: the admin who made the change that waits does not "+
				"approve it: another admin does", name), err: auth.ErrScope}
		case r.ExpiresAt == nil || !r.ExpiresAt.After(now):
			return index.CIPolicy{}, &serviceError{msg: fmt.Sprintf("policy %s: its expiry passed before the approval: renew it", name),
				err: index.ErrConflict}
		}
		if err := s.writeRepos(old.Grants); err != nil {
			return index.CIPolicy{}, err
		}
		r.ApprovedBy, r.ApprovedAt = by, &now
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	pol := fromRow(r, now)
	return &pol, nil
}

// Expire marks expired the policies of the API whose expiry has passed, active or pending, each
// with its ci.policy.expire row and event by the principal of ctx (anonymous for a replica's own
// pass), and returns how many it marked. Each mark is an update at the revision read: of the
// replicas that find one policy expired at once, one marks it, so that its row and its event are
// written once. A policy changed since the read is left for the next pass.
func (s *Service) Expire(ctx context.Context, o auth.Origin) (int, error) {
	rows, err := s.Index.ListCIPolicies(ctx)
	if err != nil {
		return 0, fmt.Errorf("ci: the trust policies that expire: %w", err)
	}
	now, n := s.now(), 0
	for _, row := range rows {
		if row.State == index.CIPolicyExpired || row.ExpiresAt == nil || row.ExpiresAt.After(now) {
			continue
		}
		err := s.Index.Tx(ctx, func(w index.Writer) error {
			r := row
			r.State = index.CIPolicyExpired
			if err := w.UpdateCIPolicy(ctx, &r); err != nil {
				return err
			}
			return s.changed(ctx, w, ActionPolicyExpire, webhook.TypeCIPolicyExpired, r, nil, o)
		})
		switch {
		case errors.Is(err, index.ErrConflict), errors.Is(err, index.ErrNotFound): // another replica, or a change
		case err != nil:
			return n, fmt.Errorf("ci: expire the trust policy %s: %w", row.Name, err)
		default:
			n++
		}
	}
	return n, nil
}

// RunExpiry runs Expire every ExpiryInterval until ctx ends, with the origin source expiry and
// the anonymous principal; a pass that fails is logged at warn, and the next one tries again.
func (s *Service) RunExpiry(ctx context.Context, log *slog.Logger) {
	t := time.NewTicker(ExpiryInterval)
	defer t.Stop()
	for {
		if n, err := s.Expire(ctx, auth.Origin{Source: "expiry"}); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "ci: the expiry of the trust policies of the API", "error", err.Error())
		} else if n > 0 {
			log.InfoContext(ctx, "ci: trust policies of the API expired", "policies", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
