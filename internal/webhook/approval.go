package webhook

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// ApprovalData is the data of the event of a request for a second admin: approval.requested,
// .approved, .rejected or .expired (docs/spec/webhooks.md §3, #1336).
type ApprovalData struct {
	Common
	// Approval is the request as GET /api/v1/approvals/<id> answers it once the change of the
	// event is made, its change holding the fields of its kind only (ApprovalEvent).
	Approval auth.Approval `json:"approval"`
	// Names are the names at the time of the event of the users among the requester, the
	// decider and a binding's principal, by principal: {"user:<uid>": "alice"}.
	Names map[string]string `json:"names"`
}

// approvalTypes are the types of the events of a request for a second admin, by the audit action
// of their rows.
var approvalTypes = map[string]string{
	auth.ActionApprovalRequest: TypeApprovalRequested,
	auth.ActionApprovalApprove: TypeApprovalApproved,
	auth.ActionApprovalReject:  TypeApprovalRejected,
	auth.ActionApprovalExpire:  TypeApprovalExpired,
}

// ApprovalEvent writes on w, in the transaction of the audit row ev of a step of the life of the
// request a (a request, an approval, a rejection or a withdrawal, an expiry: ev.Action), its event
// (docs/spec/webhooks.md §1, §3; #1336): one delivery per active webhook of the admin, whose mask
// has a *, that covers the mask of a's change, a binding's */* whatever its own (an admin's or one
// on */*), a deploy token's its mask, and whose events include the type; nothing when none does.
// The data has a as the API answers it, its change narrowed to the fields of its kind, so that no
// hash, credential or sign-in a caller left in it reaches a receiver, and the names of its users,
// read on w only when a webhook gets the event. An action of no request is an error. It is
// auth.ApprovalEvents for auth.Approvals, which may not import this package.
func (o *Outbox) ApprovalEvent(ctx context.Context, w index.Writer, ev index.AuditEvent, a auth.Approval) error {
	if o == nil {
		return nil
	}
	typ, ok := approvalTypes[ev.Action]
	if !ok {
		return fmt.Errorf("webhook: %q is no action of a request for a second admin", ev.Action)
	}
	c, err := approvalChange(a)
	if err != nil {
		return err
	}
	e := Event{Type: typ, Subject: ev.Subject, Masks: []string{c.mask}, route: "/api/v1/approvals"}
	matched, err := matching(ctx, w, e)
	if err != nil || len(matched) == 0 {
		return err
	}
	names, err := approvalNames(ctx, w, a.RequestedBy, a.DecidedBy, c.principal)
	if err != nil {
		return err
	}
	a.Change = c.shown
	e.Data = ApprovalData{Common: CommonOf(ev), Approval: a, Names: names}
	return o.write(ctx, w, e, matched)
}

// eventChange is the change of a request as its events hold it, and what they need of it.
type eventChange struct {
	shown     json.RawMessage // the fields of its kind only
	mask      string          // the mask whose webhooks of the admin get its events
	principal string          // a binding's
}

// approvalChange returns the change of a as its events hold it: a binding's principal, role and
// repoMask, whose events go to the webhooks of */*, and a deploy token's name, repoMask, scopes and
// expiresAt, whose events go to those that cover its mask. A kind this release makes no request
// of holds nothing, and its events go to the webhooks of */*, as do those of a mask that does not
// parse.
func approvalChange(a auth.Approval) (eventChange, error) {
	var (
		c     = eventChange{mask: auth.MaskAll}
		shown any
	)
	switch a.Kind {
	case index.ApprovalBinding:
		var b struct {
			Principal string `json:"principal"`
			Role      string `json:"role"`
			RepoMask  string `json:"repoMask"`
		}
		if err := json.Unmarshal(a.Change, &b); err != nil {
			return eventChange{}, fmt.Errorf("webhook: the change of request %d: %w", a.ID, err)
		}
		shown, c.principal = b, b.Principal
	case index.ApprovalDeployToken:
		var t struct {
			Name      string     `json:"name"`
			RepoMask  string     `json:"repoMask"`
			Scopes    []string   `json:"scopes"`
			ExpiresAt *time.Time `json:"expiresAt"`
		}
		if err := json.Unmarshal(a.Change, &t); err != nil {
			return eventChange{}, fmt.Errorf("webhook: the change of request %d: %w", a.ID, err)
		}
		shown = t
		if _, _, err := auth.ParseMask(t.RepoMask); err == nil {
			c.mask = t.RepoMask
		}
	default:
		c.shown = json.RawMessage(`{}`)
		return c, nil
	}
	raw, err := json.Marshal(shown)
	if err != nil {
		return eventChange{}, fmt.Errorf("webhook: the change of request %d: %w", a.ID, err)
	}
	c.shown = raw
	return c, nil
}

// approvalNames returns, read on r, the name of each user among principals, by principal: a
// deleted user's the name it had when deleted. A principal that is no user:<uid>, or whose user
// does not exist, has none.
func approvalNames(ctx context.Context, r index.Reader, principals ...string) (map[string]string, error) {
	names := map[string]string{}
	for _, s := range principals {
		p, err := auth.ParsePrincipal(s)
		if err != nil || p.Kind != auth.KindUser || !index.IsUID(p.Name) || names[s] != "" {
			continue
		}
		u, err := r.GetUserByUID(ctx, p.Name)
		switch {
		case errors.Is(err, index.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("webhook: the name of %s: %w", s, err)
		}
		names[s] = cmp.Or(u.DeletedName, u.Name)
	}
	return names, nil
}
