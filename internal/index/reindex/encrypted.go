package reindex

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lajstrom/lajstrom/internal/index"
)

// blobKeep is how long a run keeps a row of blob_map of kind blob that no manifest names: the
// longest a publication session lives (maven.sessionTimeout, at most 24 hours), longer than an
// upload session of the facade (docs/spec/index-schema.md, The mapping of an encrypted repo).
const blobKeep = 24 * time.Hour

// keysClosed returns why a run skips repo whole, "" when it reads it: a repo whose policy has
// encryption (#114), in either mode, may hold encrypted layers, and its rows are left as they are
// when the run has no master key or the repo's keys do not open.
func (w *Worker) keysClosed(ctx context.Context, repo index.Repo) string {
	if !hasEncryption(repo) {
		return ""
	}
	if w.keys == nil {
		return "the repo's policy has encryption, and this server has no master key: its encrypted layers do not open"
	}
	if err := w.keys.CanOpen(ctx, repo); err != nil {
		return "the repo's keys do not open: " + err.Error()
	}
	return ""
}

// skipped reports whether the scan skips its repo whole, an encrypted repo whose keys do not open,
// and says why in the report.
func (s *scan) skipped(ctx context.Context) bool {
	why := s.w.keysClosed(ctx, s.repo)
	if why == "" {
		return false
	}
	s.rep.Skipped = why
	s.w.log.WarnContext(ctx, "reindex: an encrypted repo skipped: its keys do not open", "repo", s.rep.Repo, "reason", why)
	return true
}

// hasEncryption reports whether the policy of repo has encryption, in either mode.
func hasEncryption(repo index.Repo) bool {
	if len(repo.Policy) == 0 || repo.Format == ociFormat || repo.Kind == index.KindGroup {
		return false
	}
	var p struct {
		Encryption json.RawMessage `json:"encryption"`
	}
	return json.Unmarshal(repo.Policy, &p) == nil && len(p.Encryption) > 0 && string(p.Encryption) != "null"
}

// sweepBlobMap removes, after a sweep, the rows of blob_map of the repo that no version, file or
// attachment names and the run did not write, but the blobs of the last day: a blob uploaded and
// not yet named by a manifest.
func (s *scan) sweepBlobMap(ctx context.Context) {
	if !hasEncryption(s.repo) {
		return
	}
	_, err := s.w.index.SweepBlobMap(ctx, s.repo.ID, "", s.start, s.start.Add(-blobKeep))
	s.dbErr(err)
}
