package reindex

import "github.com/opencontainers/go-digest"

// maxFindings bounds each list of a RepoReport; the counts stay exact.
const maxFindings = 1000

// Report is what a run found, per repo; it is stored as reindex_runs.report.
type Report struct {
	Repos []RepoReport `json:"repos"`
}

// RepoReport is the part of a Report about one repo.
type RepoReport struct {
	Repo            string `json:"repo"` // <format>/<name>
	OCIRepositories int    `json:"ociRepositories"`
	Manifests       int    `json:"manifests"` // tags read
	Added           int    `json:"added"`
	Updated         int    `json:"updated"` // read again (verify, degraded) and written
	Unchanged       int    `json:"unchanged"`
	Removed         int    `json:"removed"`
	// Pending are the rows whose manifest the registry answered unknown and that stay until
	// a second answer unknownApart later and their grace age (docs/spec/guarantees.md, G-5).
	Pending         int       `json:"pending"`
	Degraded        int       `json:"degraded"`
	Referrers       int       `json:"referrers"`
	Foreign         []Finding `json:"foreign"`
	OrphanReferrers []Finding `json:"orphanReferrers"`
	// OrphanJournal are the npm journal entries of an OCI repository without versions
	// (docs/spec/manifest-profile.md §8): not indexed, adopted by the next append to the package.
	OrphanJournal []Finding `json:"orphanJournal"`
	Drift         []Drift   `json:"drift"`
	Errors        []Finding `json:"errors"`
	Truncated     bool      `json:"truncated"` // a list stopped at maxFindings entries
}

// Finding is a manifest, or a failure, that a RepoReport lists.
type Finding struct {
	Repository   string        `json:"repository"` // OCI repository below the repo's base
	Tag          string        `json:"tag,omitempty"`
	Digest       digest.Digest `json:"digest,omitempty"`
	MediaType    string        `json:"mediaType,omitempty"`
	ArtifactType string        `json:"artifactType,omitempty"`
	Subject      digest.Digest `json:"subject,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// Drift is a tag that names another digest in the registry than in the index; the index keeps
// its digest.
type Drift struct {
	Package  string        `json:"package"`
	Tag      string        `json:"tag"`
	Index    digest.Digest `json:"index"`
	Registry digest.Digest `json:"registry"`
}

// add appends v to *list unless the list is full, and then marks the report truncated.
func add[T any](r *RepoReport, list *[]T, v T) {
	if len(*list) >= maxFindings {
		r.Truncated = true
		return
	}
	*list = append(*list, v)
}

// newRepoReport returns the empty report of a repo, with empty lists rather than nil ones, so
// the JSON has [] for them.
func newRepoReport(repo string) RepoReport {
	return RepoReport{Repo: repo, Foreign: []Finding{}, OrphanReferrers: []Finding{}, OrphanJournal: []Finding{},
		Drift: []Drift{}, Errors: []Finding{}}
}
