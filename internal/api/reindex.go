package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/index/reindex"
)

// Reindexer starts a reindex run that goes on in the background, and returns it once it holds
// the lock of its scope: a *index.ReindexConflictError when another run does
// (docs/spec/http-routing.md §4). cmd/lajstrom wires internal/index/reindex.Worker behind it.
type Reindexer interface {
	Start(ctx context.Context, req reindex.Request) (*index.ReindexRun, error)
}

// ReindexRun is a reindex run as POST and GET /api/v1/repos/{format}/{repo}/reindex answer it
// (docs/api.md).
type ReindexRun struct {
	ID          int64      `json:"id"`
	State       string     `json:"state"`
	Source      string     `json:"source"`
	Principal   string     `json:"principal"`
	Replica     string     `json:"replica"`
	AllRepos    bool       `json:"allRepos"` // a run of every repo, which covered this one
	Prune       bool       `json:"prune"`
	Verify      bool       `json:"verify"`
	StartedAt   time.Time  `json:"startedAt"`
	HeartbeatAt time.Time  `json:"heartbeatAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	Error       string     `json:"error,omitempty"`
	// Report is the repo's part of the run's report; null while the run is running, and for a
	// run of every repo that stopped before it reached the repo.
	Report *reindex.RepoReport `json:"report"`
}

// reindexBody is the optional body of POST …/reindex.
type reindexBody struct {
	Prune  bool `json:"prune"`
	Verify bool `json:"verify"`
}

// reindexRepo starts the reindex of one repo (manage): 202 with the run before it reads the
// registry, 409 when a run that overlaps holds the lock on any replica or in the CLI, 501 when
// the server has no Reindexer. The body {"prune", "verify"} is optional; both default to false,
// as for lajstrom reindex --repo.
func (h *handler) reindexRepo(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Index.GetRepo(r.Context(), f, name)
	if err != nil {
		return 0, nil, repoError("reindex", f, name, err)
	}
	var body reindexBody
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &body); err != nil {
			return 0, nil, err
		}
	}
	if h.reindex == nil {
		return 0, nil, &clientError{msg: "reindex is not enabled in this server", err: format.ErrUnsupported}
	}
	req := reindex.Request{Repo: repo, Prune: body.Prune, Verify: body.Verify, Source: SourceAPI,
		Principal: auth.PrincipalFrom(r.Context()).String()}
	run, err := h.reindex.Start(r.Context(), req)
	var conflict *index.ReindexConflictError
	if errors.As(err, &conflict) {
		return 0, nil, &clientError{err: err, msg: fmt.Sprintf("a reindex is already running on %s since %s (run %d)",
			conflict.Running.Replica, conflict.Running.StartedAt.Format(time.RFC3339), conflict.Running.ID)}
	}
	if err != nil {
		return 0, nil, err
	}
	out, err := reindexRun(run, repo)
	return http.StatusAccepted, out, err
}

// reindexStatus answers the latest run that covered the repo (manage), its own or a run of
// every repo; 404 when none has.
func (h *handler) reindexStatus(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionManage, f, name); err != nil {
		return 0, nil, err
	}
	repo, err := h.repos.Index.GetRepo(r.Context(), f, name)
	if err != nil {
		return 0, nil, repoError("reindex", f, name, err)
	}
	run, err := h.repos.Index.LatestReindexRun(r.Context(), repo.ID)
	if errors.Is(err, index.ErrNotFound) {
		return 0, nil, &clientError{msg: fmt.Sprintf("no reindex has covered %s/%s", f, name), err: err}
	}
	if err != nil {
		return 0, nil, err
	}
	out, err := reindexRun(run, repo)
	return http.StatusOK, out, err
}

// reindexRun is the answer about run for repo, with the repo's part of the report.
func reindexRun(run *index.ReindexRun, repo *index.Repo) (*ReindexRun, error) {
	out := &ReindexRun{ID: run.ID, State: run.State, Source: run.Source, Principal: run.Principal, Replica: run.Replica,
		AllRepos: run.RepoID == nil, Prune: run.Options.Prune, Verify: run.Options.Verify, StartedAt: run.StartedAt,
		HeartbeatAt: run.HeartbeatAt, FinishedAt: run.FinishedAt, Error: run.Error}
	if run.Report == nil {
		return out, nil
	}
	var rep reindex.Report
	if err := json.Unmarshal(run.Report, &rep); err != nil {
		return nil, fmt.Errorf("api: the report of reindex run %d: %w", run.ID, err)
	}
	for i := range rep.Repos {
		if rep.Repos[i].Repo == repo.Format+"/"+repo.Name {
			out.Report = &rep.Repos[i]
		}
	}
	return out, nil
}
