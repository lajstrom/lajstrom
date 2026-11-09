package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// Coordination: reindex_runs, the runs of reindex and the lock of their scopes
// (docs/spec/index-schema.md, Coordination).

// reindexLockID is the key of the Postgres advisory lock that serialises the starts of reindex
// runs: "reindex" in ASCII. It differs from the migration lock, so a start never waits for a
// migration.
const reindexLockID int64 = 0x7265696e646578

// keptRuns is how many finished runs of a scope FinishReindexRun keeps.
const keptRuns = 10

const reindexRunCols = "id, repo_id, state, source, principal, replica, options, started_at, heartbeat_at, finished_at, report, error"

func reindexRunDests(r *index.ReindexRun) []any {
	return []any{&r.ID, nullIDDest{&r.RepoID}, &r.State, &r.Source, &r.Principal, &r.Replica,
		jsonDest[index.ReindexOptions]{&r.Options}, timeDest{&r.StartedAt}, timeDest{&r.HeartbeatAt},
		nullTimeDest{&r.FinishedAt}, rawDest{&r.Report}, textDest{&r.Error}}
}

// runScope is the condition on reindex_runs of the runs whose scope is repoID's (nil: every
// repo).
func runScope(repoID *int64) (string, []any) {
	if repoID == nil {
		return "repo_id IS NULL", nil
	}
	return "repo_id = ?", []any{*repoID}
}

// optionsArg marshals the options of a run for the nullable json column: none set is NULL.
func optionsArg(o index.ReindexOptions) any {
	if o == (index.ReindexOptions{}) {
		return nil
	}
	b, _ := json.Marshal(o) // two booleans always marshal
	return string(b)
}

// GetReindexRun returns the reindex run with the given id.
func (c *conn) GetReindexRun(ctx context.Context, id int64) (*index.ReindexRun, error) {
	return getOne(ctx, c, fmt.Sprintf("reindex_runs id %d", id), reindexRunDests,
		"SELECT "+reindexRunCols+" FROM reindex_runs WHERE id = ?", id)
}

// LatestReindexRun returns the newest run that covered repoID: its own or a run of every repo;
// repoID 0 selects the runs of every repo only.
func (c *conn) LatestReindexRun(ctx context.Context, repoID int64) (*index.ReindexRun, error) {
	cond, args := "repo_id IS NULL", []any(nil)
	what := "reindex_runs of every repo"
	if repoID != 0 {
		cond, args = "(repo_id = ? OR repo_id IS NULL)", []any{repoID}
		what = fmt.Sprintf("reindex_runs of repo %d", repoID)
	}
	return getOne(ctx, c, what, reindexRunDests,
		"SELECT "+reindexRunCols+" FROM reindex_runs WHERE "+cond+" ORDER BY started_at DESC, id DESC LIMIT 1", args...)
}

// StartReindexRun takes the reindex lock, closes the stale running runs that overlap r as
// abandoned, refuses a fresh one with a *index.ReindexConflictError, and inserts r as running.
func (c *conn) StartReindexRun(ctx context.Context, r *index.ReindexRun, staleBefore time.Time) error {
	ts := now()
	staleBefore = Normalize(staleBefore)
	return c.withTx(ctx, func(c *conn) error {
		if c.d.XactLock != "" {
			if _, err := c.exec(ctx, "the reindex lock", c.d.XactLock, reindexLockID); err != nil {
				return err
			}
		}
		cond, args := "", []any{index.ReindexRunning}
		if r.RepoID != nil { // a run of every repo overlaps every run
			cond, args = " AND (repo_id IS NULL OR repo_id = ?)", append(args, *r.RepoID)
		}
		running, err := getAll(ctx, c, "running reindex_runs", reindexRunDests,
			"SELECT "+reindexRunCols+" FROM reindex_runs WHERE state = ?"+cond+" ORDER BY id", args...)
		if err != nil {
			return err
		}
		for _, run := range running {
			if !run.HeartbeatAt.Before(staleBefore) {
				return &index.ReindexConflictError{Running: run}
			}
			err := c.execOne(ctx, fmt.Sprintf("reindex_runs id %d", run.ID),
				"UPDATE reindex_runs SET state = ?, finished_at = ?, error = ? WHERE id = ? AND state = ?",
				index.ReindexAbandoned, c.d.EncodeTime(ts),
				"no heartbeat since "+run.HeartbeatAt.Format(time.RFC3339), run.ID, index.ReindexRunning)
			if err != nil {
				return err
			}
		}
		r.State, r.FinishedAt, r.Report, r.Error = index.ReindexRunning, nil, nil, ""
		stamp(&r.HeartbeatAt, stamp(&r.StartedAt, ts)) // a run beats first when it starts
		return c.scanRow(ctx, "reindex_runs",
			"INSERT INTO reindex_runs (repo_id, state, source, principal, replica, options, started_at, heartbeat_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
			[]any{idArg(r.RepoID), r.State, r.Source, r.Principal, r.Replica, optionsArg(r.Options),
				c.d.EncodeTime(r.StartedAt), c.d.EncodeTime(r.HeartbeatAt)},
			&r.ID)
	})
}

// HeartbeatReindexRun sets HeartbeatAt of a running run.
func (c *conn) HeartbeatReindexRun(ctx context.Context, id int64, at time.Time) error {
	return c.execOne(ctx, fmt.Sprintf("running reindex_runs id %d", id),
		"UPDATE reindex_runs SET heartbeat_at = ? WHERE id = ? AND state = ?", c.d.EncodeTime(at), id, index.ReindexRunning)
}

// FinishReindexRun closes the running run r.ID with r's State, FinishedAt, Report and Error,
// then deletes the finished runs of its scope beyond the keptRuns newest.
func (c *conn) FinishReindexRun(ctx context.Context, r *index.ReindexRun) error {
	switch r.State {
	case index.ReindexDone, index.ReindexFailed, index.ReindexAbandoned:
	default:
		return fmt.Errorf("index: finish reindex_runs id %d in state %q", r.ID, r.State)
	}
	if r.FinishedAt == nil {
		ts := now()
		r.FinishedAt = &ts
	}
	r.FinishedAt = stampNull(r.FinishedAt)
	return c.withTx(ctx, func(c *conn) error {
		var repoID *int64
		err := c.scanRow(ctx, fmt.Sprintf("running reindex_runs id %d", r.ID),
			"UPDATE reindex_runs SET state = ?, finished_at = ?, report = ?, error = ? WHERE id = ? AND state = ? RETURNING repo_id",
			[]any{r.State, c.d.EncodeTime(*r.FinishedAt), rawArg(r.Report), textArg(r.Error), r.ID, index.ReindexRunning},
			nullIDDest{&repoID})
		if err != nil {
			return err
		}
		cond, scope := runScope(repoID)
		args := slices.Concat(scope, []any{index.ReindexRunning}, scope, []any{index.ReindexRunning, keptRuns})
		_, err = c.exec(ctx, "finished reindex_runs",
			"DELETE FROM reindex_runs WHERE "+cond+" AND state <> ? AND id NOT IN ("+
				"SELECT id FROM reindex_runs WHERE "+cond+" AND state <> ? ORDER BY started_at DESC, id DESC LIMIT ?)",
			args...)
		return err
	})
}
