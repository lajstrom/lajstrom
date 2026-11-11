package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/format"
	"github.com/lajstrom/lajstrom/internal/index"
)

// BatchDeleteRequest is the body of POST …/versions:delete (docs/api.md, Delete versions in a
// batch).
type BatchDeleteRequest struct {
	Items  []BatchDeleteItem `json:"items"`
	Cutoff string            `json:"cutoff"`
	DryRun bool              `json:"dryRun"`
	Job    string            `json:"job"`
}

// BatchDeleteItem is one version of a batch: its package, its version string and the digests
// of its variants as the client read them.
type BatchDeleteItem struct {
	Package string   `json:"package"`
	Version string   `json:"version"`
	Digests []string `json:"digests"`
}

// BatchDeleteResult is the answer of the batch: its job and a result per item, in order.
type BatchDeleteResult struct {
	Job     string            `json:"job"`
	DryRun  bool              `json:"dryRun"`
	Results []BatchItemResult `json:"results"`
}

// BatchItemResult is what became of an item.
type BatchItemResult struct {
	Package      string           `json:"package"`
	Version      string           `json:"version"`
	Status       string           `json:"status"`
	Message      string           `json:"message,omitempty"`
	Digests      []string         `json:"digests,omitempty"`
	LastModified *time.Time       `json:"lastModified,omitempty"`
	Pointers     []format.Pointer `json:"pointers,omitempty"`
}

// The statuses of an item that are not refusals (format.Refusal).
const (
	statusDeleted = "deleted"
	statusGone    = "gone"
	statusError   = "error"
)

// maxBatchItems bounds the items of a batch; the body's 64 KiB bound most batches first.
const maxBatchItems = 1000

// jobPattern is a job id a client may choose.
var jobPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// deleteVersions answers POST /api/v1/repos/{format}/{repo}/versions:delete: each item is
// deleted as the conditional DELETE of one version is (deleteVersion), with its digests as
// If-Match and the batch's cutoff as If-Unmodified-Since, never forced, each variant under its
// own locks; the batch is not all-or-nothing. delete on the repo is checked before the body is
// read. 200 with a result per item.
func (h *handler) deleteVersions(w http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionDelete, f, name); err != nil {
		return 0, nil, err
	}
	if h.deleter == nil {
		return 0, nil, &clientError{msg: "deleting versions is not enabled in this server", err: format.ErrUnsupported}
	}
	var req BatchDeleteRequest
	if err := decodeBody(w, r, &req); err != nil {
		return 0, nil, err
	}
	items, cutoff, err := req.check()
	if err != nil {
		return 0, nil, err
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("delete versions in", f, name, err)
	}
	if req.Job == "" {
		req.Job = index.NewUID(h.repos.now())
	}
	c := conditions{since: cutoff}
	res := BatchDeleteResult{Job: req.Job, DryRun: req.DryRun, Results: make([]BatchItemResult, len(items))}
	for i, it := range items {
		c.digests = it.digests
		res.Results[i] = h.deleteItem(r, repo, req.Items[i], c, req.DryRun, req.Job)
	}
	return http.StatusOK, res, nil
}

// batchItem is an item with its digests parsed.
type batchItem struct{ digests []digest.Digest }

// check returns the items of the request with their digests and the cut-off (zero: none), or
// the error of the first rule broken, naming the field.
func (req *BatchDeleteRequest) check() ([]batchItem, time.Time, error) {
	var cutoff time.Time
	switch {
	case len(req.Items) == 0:
		return nil, cutoff, invalid("items: at least one version")
	case len(req.Items) > maxBatchItems:
		return nil, cutoff, invalid("items: at most %d versions, not %d", maxBatchItems, len(req.Items))
	case req.Job != "" && !jobPattern.MatchString(req.Job):
		return nil, cutoff, invalid("job: %q is not 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or a digit", req.Job)
	}
	if req.Cutoff != "" {
		var err error
		if cutoff, err = time.Parse(time.RFC3339, req.Cutoff); err != nil {
			return nil, cutoff, invalid("cutoff: %q is not an RFC 3339 time, like 2026-11-01T00:00:00Z", req.Cutoff)
		}
	}
	items := make([]batchItem, len(req.Items))
	for i, it := range req.Items {
		switch {
		case it.Package == "":
			return nil, cutoff, invalid("items[%d].package: required", i)
		case it.Version == "":
			return nil, cutoff, invalid("items[%d].version: required", i)
		case len(it.Digests) == 0:
			return nil, cutoff, invalid("items[%d].digests: the digest of every variant, at least one", i)
		}
		for j, s := range it.Digests {
			d, err := digest.Parse(s)
			if err != nil {
				return nil, cutoff, invalid("items[%d].digests[%d]: %q is not a digest", i, j, s)
			}
			items[i].digests = append(items[i].digests, d)
		}
	}
	return items, cutoff, nil
}

// deleteItem deletes one item of a batch, or in a dry run checks it, and reports what became of
// it. Refusals and failures are the item's, never the batch's.
func (h *handler) deleteItem(r *http.Request, repo *index.Repo, it BatchDeleteItem, c conditions, dryRun bool, job string) BatchItemResult {
	res := BatchItemResult{Package: it.Package, Version: it.Version, Status: statusDeleted}
	ctx := r.Context()
	pkg, versions, err := h.versionOf(ctx, repo, it.Package, it.Version, nil)
	if err == nil {
		err = c.check(ctx, h.repos.Index, repo, pkg, versions)
	}
	if err == nil && !dryRun {
		err = h.deleteEach(r, repo, pkg, versions, c, job)
	}
	var refused *format.RefusedError
	switch {
	case err == nil:
		return res
	case errors.As(err, &refused):
		res.Status = string(refused.Reason)
		switch refused.Reason {
		case format.RefusedChanged:
			for _, d := range refused.Digests {
				res.Digests = append(res.Digests, d.String())
			}
		case format.RefusedYoung:
			at := refused.PublishedAt.UTC()
			res.LastModified = &at
		case format.RefusedProtected:
			res.Pointers = refused.Pointers
		}
	case errors.Is(err, index.ErrNotFound):
		res.Status = statusGone
	default:
		res.Status = statusError
	}
	res.Message = clientMessage(err)
	if res.Status == statusError {
		h.log.ErrorContext(ctx, "api: an item of a batch delete failed", "repo", repo.Format+"/"+repo.Name, "package", it.Package,
			"version", it.Version, "job", job, "error", err.Error())
	}
	return res
}

// clientMessage is the message the API answers err with (fail): its own for a 4xx and a
// *clientError, for any other 5xx the status text and what a *format.PartialError deleted
// (format.ClientMessage).
func clientMessage(err error) string {
	var ce *clientError
	if errors.As(err, &ce) {
		return strings.TrimSpace(err.Error())
	}
	return strings.TrimSpace(format.ClientMessage(err))
}
