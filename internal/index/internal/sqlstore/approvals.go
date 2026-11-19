package sqlstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The requests that wait for a second admin (docs/spec/rbac.md §13, docs/spec/index-schema.md,
// Approvals): control plane that grants nothing, so no write moves auth_version. A state changes
// once, from pending, in a statement that changes it only while it is pending.

const approvalCols = "id, kind, subject, change, change_reason, state, requested_by, requested_at, expires_at, decided_by," +
	" decided_at, decision_reason, result"

func approvalDests(a *index.Approval) []any {
	return []any{&a.ID, &a.Kind, &a.Subject, rawDest{&a.Change}, textDest{&a.ChangeReason}, &a.State, &a.RequestedBy,
		timeDest{&a.RequestedAt}, timeDest{&a.ExpiresAt}, textDest{&a.DecidedBy}, nullTimeDest{&a.DecidedAt},
		textDest{&a.DecisionReason}, textDest{&a.Result}}
}

// GetApproval returns the request id.
func (c *conn) GetApproval(ctx context.Context, id int64) (*index.Approval, error) {
	return getOne(ctx, c, fmt.Sprintf("approvals id %d", id), approvalDests, "SELECT "+approvalCols+" FROM approvals WHERE id = ?", id)
}

// ListApprovals returns the requests of state ("" any) below the id before (0: from the newest),
// newest first, at most limit.
func (c *conn) ListApprovals(ctx context.Context, state string, before int64, limit int) ([]index.Approval, error) {
	if limit < 1 {
		return nil, nil
	}
	w := where{}
	if state != "" {
		w.add("state = ?", state)
	}
	if before > 0 {
		w.add("id < ?", before)
	}
	return getAll(ctx, c, "approvals", approvalDests, "SELECT "+approvalCols+" FROM approvals"+w.clause()+" ORDER BY id DESC LIMIT ?",
		append(w.args, limit)...)
}

// CountPendingApprovals counts the pending requests whose expires_at is after at and whose
// requested_by is not notBy. The state is written in the statement, so that the planner of
// Postgres reads the partial index approvals_expires whatever the plan.
func (c *conn) CountPendingApprovals(ctx context.Context, at time.Time, notBy string) (int, error) {
	var n int
	err := c.scanRow(ctx, "approvals", "SELECT COUNT(*) FROM approvals WHERE state = 'pending' AND expires_at > ? AND requested_by <> ?",
		[]any{c.d.EncodeTime(at), notBy}, &n)
	return n, err
}

// CreateApproval inserts a pending: RequestedAt now when zero, an empty Change {}. The unique
// index approvals_pending makes the same change pending twice index.ErrConflict.
func (c *conn) CreateApproval(ctx context.Context, a *index.Approval) error {
	stamp(&a.RequestedAt, now())
	a.ExpiresAt = Normalize(a.ExpiresAt)
	a.State, a.DecidedBy, a.DecidedAt, a.DecisionReason, a.Result = index.ApprovalPending, "", nil, "", ""
	change := string(a.Change)
	if change == "" {
		change = "{}"
	}
	return c.scanRow(ctx, "approvals "+a.Kind+" "+a.Subject,
		"INSERT INTO approvals (kind, subject, change, change_reason, state, requested_by, requested_at, expires_at)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{a.Kind, a.Subject, change, textArg(a.ChangeReason), a.State, a.RequestedBy, c.d.EncodeTime(a.RequestedAt),
			c.d.EncodeTime(a.ExpiresAt)},
		&a.ID)
}

// DecideApproval stores the decision of a while the row is pending, in one UPDATE that returns
// the row; none returned is a row that is not pending (index.ErrConflict) or no row.
func (c *conn) DecideApproval(ctx context.Context, a *index.Approval) error {
	what := fmt.Sprintf("approvals id %d", a.ID)
	if a.DecidedAt == nil {
		a.DecidedAt = new(time.Time)
	}
	decided := stamp(a.DecidedAt, now())
	err := c.scanRow(ctx, what, "UPDATE approvals SET state = ?, decided_by = ?, decided_at = ?, decision_reason = ?, result = ?"+
		" WHERE id = ? AND state = ? RETURNING "+approvalCols,
		[]any{a.State, textArg(a.DecidedBy), c.d.EncodeTime(decided), textArg(a.DecisionReason), textArg(a.Result), a.ID,
			index.ApprovalPending},
		approvalDests(a)...)
	if !errors.Is(err, index.ErrNotFound) {
		return err
	}
	row, err := c.GetApproval(ctx, a.ID)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is %s, not pending", index.ErrConflict, what, row.State)
}

// ExpireApprovals marks expired, in one UPDATE that returns them, the pending requests whose
// expires_at is not after at.
func (c *conn) ExpireApprovals(ctx context.Context, at time.Time) ([]index.Approval, error) {
	rows, err := getAll(ctx, c, "approvals", approvalDests,
		"UPDATE approvals SET state = ?, decided_by = ?, decided_at = ? WHERE state = ? AND expires_at <= ? RETURNING "+approvalCols,
		index.ApprovalExpired, "anonymous", c.d.EncodeTime(at), index.ApprovalPending, c.d.EncodeTime(at))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b index.Approval) int { return cmp.Compare(a.ID, b.ID) })
	return rows, nil
}
