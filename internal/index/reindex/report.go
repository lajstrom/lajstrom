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
	// a second answer unknownApart later and their grace age (docs/spec/guarantees.md, INV-5).
	Pending int `json:"pending"`
	// DeletedPending are the tags the registry still lists, of manifests the index does not have
	// and the audit log has as deleted through Lajstrom less than the grace age before the run,
	// on a registry that is not strongly consistent: not indexed again (docs/spec/guarantees.md,
	// INV-13).
	DeletedPending  int       `json:"deletedPending"`
	Degraded        int       `json:"degraded"`
	Referrers       int       `json:"referrers"`
	Attachments     int       `json:"attachments"` // attachment rows of the versions seen or written (#89; index-schema.md, Attachments)
	Foreign         []Finding `json:"foreign"`
	OrphanReferrers []Finding `json:"orphanReferrers"`
	// OrphanJournal are the npm journal entries of an OCI repository without versions
	// (docs/spec/manifest-profile.md §8): not indexed, adopted by the next append to the package.
	OrphanJournal []Finding `json:"orphanJournal"`
	Drift         []Drift   `json:"drift"`
	// Split are the packages found at two forms of their OCI name (ADR 0017), one entry per
	// other form: the versions there are not indexed (#609).
	Split []Split `json:"split"`
	// RepositoryUnknown are the versions of the index whose OCI repository the registry does
	// not know: prune keeps their rows, and a delete through the API removes them (#567).
	RepositoryUnknown []RepositoryUnknown `json:"repositoryUnknown"`
	// RepairedReferrers are the sha256-<hex> indexes that lacked referrers the audit log has as
	// pushed and the registry still has, or were missing, and that ingest pushed again with them
	// (#628): runs of the reconcile loop and with Verify, on a registry without the referrers API.
	RepairedReferrers []RepairedIndex `json:"repairedReferrers"`
	Errors            []Finding       `json:"errors"`
	Truncated         bool            `json:"truncated"` // a list stopped at maxFindings entries
}

// RepairedIndex is a subject's sha256-<hex> index that ingest pushed again for a run, with the
// referrers it lacked (ingest.Pipeline.RepairReferrers).
type RepairedIndex struct {
	Repository string          `json:"repository"` // the OCI repository, below the repo's base
	Subject    digest.Digest   `json:"subject"`
	Index      digest.Digest   `json:"index"` // the index pushed
	Added      []digest.Digest `json:"added"` // the referrers it lacked, in the order of their pushes
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

// Split is a package whose manifests are at two forms of its OCI name (ADR 0017): the index has
// the package, and its versions, at Indexed; the manifests at Other are left out, since their
// rows would send each download to Indexed, which does not hold them. A run copies or moves
// nothing in the registry.
type Split struct {
	Package     string   `json:"package"`
	Indexed     string   `json:"indexed"`     // the package's name_oci, below the repo's base
	IndexedTags []string `json:"indexedTags"` // the tags of its versions in the index
	Other       string   `json:"other"`       // the other form, below the repo's base
	OtherTags   []string `json:"otherTags"`   // the tags of the profile manifests there, not indexed
}

// RepositoryUnknown is a version of the index whose OCI repository the registry does not know:
// its tag does not resolve, the manifest by its digest is a 404 without MANIFEST_UNKNOWN (Zot's
// NAME_UNKNOWN), and the repository's tag list is a 404 too, as after a delete of the repository
// in the registry's console. Prune counts only MANIFEST_UNKNOWN (docs/spec/guarantees.md,
// INV-5), so the row stays, on every run, until it is deleted through the management API
// (Delete; also on a registry that cannot delete manifests, #692) or the repository comes back. A run deletes nothing in the registry.
type RepositoryUnknown struct {
	Repository string        `json:"repository"` // the OCI repository, below the repo's base
	Package    string        `json:"package"`
	Version    string        `json:"version"`
	Variant    string        `json:"variant,omitempty"`
	Tag        string        `json:"tag"`
	Digest     digest.Digest `json:"digest"`
	// Delete is the request of the management API that deletes the version's rows, the variant
	// alone when it has one: DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>
	// (docs/api.md), with the package and the version path-escaped.
	Delete string `json:"delete"`
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
		Drift: []Drift{}, Split: []Split{}, RepositoryUnknown: []RepositoryUnknown{}, RepairedReferrers: []RepairedIndex{},
		Errors: []Finding{}}
}
