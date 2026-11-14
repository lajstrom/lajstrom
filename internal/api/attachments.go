package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/lajstrom/lajstrom/internal/auth"
	"github.com/lajstrom/lajstrom/internal/index"
)

// AttachmentList is the answer of GET …/packages/{name}/versions/{version}/attachments (docs/api.md,
// Attachments of a version; #89).
type AttachmentList struct {
	Attachments []AttachmentEntry `json:"attachments"`
	// Truncated says that there are more rows than the answer has: the oldest are left out.
	Truncated bool `json:"truncated"`
}

// AttachmentEntry is one attachment of a version: one layer of a referrer of one of its
// variants, with its label (docs/spec/manifest-profile.md §6).
type AttachmentEntry struct {
	Variant       string            `json:"variant"`
	Subject       string            `json:"subject"` // the variant's manifest digest
	Digest        string            `json:"digest"`  // the referrer's
	Tag           string            `json:"tag,omitempty"`
	Layer         string            `json:"layer"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Kind          string            `json:"kind"`
	PredicateType string            `json:"predicateType"`
	Size          int64             `json:"size"`
	Annotations   map[string]string `json:"annotations"`
	Tool          string            `json:"tool,omitempty"`
	ToolVersion   string            `json:"toolVersion,omitempty"`
	Database      string            `json:"database,omitempty"`
	Input         string            `json:"input,omitempty"`
	Verification  string            `json:"verification"`
	SignedBy      string            `json:"signedBy,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	CreatedAt     *time.Time        `json:"createdAt,omitempty"`
	IndexedAt     time.Time         `json:"indexedAt"`
}

// attachmentKinds are the values ?kind= takes.
var attachmentKinds = []string{index.AttachmentSignature, index.AttachmentSBOM, index.AttachmentScan, index.AttachmentProvenance,
	index.AttachmentAttestation, index.AttachmentOther}

// attachmentFilter is what the query of a list of attachments keeps.
type attachmentFilter struct {
	kind, predicateType string
	verified            *bool
	limit               int
}

// attachmentFilterOf reads ?kind, ?predicateType, ?verified and ?limit; a kind, a verified or a
// limit that is none of its values is 400.
func attachmentFilterOf(r *http.Request) (attachmentFilter, error) {
	q := r.URL.Query()
	f := attachmentFilter{kind: q.Get("kind"), predicateType: q.Get("predicateType"), limit: maxLimit}
	if f.kind != "" && !slices.Contains(attachmentKinds, f.kind) {
		return f, invalid("kind: %q is not one of %v", f.kind, attachmentKinds)
	}
	if v := q.Get("verified"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return f, invalid("verified: %q is not true or false", v)
		}
		f.verified = &b
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return f, invalid("limit: %q is not a number from 1 to %d", v, maxLimit)
		}
		f.limit = n
	}
	return f, nil
}

// keeps reports whether the filter keeps a.
func (f attachmentFilter) keeps(a index.Attachment) bool {
	return (f.kind == "" || a.Kind == f.kind) && (f.predicateType == "" || a.PredicateType == f.predicateType) &&
		(f.verified == nil || *f.verified == (a.Verification == index.AttachmentVerified))
}

// listAttachments answers GET /api/v1/repos/{format}/{repo}/packages/{name}/versions/{version}/attachments
// (docs/spec/http-routing.md §4, #89): read on the repo, checked first; then the attachments of
// the version's variants, or of the one ?variant= names, from the index, newest first, every
// result kept, those the filter keeps, at most ?limit. A repo, package, version or variant that
// does not exist is 404, as for the version.
func (h *handler) listAttachments(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	f, name := r.PathValue("format"), r.PathValue("repo")
	if err := h.allow(r, auth.ActionRead, f, name); err != nil {
		return 0, nil, err
	}
	filter, err := attachmentFilterOf(r)
	if err != nil {
		return 0, nil, err
	}
	var variant *string
	if v, ok := r.URL.Query()["variant"]; ok {
		variant = &v[0]
	}
	ctx := r.Context()
	repo, err := h.repos.Index.GetRepo(ctx, f, name)
	if err != nil {
		return 0, nil, repoError("read the attachments of a version of", f, name, err)
	}
	pkg, rows, err := h.versionOf(ctx, repo, r.PathValue("name"), r.PathValue("version"), variant)
	if err != nil {
		return 0, nil, err
	}
	byID := map[int64]index.Version{}
	ids := make([]int64, 0, len(rows))
	for _, v := range rows {
		byID[v.ID] = v
		ids = append(ids, v.ID)
	}
	all, err := h.repos.Index.ListAttachments(ctx, ids)
	if err != nil {
		return 0, nil, fmt.Errorf("api: attachments of %q %s in %s/%s: %w", pkg.Name, r.PathValue("version"), repo.Format, repo.Name, err)
	}
	out := AttachmentList{Attachments: []AttachmentEntry{}}
	for _, a := range all {
		if !filter.keeps(a) {
			continue
		}
		if len(out.Attachments) == filter.limit {
			out.Truncated = true
			break
		}
		out.Attachments = append(out.Attachments, attachmentEntry(byID[a.VersionID], a))
	}
	return http.StatusOK, out, nil
}

// attachmentEntry is the entry of the attachment a of the variant v.
func attachmentEntry(v index.Version, a index.Attachment) AttachmentEntry {
	e := AttachmentEntry{Variant: v.Variant, Subject: v.Digest.String(), Digest: a.Digest.String(), Tag: a.Tag,
		Layer: a.Layer.String(), MediaType: a.MediaType, ArtifactType: a.ArtifactType, Kind: a.Kind, PredicateType: a.PredicateType,
		Size: a.Size, Annotations: orEmpty(a.Annotations), Tool: a.Tool, ToolVersion: a.ToolVersion, Database: a.Database,
		Input: a.Input.String(), Verification: a.Verification, SignedBy: a.SignedBy, Reason: a.Reason, IndexedAt: a.IndexedAt.UTC()}
	if a.CreatedAt != nil {
		t := a.CreatedAt.UTC()
		e.CreatedAt = &t
	}
	return e
}
