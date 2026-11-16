package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/index"
)

// The policy (#101; docs/spec/policy.md, docs/spec/index-schema.md, Policy): policy_rules and
// policy_state are control plane, verdicts derived, policy_seen a cache.

const policyRuleCols = "id, repo_mask, name, version, variant, effect, reason, created_by, created_at, expires_at"

func policyRuleDests(r *index.PolicyRule) []any {
	return []any{&r.ID, &r.RepoMask, &r.Name, &r.Version, &r.Variant, &r.Effect, &r.Reason, &r.CreatedBy,
		timeDest{&r.CreatedAt}, nullTimeDest{&r.ExpiresAt}}
}

const verdictCols = "id, repo_id, COALESCE(attachment_id, 0), path, subject, referrer, layer, verdict, reason, source, scope," +
	" valid_until, signed_by, created_at, indexed_at"

func verdictDests(v *index.Verdict) []any {
	return []any{&v.ID, &v.RepoID, &v.AttachmentID, &v.Path, (*string)(&v.Subject), (*string)(&v.Referrer), (*string)(&v.Layer),
		&v.Verdict, &v.Reason, &v.Source, &v.Scope, nullTimeDest{&v.ValidUntil}, &v.SignedBy, nullTimeDest{&v.CreatedAt},
		timeDest{&v.IndexedAt}}
}

// ListPolicyRules returns every rule by id.
func (c *conn) ListPolicyRules(ctx context.Context) ([]index.PolicyRule, error) {
	return getAll(ctx, c, "policy_rules", policyRuleDests, "SELECT "+policyRuleCols+" FROM policy_rules ORDER BY id")
}

// GetPolicyRule returns the rule id.
func (c *conn) GetPolicyRule(ctx context.Context, id int64) (*index.PolicyRule, error) {
	return getOne(ctx, c, fmt.Sprintf("policy_rules id %d", id), policyRuleDests,
		"SELECT "+policyRuleCols+" FROM policy_rules WHERE id = ?", id)
}

// PolicyGeneration returns policy_state.generation.
func (c *conn) PolicyGeneration(ctx context.Context) (int64, error) {
	var gen int64
	err := c.scanRow(ctx, "policy_state", "SELECT generation FROM policy_state WHERE id = 1", nil, &gen)
	return gen, err
}

// nextPolicyGeneration increments policy_state.generation, in the caller's transaction.
func (c *conn) nextPolicyGeneration(ctx context.Context) error {
	return c.execOne(ctx, "policy_state", "UPDATE policy_state SET generation = generation + 1, updated_at = ? WHERE id = 1",
		c.d.EncodeTime(now()))
}

// ListVerdicts returns the verdicts f selects, newest first, then by referrer and layer.
func (c *conn) ListVerdicts(ctx context.Context, f index.VerdictFilter) ([]index.Verdict, error) {
	var w where
	switch {
	case f.RepoID != 0 && f.Scoped:
		w.add("(repo_id = ? OR scope <> '')", f.RepoID)
	case f.RepoID != 0:
		w.add("repo_id = ?", f.RepoID)
	}
	if f.Subject != "" {
		w.add("subject = ?", string(f.Subject))
	}
	rows, err := getAll(ctx, c, "verdicts", verdictDests, "SELECT "+verdictCols+" FROM verdicts"+w.clause(), w.args...)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(rows, compareVerdicts)
	return rows, nil
}

// compareVerdicts orders verdicts newest first, those without a time last, then by referrer and
// layer.
func compareVerdicts(a, b index.Verdict) int {
	switch {
	case a.CreatedAt != nil && b.CreatedAt == nil:
		return -1
	case a.CreatedAt == nil && b.CreatedAt != nil:
		return 1
	case a.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt):
		return b.CreatedAt.Compare(*a.CreatedAt)
	}
	if n := strings.Compare(string(a.Referrer), string(b.Referrer)); n != 0 {
		return n
	}
	return strings.Compare(string(a.Layer), string(b.Layer))
}

// GetPolicySeen returns the policy_seen row of a version.
func (c *conn) GetPolicySeen(ctx context.Context, repoID int64, name, version, variant string) (*index.PolicySeen, error) {
	s := &index.PolicySeen{RepoID: repoID, Name: name, Version: version, Variant: variant}
	err := c.scanRow(ctx, fmt.Sprintf("policy_seen %s %s %q in repo %d", name, version, variant, repoID),
		"SELECT seen_at FROM policy_seen WHERE repo_id = ? AND name = ? AND version = ? AND variant = ?",
		[]any{repoID, name, version, variant}, timeDest{&s.SeenAt})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// CreatePolicyRule inserts r and increments the generation, in one transaction.
func (c *conn) CreatePolicyRule(ctx context.Context, r *index.PolicyRule) error {
	stamp(&r.CreatedAt, now())
	r.ExpiresAt = stampNull(r.ExpiresAt)
	return c.withTx(ctx, func(c *conn) error {
		err := c.scanRow(ctx, "policy_rules", "INSERT INTO policy_rules (repo_mask, name, version, variant, effect, reason, created_by,"+
			" created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
			[]any{r.RepoMask, r.Name, r.Version, r.Variant, r.Effect, r.Reason, r.CreatedBy, c.d.EncodeTime(r.CreatedAt),
				c.d.encodeNullTime(r.ExpiresAt)}, &r.ID)
		if err != nil {
			return err
		}
		return c.nextPolicyGeneration(ctx)
	})
}

// DeletePolicyRule removes the rule id and increments the generation, in one transaction.
func (c *conn) DeletePolicyRule(ctx context.Context, id int64) error {
	return c.withTx(ctx, func(c *conn) error {
		if err := c.execOne(ctx, fmt.Sprintf("policy_rules id %d", id), "DELETE FROM policy_rules WHERE id = ?", id); err != nil {
			return err
		}
		return c.nextPolicyGeneration(ctx)
	})
}

// insertVerdict inserts v, whose RepoID, Referrer and IndexedAt are set, with the attachment
// id, 0 for none, and fills its ID.
func (c *conn) insertVerdict(ctx context.Context, v *index.Verdict) error {
	v.ValidUntil, v.CreatedAt = stampNull(v.ValidUntil), stampNull(v.CreatedAt)
	var attachment any
	if v.AttachmentID != 0 {
		attachment = v.AttachmentID
	}
	return c.scanRow(ctx, fmt.Sprintf("verdicts %s layer %q in repo %d", v.Referrer, v.Layer, v.RepoID),
		"INSERT INTO verdicts (repo_id, attachment_id, path, subject, referrer, layer, verdict, reason, source, scope, valid_until,"+
			" signed_by, created_at, indexed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		[]any{v.RepoID, attachment, v.Path, string(v.Subject), string(v.Referrer), string(v.Layer), v.Verdict, v.Reason, v.Source,
			v.Scope, c.d.encodeNullTime(v.ValidUntil), v.SignedBy, c.d.encodeNullTime(v.CreatedAt), c.d.EncodeTime(v.IndexedAt)},
		&v.ID)
}

// attachedVerdicts reports whether a verdicts row belongs to one of the attachments that sel, a
// SELECT of attachments ids, names.
func (c *conn) attachedVerdicts(ctx context.Context, what, sel string, args ...any) (bool, error) {
	var one int
	err := c.scanRow(ctx, "verdicts of "+what, "SELECT 1 FROM verdicts WHERE attachment_id IN ("+sel+") LIMIT 1", args, &one)
	if errors.Is(err, index.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ReplaceOCIVerdicts makes the rows without an attachment of the referrer d of a repo exactly
// rows, and increments the generation, in one transaction.
func (c *conn) ReplaceOCIVerdicts(ctx context.Context, repoID int64, d digest.Digest, rows []index.Verdict) error {
	ts := now()
	return c.withTx(ctx, func(c *conn) error {
		n, err := c.exec(ctx, fmt.Sprintf("verdicts %s in repo %d", d, repoID),
			"DELETE FROM verdicts WHERE repo_id = ? AND referrer = ? AND attachment_id IS NULL", repoID, string(d))
		if err != nil {
			return err
		}
		for i := range rows {
			v := &rows[i]
			v.RepoID, v.Referrer, v.AttachmentID = repoID, d, 0
			stamp(&v.IndexedAt, ts)
			if err := c.insertVerdict(ctx, v); err != nil {
				return err
			}
		}
		if n == 0 && len(rows) == 0 {
			return nil
		}
		return c.nextPolicyGeneration(ctx)
	})
}

// DeleteOCIVerdicts removes the rows without an attachment of the referrer d of a repo, and
// increments the generation when it removed any.
func (c *conn) DeleteOCIVerdicts(ctx context.Context, repoID int64, d digest.Digest) (int, error) {
	var n int
	err := c.withTx(ctx, func(c *conn) (err error) {
		n, err = c.exec(ctx, fmt.Sprintf("verdicts %s in repo %d", d, repoID),
			"DELETE FROM verdicts WHERE repo_id = ? AND referrer = ? AND attachment_id IS NULL", repoID, string(d))
		if err != nil || n == 0 {
			return err
		}
		return c.nextPolicyGeneration(ctx)
	})
	return n, err
}

// TouchOCIVerdicts sets IndexedAt of the rows with the given ids, in statements of at most
// touchBatch ids.
func (c *conn) TouchOCIVerdicts(ctx context.Context, ids []int64, at time.Time) error {
	return c.withTx(ctx, func(c *conn) error {
		for chunk := range slices.Chunk(ids, touchBatch) {
			args := make([]any, 0, len(chunk)+1)
			args = append(args, c.d.EncodeTime(at))
			for _, id := range chunk {
				args = append(args, id)
			}
			if _, err := c.exec(ctx, fmt.Sprintf("verdicts %d to touch", len(chunk)),
				"UPDATE verdicts SET indexed_at = ? WHERE id IN ("+strings.Repeat(", ?", len(chunk))[2:]+")", args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// SweepOCIVerdicts removes the rows without an attachment of a repo's OCI repository path not
// seen since seenBefore, and increments the generation when it removed any.
func (c *conn) SweepOCIVerdicts(ctx context.Context, repoID int64, path string, seenBefore time.Time) (int, error) {
	var n int
	err := c.withTx(ctx, func(c *conn) (err error) {
		n, err = c.exec(ctx, fmt.Sprintf("verdicts of %q in repo %d not seen since %s", path, repoID, seenBefore.Format(time.RFC3339)),
			"DELETE FROM verdicts WHERE repo_id = ? AND path = ? AND attachment_id IS NULL AND indexed_at < ?",
			repoID, path, c.d.EncodeTime(seenBefore))
		if err != nil || n == 0 {
			return err
		}
		return c.nextPolicyGeneration(ctx)
	})
	return n, err
}

// PutPolicySeen inserts s unless its row exists, and reads the row's seen_at into s.
func (c *conn) PutPolicySeen(ctx context.Context, s *index.PolicySeen) error {
	stamp(&s.SeenAt, now())
	what := fmt.Sprintf("policy_seen %s %s %q in repo %d", s.Name, s.Version, s.Variant, s.RepoID)
	return c.withTx(ctx, func(c *conn) error {
		if _, err := c.exec(ctx, what, "INSERT INTO policy_seen (repo_id, name, version, variant, seen_at) VALUES (?, ?, ?, ?, ?)"+
			" ON CONFLICT (repo_id, name, version, variant) DO NOTHING",
			s.RepoID, s.Name, s.Version, s.Variant, c.d.EncodeTime(s.SeenAt)); err != nil {
			return err
		}
		return c.scanRow(ctx, what, "SELECT seen_at FROM policy_seen WHERE repo_id = ? AND name = ? AND version = ? AND variant = ?",
			[]any{s.RepoID, s.Name, s.Version, s.Variant}, timeDest{&s.SeenAt})
	})
}
