package api

import (
	"cmp"
	"net/http"
	"slices"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
	"github.com/lajstrom/lajstrom/internal/keys"
)

// RepoStats are the counts of the index of a repo (docs/api.md, Repos): what the derived tables
// hold of it, so that reindex rebuilds them.
type RepoStats struct {
	Packages int64 `json:"packages"`
	// Versions are the distinct versions of its packages; Manifests one per variant of a
	// version.
	Versions  int64 `json:"versions"`
	Manifests int64 `json:"manifests"`
	// Files are their file layers, and Size the sum of their sizes: a blob of two manifests
	// counts twice and the metadata layer not at all, which is not what the registry stores.
	Files int64 `json:"files"`
	Size  int64 `json:"size"`
	// LastPublishedAt is the newest publish time of its manifests; nil without one.
	LastPublishedAt *time.Time `json:"lastPublishedAt"`
}

// SigningKey is a signing key of an apt or rpm repo, its public facts only.
type SigningKey struct {
	Fingerprint string     `json:"fingerprint"` // uppercase hexadecimal
	Algorithm   string     `json:"algorithm"`   // keys.Algorithm
	CreatedAt   time.Time  `json:"createdAt"`
	RetiredAt   *time.Time `json:"retiredAt"` // nil for the active key
}

// signingFormats are the formats whose plugins sign their index documents with the repo's keys:
// apt's InRelease and Release.gpg, rpm's repomd.xml.asc.
var signingFormats = map[string]bool{"apt": true, "rpm": true}

// repoActions are the actions of permissions, in the order it lists them (docs/spec/rbac.md §2).
var repoActions = []auth.Action{auth.ActionRead, auth.ActionWrite, auth.ActionDelete, auth.ActionManage}

// withFacts fills Permissions, Stats and SigningKeys of repos for the request (docs/api.md,
// Repos): the actions allow grants on each, the counts of all of them in one RepoStats, and the
// keys of each apt and rpm repo. A group and an oci repo, which index no versions, get no
// Stats; a group no SigningKeys.
func (h *handler) withFacts(r *http.Request, repos []Repo) error {
	var ids []int64
	for i := range repos {
		perms, err := h.permissions(r, repos[i].Format, repos[i].Name)
		if err != nil {
			return err
		}
		repos[i].Permissions = perms
		if indexed(repos[i]) {
			ids = append(ids, repos[i].id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.repos.Index.RepoStats(r.Context(), ids)
	if err != nil {
		return err
	}
	stats := make(map[int64]index.RepoStats, len(rows))
	for _, s := range rows {
		stats[s.RepoID] = s
	}
	for i := range repos {
		if !indexed(repos[i]) {
			continue
		}
		s := stats[repos[i].id]
		repos[i].Stats = &RepoStats{Packages: s.Packages, Versions: s.Versions, Manifests: s.Manifests, Files: s.Files,
			Size: s.Size, LastPublishedAt: utc(s.LastPublishedAt)}
		if signingFormats[repos[i].Format] {
			ks, err := h.signingKeys(r, repos[i].id)
			if err != nil {
				return err
			}
			repos[i].SigningKeys = &ks
		}
	}
	return nil
}

// indexed reports whether the index holds versions of repo: not of a group, nor of an oci repo.
func indexed(repo Repo) bool { return repo.Kind != index.KindGroup && repo.Format != formatOCI }

// permissions returns the actions of repoActions that allow grants the request on the repo
// formatName/name, in that order. A denial leaves the action out; an Authorizer that cannot
// decide is the error.
func (h *handler) permissions(r *http.Request, formatName, name string) ([]auth.Action, error) {
	out := []auth.Action{}
	for _, a := range repoActions {
		switch err := h.allow(r, a, formatName, name); {
		case err == nil:
			out = append(out, a)
		case !denied(err):
			return nil, err
		}
	}
	return out, nil
}

// signingKeys returns the gpg keys of the repo id, the active one first, then the retired ones,
// the most recently retired first.
func (h *handler) signingKeys(r *http.Request, repoID int64) ([]SigningKey, error) {
	rows, err := h.repos.Index.ListKeys(r.Context(), &repoID)
	if err != nil {
		return nil, err
	}
	out := []SigningKey{}
	for _, k := range rows {
		if k.Kind != keys.KindGPG {
			continue
		}
		algorithm, err := keys.Algorithm(k.Public)
		if err != nil {
			return nil, err
		}
		out = append(out, SigningKey{Fingerprint: k.Fingerprint, Algorithm: algorithm, CreatedAt: k.CreatedAt.UTC(),
			RetiredAt: utc(k.RetiredAt)})
	}
	slices.SortStableFunc(out, func(a, b SigningKey) int {
		if a.RetiredAt == nil || b.RetiredAt == nil {
			return cmp.Compare(retired(a), retired(b))
		}
		return b.RetiredAt.Compare(*a.RetiredAt)
	})
	return out, nil
}

// retired is 1 for a retired key and 0 for the active one.
func retired(k SigningKey) int {
	if k.RetiredAt == nil {
		return 0
	}
	return 1
}

// utc returns t in UTC, nil for nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
