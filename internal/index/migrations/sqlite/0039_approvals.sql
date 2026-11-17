-- 0039: approvals, the requests that wait for a second admin (#1285; docs/spec/rbac.md §13,
-- docs/spec/index-schema.md, Approvals). A change of the control plane that auth.approval
-- second-admin holds back, an admin binding, a binding on */* or a deploy token over a mask with
-- write, is kept as the change, as JSON, which its approval makes in bindings or tokens; nothing
-- that reads those two reads this table, so a request grants nothing. The state changes once,
-- from pending, in a statement that changes it only while it is pending: of an approval, a
-- rejection and the replicas that mark an expiry, one decides. The same change pending twice is
-- a conflict (approvals_pending). Rows are never deleted, and are not in a snapshot. Expand
-- only: the binaries before it neither read nor write the table, and make every change at once
-- (SQLite).

CREATE TABLE approvals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    kind            TEXT    NOT NULL,
    subject         TEXT    NOT NULL,
    change          TEXT    NOT NULL,
    change_reason   TEXT,
    state           TEXT    NOT NULL,
    requested_by    TEXT    NOT NULL,
    requested_at    TEXT    NOT NULL,
    expires_at      TEXT    NOT NULL,
    decided_by      TEXT,
    decided_at      TEXT,
    decision_reason TEXT,
    result          TEXT
);

CREATE UNIQUE INDEX approvals_pending ON approvals (kind, subject) WHERE state = 'pending';
CREATE INDEX approvals_expires ON approvals (expires_at) WHERE state = 'pending';
