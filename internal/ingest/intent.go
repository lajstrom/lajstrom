package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The audit row of a registry write is written twice (docs/spec/index-schema.md, audit; INV-14
// of docs/spec/guarantees.md): pending, in a transaction of its own before the write, so that no
// write reaches the registry without its row; then completed, ok in the transaction that indexes
// the write, or failed with the error that the write was answered with. A row left pending, by a
// process that stopped between the two or a database that failed both, is unknown once reindex
// or the reconcile loop finds it older than ingest.claimTTL. With an audit.Writer, the seam of
// the tests, none of this happens: the Writer gets the event after the commit.

// intend writes ev as the pending row of the registry write that follows and fills ev.ID. Its
// failure is the write's: nothing reaches the registry without a row.
func (p *Pipeline) intend(ctx context.Context, ev *index.AuditEvent) error {
	if p.audit != nil {
		return nil
	}
	ev.State = index.AuditPending
	if err := p.index.AppendAudit(ctx, ev); err != nil {
		return fmt.Errorf("ingest: the audit row of a %s before the registry write: %w", ev.Action, err)
	}
	return nil
}

// complete records on w, in the transaction that indexes the write, that the write went
// through: ev's pending row is ok, with ev's details. An event without a pending row is
// appended ok.
func (p *Pipeline) complete(ctx context.Context, w index.Writer, ev *index.AuditEvent) error {
	if p.audit != nil {
		return nil
	}
	ev.State, ev.Error = index.AuditOK, ""
	if ev.ID == 0 {
		return w.AppendAudit(ctx, ev)
	}
	return w.CompleteAudit(ctx, ev)
}

// fail marks ev's pending row failed with cause, in a transaction of its own, also when ctx is
// done: the write failed, or the transaction of complete rolled back. When the database fails
// this too, the row stays pending until it is unknown; that is logged.
func (p *Pipeline) fail(ctx context.Context, ev *index.AuditEvent, cause error) {
	if p.audit != nil || ev.ID == 0 {
		return
	}
	ev.State, ev.Error = index.AuditFailed, cause.Error()
	if err := p.index.CompleteAudit(context.WithoutCancel(ctx), ev); err != nil {
		p.log.ErrorContext(ctx, "ingest: the audit row of a failed write not marked failed; it is unknown after ingest.claimTTL",
			"audit", ev.ID, "action", ev.Action, "repo", ev.Repo, "subject", ev.Subject, "error", err.Error())
	}
}

// setDetail sets key to v in the details of ev: what the write learnt after its row was
// written, such as the referrers a delete took with it.
func setDetail(ev *index.AuditEvent, key string, v any) {
	details := map[string]any{}
	_ = json.Unmarshal(ev.Details, &details) // ingest wrote them; nil is none
	details[key] = v
	ev.Details, _ = json.Marshal(details) // strings, numbers and booleans always marshal
}
